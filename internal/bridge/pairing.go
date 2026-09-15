package bridge

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var ErrPairing = errors.New("pairing in progress")
var ErrPairingTicket = errors.New("invalid or expired pairing ticket")

type PairingState struct {
	generation            uint64
	State                 string    `json:"state"`
	Reason                string    `json:"reason,omitempty"`
	CanRepair             bool      `json:"can_repair"`
	AgentEnrolled         bool      `json:"agent_enrolled"`
	Required              bool      `json:"required"`
	RequiredReason        string    `json:"required_reason,omitempty"`
	Ticket                string    `json:"ticket,omitempty"`
	Emoji                 string    `json:"emoji,omitempty"`
	Detail                string    `json:"detail,omitempty"`
	Expires               time.Time `json:"expires"`
	SessionEpoch          uint64    `json:"session_epoch"`
	PreviousConversations int       `json:"previous_session_conversations"`
	PreviousMessages      int       `json:"previous_session_messages"`
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
	b.mu.Lock()
	var cancel context.CancelFunc
	var clearErr error
	if activePair(b.pairingState.State) && !time.Now().Before(b.pairingState.Expires) {
		cancel = b.pairCancel
		if b.pairDone == nil {
			b.pairingState.State = "failed"
			b.pairingState.Reason = "ticket_expired"
			b.pairingState.Detail = "Pairing expired; start again"
			b.pairingState.Ticket = ""
			b.pairingState.Emoji = ""
			b.pairCookies = nil
			clearErr = b.Store.ClearPairingAttempt()
		}
	}
	state := b.pairingState
	if !activePair(state.State) && b.status.State == "authentication_required" {
		state.Required = true
		state.RequiredReason = b.status.Reason
		if state.Reason == "" || state.State == "paired" {
			state.Reason = b.status.Reason
		}
	}
	b.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if clearErr != nil {
		b.storageFailure(clearErr)
	}
	if summary, err := b.Store.SessionSummary(); err == nil {
		state.SessionEpoch = summary.Epoch
		state.PreviousConversations = summary.PreviousConversations
		state.PreviousMessages = summary.PreviousMessages
	}
	cookies, err := b.savedPairingCookies()
	state.CanRepair = err == nil && len(cookies) > 0
	digest, err := b.Store.PairingAgentDigest()
	state.AgentEnrolled = err == nil && len(digest) == 32
	return state
}
func (b *Bridge) SetOfflineOnly(offline bool) {
	b.mu.Lock()
	b.offlineOnly = offline
	b.mu.Unlock()
}
func (b *Bridge) savedPairingCookies() (map[string]string, error) {
	data, err := b.Store.Session()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStorage, err)
	}
	var auth struct {
		Cookies map[string]string `json:"cookies"`
	}
	if len(data) == 0 || json.Unmarshal(data, &auth) != nil {
		return nil, ErrInvalid
	}
	clean := make(map[string]string)
	for name, required := range cookieNames {
		value := auth.Cookies[name]
		if (required && value == "") || len(value) > 8192 {
			return nil, ErrInvalid
		}
		if value != "" {
			clean[name] = value
		}
	}
	return clean, nil
}

func (b *Bridge) BeginPairing() (PairingState, error) {
	return b.beginPairing(false)
}

func (b *Bridge) BeginRepairing() (PairingState, error) {
	return b.beginPairing(true)
}

func (b *Bridge) beginPairing(reuseSignIn bool) (PairingState, error) {
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
	if activePair(b.pairingState.State) && (!reuseSignIn || b.pairingState.State != "waiting_for_login") {
		state := b.pairingState
		b.mu.Unlock()
		return state, nil
	}
	var cookies map[string]string
	if reuseSignIn {
		var err error
		cookies, err = b.savedPairingCookies()
		if err != nil {
			b.mu.Unlock()
			return PairingState{}, err
		}
	}
	ticket := make([]byte, 32)
	if _, err := rand.Read(ticket); err != nil {
		b.mu.Unlock()
		return PairingState{}, err
	}
	now := time.Now().UTC()
	state := PairingState{generation: b.pairingState.generation + 1, State: "waiting_for_login", Ticket: base64.RawURLEncoding.EncodeToString(ticket), Expires: now.Add(10 * time.Minute), Detail: "Sign in to Google using the pairing helper, then return here"}
	if err := b.Store.BeginPairingAttempt(now, state.Expires); err != nil {
		b.mu.Unlock()
		b.storageFailure(err)
		return PairingState{}, err
	}
	if reuseSignIn {
		state.State = "connecting"
		state.Ticket = ""
		state.Detail = "Reconnecting with your saved Google sign-in; keep your phone nearby"
		state.CanRepair = true
		b.pairCookies = cookies
	}
	b.pairingState = state
	b.mu.Unlock()
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
	b.mutationMu.Lock()
	defer b.mutationMu.Unlock()
	state, _, changed := b.cancelPairing()
	if changed {
		b.clearPairingAttempt()
	}
	return state
}

func (b *Bridge) CancelPairingAndWait(ctx context.Context) (PairingState, error) {
	b.mutationMu.Lock()
	defer b.mutationMu.Unlock()
	state, done, changed := b.cancelPairing()
	if changed {
		b.clearPairingAttempt()
	}
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return state, ctx.Err()
		}
	}
	return state, nil
}

func (b *Bridge) cancelPairing() (PairingState, <-chan struct{}, bool) {
	b.mu.Lock()
	var cancel context.CancelFunc
	var done <-chan struct{}
	changed := false
	if activePair(b.pairingState.State) {
		changed = true
		cancel = b.pairCancel
		done = b.pairDone
		b.pairingState.State = "canceled"
		b.pairingState.Reason = "canceled"
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
	return state, done, changed
}
func (b *Bridge) finishPairing(state, detail string, generation uint64) {
	b.finishPairingReason(state, "", detail, generation)
}

func (b *Bridge) finishPairingReason(state, reason, detail string, generation uint64) {
	b.mu.Lock()
	if b.pairingState.generation != generation || !activePair(b.pairingState.State) {
		b.mu.Unlock()
		return
	}
	b.pairingState.State = state
	b.pairingState.Reason = reason
	if state == "failed" && reason == "" {
		b.pairingState.Reason = "pairing_failed"
	}
	b.pairingState.Detail = detail
	b.pairingState.Ticket = ""
	b.pairingState.Emoji = ""
	b.pairCookies = nil
	clearErr := b.Store.ClearPairingAttempt()
	b.mu.Unlock()
	if clearErr != nil {
		b.storageFailure(clearErr)
	}
}

func (b *Bridge) clearPairingAttempt() {
	if err := b.Store.ClearPairingAttempt(); err != nil {
		b.storageFailure(err)
	}
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
		done := make(chan struct{})
		b.pairDone = done
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
		if b.pairDone == done {
			b.pairCancel = nil
			b.pairDone = nil
		}
		b.mu.Unlock()
		if errors.Is(err, ErrStorage) {
			b.finishPairing("failed", "Could not save pairing", state.generation)
			close(done)
			return err
		}
		if !time.Now().Before(state.Expires) {
			b.finishPairingReason("failed", "ticket_expired", "Pairing expired; start again", state.generation)
		} else if err != nil || canceled {
			b.finishPairing("failed", "Pairing did not complete. If you confirmed on your phone, try again; otherwise use browser sign-in to refresh your Google account", state.generation)
		} else {
			b.finishPairing("paired", "Phone paired; connecting and loading history", state.generation)
		}
		close(done)
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
		b.pairingState.Reason = ""
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
