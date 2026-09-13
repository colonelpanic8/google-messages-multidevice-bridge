package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
)

// PushService is the subset of the sender the HTTP surface needs.
type PushService interface {
	PublicKey() string
	Subscribe(raw []byte) (string, error)
	Unsubscribe(endpoint string) error
	Count() (int, error)
	Send(ctx context.Context, title, body, tag, conversation string) error
}

const maxSubscriptionBytes = 4 << 10

func registerPush(mux *http.ServeMux, service PushService) {
	if service == nil {
		// Without a configured sender the routes still answer, so a client can
		// tell "push is off" apart from "this build has no push".
		mux.HandleFunc("GET /v1/push", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{"enabled": false})
		})
		return
	}
	mux.HandleFunc("GET /v1/push", func(w http.ResponseWriter, r *http.Request) {
		count, err := service.Count()
		if err != nil {
			apiError(w, err)
			return
		}
		writeJSON(w, map[string]any{"enabled": true, "public_key": service.PublicKey(), "subscriptions": count})
	})
	mux.HandleFunc("POST /v1/push/subscriptions", func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSubscriptionBytes))
		if err != nil {
			http.Error(w, "subscription too large", http.StatusRequestEntityTooLarge)
			return
		}
		if _, err = service.Subscribe(raw); err != nil {
			apiError(w, err)
			return
		}
		w.WriteHeader(http.StatusCreated)
	})
	// Delivery depends on the browser, the OS, and a third-party push service,
	// so the user needs a way to prove the whole chain works.
	mux.HandleFunc("POST /v1/push/test", func(w http.ResponseWriter, r *http.Request) {
		count, err := service.Count()
		if err != nil {
			apiError(w, err)
			return
		}
		if count == 0 {
			http.Error(w, "no device is subscribed", http.StatusConflict)
			return
		}
		if err = service.Send(r.Context(), "Test notification", "Your bridge can reach this device.", "bridge-test", ""); err != nil {
			http.Error(w, "push delivery failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /v1/push/subscriptions", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Endpoint string `json:"endpoint"`
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSubscriptionBytes))
		if err != nil || json.Unmarshal(raw, &body) != nil || body.Endpoint == "" {
			http.Error(w, "endpoint is required", http.StatusBadRequest)
			return
		}
		if err = service.Unsubscribe(body.Endpoint); err != nil {
			apiError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
