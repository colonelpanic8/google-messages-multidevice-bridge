package api

import (
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/bridge"
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/model"
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/provider"
	"io"
	"net/http"
	"strings"
)

func registerFeatures(mux *http.ServeMux, b *bridge.Bridge) {
	mux.HandleFunc("POST /v1/connection/restart", func(w http.ResponseWriter, r *http.Request) { b.RequestReconnect(); w.WriteHeader(http.StatusAccepted) })
	queue := func(w http.ResponseWriter, r *http.Request, req model.SendRequest) {
		out, created, err := b.Queue(r.Header.Get("Idempotency-Key"), req)
		if err != nil {
			apiError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if created {
			w.WriteHeader(http.StatusAccepted)
		}
		writeJSON(w, out)
	}
	mux.HandleFunc("POST /v1/conversations", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Recipients []string `json:"recipients"`
		}
		if !decode(w, r, &req) {
			return
		}
		queue(w, r, model.SendRequest{Kind: "conversation", Recipients: req.Recipients})
	})
	mux.HandleFunc("POST /v1/conversations/{id}/reactions", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			MessageID string `json:"message_id"`
			Emoji     string `json:"emoji"`
			Remove    bool   `json:"remove"`
		}
		if !decode(w, r, &req) {
			return
		}
		queue(w, r, model.SendRequest{Kind: "reaction", ConversationID: r.PathValue("id"), MessageID: req.MessageID, Emoji: req.Emoji, Remove: req.Remove})
	})
	mux.HandleFunc("POST /v1/conversations/{id}/typing", func(w http.ResponseWriter, r *http.Request) {
		if err := b.Typing(r.Context(), r.PathValue("id")); err != nil {
			apiError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /v1/attachments/{id}/request", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if len(id) != 64 || strings.ContainsAny(id, "/\\.") {
			apiError(w, bridge.ErrInvalid)
			return
		}
		if err := b.RequestAttachment(r.Context(), id); err != nil {
			apiError(w, err)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("POST /v1/uploads", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, provider.MaxAttachmentBytes)
		data, err := io.ReadAll(r.Body)
		if err != nil {
			apiError(w, provider.ErrTooLarge)
			return
		}
		out, err := b.SaveUpload(r.URL.Query().Get("name"), r.Header.Get("Content-Type"), data)
		if err != nil {
			apiError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, out)
	})
	mux.HandleFunc("POST /v1/history", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ConversationID string `json:"conversation_id"`
			Folder         string `json:"folder"`
			Restart        bool   `json:"restart"`
		}
		if !decode(w, r, &req) {
			return
		}
		job, err := b.QueueHistory(req.ConversationID, req.Folder, req.Restart)
		if err != nil {
			apiError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		writeJSON(w, job)
	})
	mux.HandleFunc("GET /v1/history", func(w http.ResponseWriter, r *http.Request) {
		jobs, cursor, err := b.Store.Snapshot("history")
		if err != nil {
			apiError(w, err)
			return
		}
		writeJSON(w, map[string]any{"jobs": jobs, "cursor": cursor})
	})
	mux.HandleFunc("POST /v1/history/{id}/pause", func(w http.ResponseWriter, r *http.Request) {
		job, err := b.Store.PauseHistory(r.PathValue("id"))
		if err != nil {
			apiError(w, err)
			return
		}
		b.Hub.Notify()
		writeJSON(w, job)
	})
}
