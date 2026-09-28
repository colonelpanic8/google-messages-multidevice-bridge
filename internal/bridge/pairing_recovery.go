package bridge

import (
	"context"
	"fmt"
	"time"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/store"
)

const recoveryAttempts = 3
const recoveryCooldown = time.Minute
const whatsappPairingLifetime = 3 * time.Minute

// ConfigurePairingRecovery keeps budgets across repeated configuration requests.
// Resume is an explicit request for a new budget after cancellation or exhaustion.
func (b *Bridge) ConfigurePairingRecovery(enabled bool, phone string, resume bool) error {
	if b.Network() != "whatsapp" || (enabled && !phoneNumber.MatchString(phone)) {
		return ErrInvalid
	}
	b.mutationMu.Lock()
	defer b.mutationMu.Unlock()
	b.mu.RLock()
	busy := activePair(b.pairingState.State) || b.pairCancel != nil
	b.mu.RUnlock()
	if enabled && busy {
		return ErrPairing
	}
	err := b.Store.UpdatePairingRecovery(func(state *store.PairingRecovery) {
		if enabled && (resume || !state.Enabled || phone != state.PhoneNumber) {
			*state = store.PairingRecovery{}
		}
		state.Enabled = enabled
		state.PhoneNumber = phone
		if !enabled {
			state.PhoneNumber = ""
		}
	})
	if err != nil {
		return b.recoveryStorageError(err)
	}
	b.mu.RLock()
	automatic := b.pairingState.Automatic
	b.mu.RUnlock()
	if !enabled && automatic {
		_, _, changed := b.cancelPairing()
		if changed {
			b.clearPairingAttempt()
		}
	}
	// Wake an authentication wait without disrupting a healthy connection.
	wake(b.reconnectWake)
	b.Hub.Notify()
	return nil
}

func (b *Bridge) recoveryStorageError(err error) error {
	b.storageFailure(err)
	return fmt.Errorf("%w: %v", ErrStorage, err)
}

func recoveryEligible(status Status) bool {
	return status.State == "authentication_required" && (status.Reason == "no_session" || status.Reason == "session_expired")
}

// recoverPairing reserves an attempt durably before starting any network work.
// A crash may consume an attempt, but cannot replenish the retry budget.
func (b *Bridge) recoverPairing(now time.Time) (bool, time.Duration, error) {
	if b.Network() != "whatsapp" {
		return false, 0, nil
	}
	b.mutationMu.Lock()
	defer b.mutationMu.Unlock()
	b.mu.RLock()
	eligible := !b.offlineOnly && recoveryEligible(b.status)
	busy := activePair(b.pairingState.State) || b.pairCancel != nil
	b.mu.RUnlock()
	if !eligible || busy {
		return false, 0, nil
	}
	var phone string
	var delay time.Duration
	err := b.Store.UpdatePairingRecovery(func(state *store.PairingRecovery) {
		if !state.Enabled || state.Paused || state.Attempts >= recoveryAttempts {
			return
		}
		if now.Before(state.NextAttempt) {
			delay = state.NextAttempt.Sub(now)
			return
		}
		state.Attempts++
		state.NextAttempt = now.Add(whatsappPairingLifetime + recoveryCooldown)
		phone = state.PhoneNumber
	})
	if err != nil {
		return false, 0, b.recoveryStorageError(err)
	}
	if phone == "" {
		return false, delay, nil
	}
	_, err = b.beginWhatsAppPairing(phone, true)
	return err == nil, 0, err
}

func (b *Bridge) pausePairingRecovery() error {
	if b.Network() != "whatsapp" {
		return nil
	}
	err := b.Store.UpdatePairingRecovery(func(state *store.PairingRecovery) {
		if state.Enabled {
			state.Paused = true
		}
	})
	if err != nil {
		return b.recoveryStorageError(err)
	}
	b.Hub.Notify()
	return nil
}

func (b *Bridge) waitForAuthentication(ctx context.Context) error {
	for {
		started, delay, err := b.recoverPairing(time.Now().UTC())
		if err != nil || started {
			return err
		}
		var retry <-chan time.Time
		var timer *time.Timer
		if delay > 0 {
			timer = time.NewTimer(delay)
			retry = timer.C
		}
		select {
		case <-ctx.Done():
		case <-b.reconnectWake:
		case <-retry:
			continue
		}
		if timer != nil {
			timer.Stop()
		}
		return nil
	}
}
