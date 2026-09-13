package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/bridge"
)

// New builds the HTTP surface. push may be nil, which disables notifications
// without removing the routes that report their state.
func New(b *bridge.Bridge, token string, push PushService) http.Handler {
	mux := http.NewServeMux()
	registerRecords(mux, b)
	registerFeatures(mux, b)
	registerPairing(mux, b)
	registerPush(mux, push)
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, b.Status()) })
	mux.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
		after, err := cursor(r)
		if err == nil {
			var mark uint64
			mark, err = b.Store.Watermark()
			if err == nil && after > mark {
				err = fmt.Errorf("cursor ahead of store")
			}
		}
		if err != nil {
			http.Error(w, "invalid cursor", http.StatusBadRequest)
			return
		}
		limit := 100
		if q := r.URL.Query().Get("limit"); q != "" {
			limit, err = strconv.Atoi(q)
		}
		if err != nil || limit < 1 || limit > 1000 {
			http.Error(w, "limit must be 1..1000", http.StatusBadRequest)
			return
		}
		events, err := b.Store.Events(after, limit)
		if err != nil {
			http.Error(w, "storage unavailable", http.StatusServiceUnavailable)
			return
		}
		for i, event := range events {
			events[i], err = publicEvent(event)
			if err != nil {
				http.Error(w, "unsupported stored event", http.StatusServiceUnavailable)
				return
			}
		}
		next := after
		if len(events) > 0 {
			next = events[len(events)-1].ID
		}
		writeJSON(w, map[string]any{"events": events, "next_cursor": next})
	})
	mux.HandleFunc("GET /v1/stream", func(w http.ResponseWriter, r *http.Request) {
		after, err := cursor(r)
		if err == nil {
			var mark uint64
			mark, err = b.Store.Watermark()
			if err == nil && after > mark {
				err = fmt.Errorf("cursor ahead of store")
			}
		}
		if err != nil {
			http.Error(w, "invalid cursor", http.StatusBadRequest)
			return
		}
		sub, unsubscribe := b.Hub.Subscribe()
		defer unsubscribe()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Accel-Buffering", "no")
		rc := http.NewResponseController(w)
		defer rc.SetWriteDeadline(time.Time{})
		flush := func() error { return rc.Flush() }
		deadline := func() { _ = rc.SetWriteDeadline(time.Now().Add(10 * time.Second)) }
		deadline()
		if _, err := fmt.Fprint(w, ": connected\n\n"); err != nil {
			return
		}
		if flush() != nil {
			return
		}
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		caughtUp := false
		for {
			events, err := b.Store.Events(after, 100)
			if err != nil {
				return
			}
			if len(events) == 0 && !caughtUp {
				// Marks the end of replay so clients can treat later events as new.
				caughtUp = true
				deadline()
				if _, err := fmt.Fprint(w, "event: live\ndata: {\"type\":\"live\"}\n\n"); err != nil {
					return
				}
				if flush() != nil {
					return
				}
			}
			for _, event := range events {
				event, err = publicEvent(event)
				if err != nil {
					return
				}
				data, err := json.Marshal(event)
				if err != nil {
					return
				}
				deadline()
				if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", event.ID, event.Type, data); err != nil {
					return
				}
				after = event.ID
			}
			if len(events) > 0 {
				if flush() != nil {
					return
				}
				continue
			}
			select {
			case <-r.Context().Done():
				return
			case <-sub.Done:
				return
			case <-sub.Wake:
			case event := <-sub.Live:
				if time.Since(event.Time) > 5*time.Second {
					continue
				}
				data, err := json.Marshal(event)
				if err != nil {
					return
				}
				deadline()
				if _, err := fmt.Fprintf(w, "event: typing\ndata: %s\n\n", data); err != nil {
					return
				}
				if flush() != nil {
					return
				}
			case <-ticker.C:
				deadline()
				if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
					return
				}
				if flush() != nil {
					return
				}
			}
		}
	})
	want := sha256.Sum256([]byte(token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if servePairingHelper(w, r) {
			return
		}
		if r.URL.Path == "/v1/pairing/credentials" {
			pairingCredentials(w, r, b)
			return
		}
		if serveAsset(w, r) {
			return
		}
		value, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		got := sha256.Sum256([]byte(value))
		if token == "" || !ok || subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" {
			if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+r.Host && origin != "https://"+r.Host {
				http.Error(w, "cross-origin request rejected", http.StatusForbidden)
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

func cursor(r *http.Request) (uint64, error) {
	value := r.Header.Get("Last-Event-ID")
	if value == "" {
		value = r.URL.Query().Get("after")
	}
	if value == "" {
		return 0, nil
	}
	return strconv.ParseUint(value, 10, 64)
}
func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
