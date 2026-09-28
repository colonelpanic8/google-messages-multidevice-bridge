package whatsapp

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
)

type AppStateClient interface {
	FetchAppState(context.Context, appstate.WAPatchName, bool, bool) error
}

type AppStateSync struct {
	Provider *Provider
	Client   AppStateClient
	mu       sync.Mutex
	done     map[appstate.WAPatchName]bool
	retry    map[appstate.WAPatchName]uint64
}

func (s *AppStateSync) Retry(ctx context.Context, name appstate.WAPatchName) error {
	if err := s.Provider.Keys.put(ctx, "appsync", string(name), false); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.retry == nil {
		s.retry = map[appstate.WAPatchName]uint64{}
	}
	s.retry[name]++
	delete(s.done, name)
	return nil
}

// Sync retries incomplete collections and performs one delta fetch per connection.
func (s *AppStateSync) Sync(ctx context.Context) error {
	keys, err := s.Provider.Keys.GetLatestAppStateSyncKeyID(ctx)
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		s.Provider.diagnostic("appstate_waiting_keys", 0, "missing_keys")
		return appstate.ErrKeyNotFound
	}
	var failures []error
	for _, name := range appstate.AllPatchNames {
		s.mu.Lock()
		done, generation := s.done[name], s.retry[name]
		s.mu.Unlock()
		if done {
			continue
		}
		var complete bool
		if _, err = s.Provider.Keys.get(ctx, "appsync", string(name), &complete); err != nil {
			return err
		}
		if err = s.Provider.Keys.put(ctx, "appsync", string(name), false); err != nil {
			return err
		}
		err = s.Client.FetchAppState(ctx, name, !complete, false)
		if err != nil {
			s.Provider.diagnostic("appstate_fetch_"+string(name), 0, ErrorClass(err))
			failures = append(failures, err)
			continue
		}
		if err = s.Provider.Keys.put(ctx, "appsync", string(name), true); err != nil {
			return err
		}
		s.mu.Lock()
		if s.done == nil {
			s.done = map[appstate.WAPatchName]bool{}
		}
		s.done[name] = s.retry[name] == generation
		s.mu.Unlock()
		s.Provider.diagnostic("appstate_fetch_"+string(name), 1, "")
	}
	return errors.Join(failures...)
}

func ErrorClass(err error) string {
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, appstate.ErrKeyNotFound):
		return "missing_keys"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, whatsmeow.ErrNotConnected), errors.Is(err, whatsmeow.ErrNotLoggedIn):
		return "disconnected"
	}
	var storage *storageError
	if errors.As(err, &storage) {
		return "storage"
	}
	return "provider"
}

func (p *Provider) Diagnose(ctx context.Context) error {
	for _, kind := range []string{"contact", "appkey", "appversion", "inbox", "quarantine"} {
		rows, err := p.Keys.scan(ctx, kind)
		if err != nil {
			return err
		}
		p.diagnostic("stored_"+kind, len(rows), "")
		if kind == "inbox" || kind == "quarantine" {
			counts := map[string]int{}
			for _, raw := range rows {
				var header struct{ Kind string }
				if json.Unmarshal(raw, &header) == nil {
					switch header.Kind {
					case "message", "history", "history-chat", "receipt":
						counts[header.Kind]++
					}
				}
			}
			for _, name := range []string{"message", "history", "history-chat", "receipt"} {
				if counts[name] > 0 {
					p.diagnostic("stored_"+kind+"_"+name, counts[name], "")
				}
			}
		}
	}
	return nil
}
