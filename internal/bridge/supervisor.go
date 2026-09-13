package bridge

import (
	"context"
	"errors"
	"time"
)

func (b *Bridge) RequestReconnect() {
	b.mu.Lock()
	cancel := b.connectionCancel
	b.mu.Unlock()
	wake(b.reconnectWake)
	if cancel != nil {
		cancel()
	}
}

// ServeConnection retains the local service while transient provider failures
// reconnect. Authentication failures wait for an explicit restart after pairing.
func (b *Bridge) ServeConnection(ctx context.Context, offline bool) error {
	return b.supervise(ctx, offline, func(ctx context.Context) error { return b.Run(ctx, offline, nil, nil) })
}
func (b *Bridge) supervise(ctx context.Context, offline bool, run func(context.Context) error) error {
	delay := b.reconnectDelay
	for {
		if ctx.Err() != nil {
			return nil
		}
		b.mu.RLock()
		storageErr := b.storageErr
		b.mu.RUnlock()
		if storageErr != nil {
			return storageErr
		}
		if err := b.processPairing(ctx); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
		b.eventMu.Lock()
		b.stopped = false
		b.eventMu.Unlock()
		select {
		case <-b.fatal:
		default:
		}
		started := time.Now()
		callCtx, cancel := context.WithCancel(ctx)
		b.mu.Lock()
		if activePair(b.pairingState.State) {
			b.mu.Unlock()
			cancel()
			continue
		}
		b.connectionCancel = cancel
		b.mu.Unlock()
		err := run(callCtx)
		cancel()
		b.mu.Lock()
		b.connectionCancel = nil
		b.mu.Unlock()
		if errors.Is(err, ErrStorage) || offline {
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
		if b.Status().State == "authentication_required" {
			select {
			case <-ctx.Done():
				return nil
			case <-b.reconnectWake:
			}
		} else {
			if time.Since(started) > time.Minute {
				delay = b.reconnectDelay
			}
			b.setStatus("connection_failed", "Google connection stopped; reconnecting automatically")
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil
			case <-b.reconnectWake:
				timer.Stop()
			case <-timer.C:
			}
			if delay < 5*time.Minute {
				delay *= 2
				if delay > 5*time.Minute {
					delay = 5 * time.Minute
				}
			}
		}
	}
}
