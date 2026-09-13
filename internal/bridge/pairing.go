package bridge

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"time"
)

var ErrPairing = errors.New("pairing in progress")
var ErrPairingTicket = errors.New("invalid or expired pairing ticket")

type PairingState struct {
	generation uint64
	State      string    `json:"state"`
	Ticket     string    `json:"ticket,omitempty"`
	Emoji      string    `json:"emoji,omitempty"`
	Detail     string    `json:"detail,omitempty"`
	Expires    time.Time `json:"expires"`
}

func activePair(state string) bool {
	return state == "waiting_for_login" || state == "connecting" || state == "confirm_on_phone"
}
func (b *Bridge) PairingActive() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return activePair(b.pairingState.State)
}
func (b *Bridge) PairingStatus() PairingState {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.pairingState
}
func (b *Bridge) SetOfflineOnly(offline bool) {
	b.mu.Lock()
	b.offlineOnly = offline
	b.mu.Unlock()
}
func (b *Bridge) BeginPairing() (PairingState, error) {
	b.mutationMu.Lock()
	defer b.mutationMu.Unlock()
	b.mu.Lock()
	if b.offlineOnly {
		b.mu.Unlock()
		return PairingState{}, ErrInvalid
	}
	if b.pairCancel != nil && !activePair(b.pairingState.State) {
		b.mu.Unlock()
		return PairingState{}, ErrPairing
	}
	if activePair(b.pairingState.State) {
		state := b.pairingState
		b.mu.Unlock()
		return state, nil
	}
	ticket := make([]byte, 32)
	_, _ = rand.Read(ticket)
	b.pairingState = PairingState{generation: b.pairingState.generation + 1, State: "waiting_for_login", Ticket: base64.RawURLEncoding.EncodeToString(ticket), Expires: time.Now().UTC().Add(10 * time.Minute), Detail: "Sign in to Google using the pairing helper, then return here"}
	state := b.pairingState
	b.mu.Unlock()
	if err := b.Store.CancelQueuedForPairing(); err != nil {
		b.finishPairing("failed", "Storage could not prepare pairing", state.generation)
		b.storageFailure(err)
		return PairingState{}, err
	}
	b.Hub.Notify()
	b.RequestReconnect()
	wake(b.pairWake)
	return state, nil
}

var cookieNames = map[string]bool{"SID": true, "HSID": true, "OSID": true, "SSID": true, "APISID": true, "SAPISID": true, "__Secure-1PSIDTS": false}

func (b *Bridge) SubmitPairingCookies(ticket string, cookies map[string]string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pairingState.State != "waiting_for_login" || time.Now().After(b.pairingState.Expires) || len(ticket) != 43 || subtle.ConstantTimeCompare([]byte(ticket), []byte(b.pairingState.Ticket)) != 1 {
		return ErrPairingTicket
	}
	for name, required := range cookieNames {
		if required && cookies[name] == "" {
			return ErrInvalid
		}
	}
	clean := make(map[string]string)
	for name, value := range cookies {
		if _, ok := cookieNames[name]; ok {
			if len(value) > 8192 {
				return ErrInvalid
			}
			clean[name] = value
		}
	}
	b.pairCookies = clean
	b.pairingState.State = "connecting"
	b.pairingState.Ticket = ""
	b.pairingState.Detail = "Contacting Google to pair your phone"
	wake(b.pairWake)
	return nil
}
func (b *Bridge) CancelPairing() PairingState {
	b.mu.Lock()
	var cancel context.CancelFunc
	if activePair(b.pairingState.State) {
		cancel = b.pairCancel
		b.pairingState.State = "canceled"
		b.pairingState.Detail = "Pairing canceled"
		b.pairingState.Ticket = ""
		b.pairingState.Emoji = ""
		b.pairCookies = nil
	}
	state := b.pairingState
	b.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	wake(b.pairWake)
	return state
}
func (b *Bridge) finishPairing(state, detail string, generation uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pairingState.generation != generation || b.pairingState.State == "canceled" || (b.pairingState.State == "paired" && state != "paired") {
		return
	}
	b.pairingState.State = state
	b.pairingState.Detail = detail
	b.pairingState.Ticket = ""
	b.pairingState.Emoji = ""
	b.pairCookies = nil
}

// processPairing is called only after the previous connection has fully joined.
func (b *Bridge) processPairing(ctx context.Context) error {
	for {
		b.mu.RLock()
		storageErr := b.storageErr
		b.mu.RUnlock()
		if storageErr != nil {
			return storageErr
		}
		state := b.PairingStatus()
		if !activePair(state.State) {
			return nil
		}
		if ctx.Err() != nil {
			b.CancelPairing()
			return nil
		}
		if time.Now().After(state.Expires) {
			b.finishPairing("failed", "Pairing expired; start again", state.generation)
			return nil
		}
		if state.State == "waiting_for_login" {
			timer := time.NewTimer(time.Until(state.Expires))
			select {
			case <-ctx.Done():
				timer.Stop()
				b.CancelPairing()
				return nil
			case <-b.pairWake:
				timer.Stop()
			case <-timer.C:
			}
			continue
		}
		select {
		case <-b.fatal:
		default:
		}
		callCtx, cancel := context.WithDeadline(ctx, state.Expires)
		b.mu.Lock()
		if b.pairingState.generation != state.generation || b.pairingState.State != "connecting" {
			b.mu.Unlock()
			cancel()
			continue
		}
		cookies := b.pairCookies
		b.pairCookies = nil
		b.pairCancel = cancel
		attempt := b.pairAttempt
		b.mu.Unlock()
		if attempt == nil {
			attempt = func(ctx context.Context, cookies map[string]string, emoji func(string)) error {
				return b.Run(ctx, false, cookies, emoji)
			}
		}
		b.eventMu.Lock()
		b.stopped = false
		b.eventMu.Unlock()
		err := attempt(callCtx, cookies, func(emoji string) {
			b.mu.Lock()
			defer b.mu.Unlock()
			if b.pairingState.generation == state.generation && b.pairingState.State == "connecting" {
				b.pairingState.State = "confirm_on_phone"
				b.pairingState.Emoji = emoji
				b.pairingState.Detail = "Choose this emoji in Google Messages on your phone"
			}
		})
		canceled := callCtx.Err() != nil
		cancel()
		b.mu.Lock()
		b.pairCancel = nil
		b.mu.Unlock()
		if errors.Is(err, ErrStorage) {
			b.finishPairing("failed", "Could not save pairing", state.generation)
			return err
		}
		if err != nil || canceled {
			b.finishPairing("failed", "Pairing did not complete; check Google sign-in and try again", state.generation)
		} else {
			b.finishPairing("paired", "Phone paired; connecting and loading history", state.generation)
		}
		return nil
	}
}

// The commit and cancellation share a fence: once persisted, the UI must report
// paired even if cancellation arrived while the transaction was committing.
func (b *Bridge) commitPairedSession(ctx context.Context, generation uint64, data []byte) error {
	b.mu.Lock()
	if ctx.Err() != nil || (generation != 0 && (b.pairingState.generation != generation || !activePair(b.pairingState.State))) {
		b.mu.Unlock()
		return context.Canceled
	}
	err := b.Store.SavePairedSession(data)
	if err == nil && generation != 0 {
		b.pairingState.State = "paired"
		b.pairingState.Detail = "Phone paired; connecting and loading history"
		b.pairingState.Ticket, b.pairingState.Emoji = "", ""
		b.pairCookies = nil
	}
	b.mu.Unlock()
	if err != nil {
		b.storageFailure(err)
		return fmt.Errorf("%w: %v", ErrStorage, err)
	}
	return nil
}
