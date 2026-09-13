package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/bridge"
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/model"
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/provider"
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/store"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// publicEvent normalizes legacy prototype records without exposing provider keys.
func publicEvent(e store.Event) (store.Event, error) {
	if e.Type != "message" && e.Type != "conversation" {
		return e, nil
	}
	var header struct {
		Schema int `json:"schema"`
	}
	if err := json.Unmarshal(e.Data, &header); err != nil {
		return e, err
	}
	if header.Schema == model.Schema {
		return e, nil
	}
	if header.Schema != 0 {
		return e, errors.New("unsupported event schema")
	}
	var raw proto.Message = &gmproto.Message{}
	if e.Type == "conversation" {
		raw = &gmproto.Conversation{}
	}
	if err := protojson.Unmarshal(e.Data, raw); err != nil {
		return e, err
	}
	s, err := provider.SnapshotOf(raw)
	if err != nil {
		return e, err
	}
	e.Data = s.Event.Data
	return e, nil
}
func apiError(w http.ResponseWriter, err error) {
	code, message := http.StatusServiceUnavailable, "service unavailable"
	switch {
	case errors.Is(err, provider.ErrTooLarge):
		code, message = 413, "attachment exceeds size limit"
	case errors.Is(err, bridge.ErrPairing):
		code, message = 503, "pairing is in progress"
	case errors.Is(err, bridge.ErrInvalid):
		code, message = 400, "invalid request"
	case errors.Is(err, store.ErrNotFound):
		code, message = 404, "record not found"
	case errors.Is(err, store.ErrConflict):
		code, message = 409, "idempotency conflict or operation already attempted"
	case errors.Is(err, provider.ErrUnavailable):
		code, message = 503, "phone connection unavailable"
	}
	http.Error(w, message, code)
}
func decode(w http.ResponseWriter, r *http.Request, out any) bool {
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != "application/json" {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if d.Decode(out) != nil || d.Decode(new(any)) != io.EOF {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return false
	}
	return true
}
func pageLimit(w http.ResponseWriter, r *http.Request) (int, bool) {
	limit := 100
	if value := r.URL.Query().Get("limit"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || n > 500 {
			http.Error(w, "limit must be 1..500", http.StatusBadRequest)
			return 0, false
		}
		limit = n
	}
	return limit, true
}
func registerRecords(mux *http.ServeMux, b *bridge.Bridge) {
	mux.HandleFunc("GET /v1/conversations", func(w http.ResponseWriter, r *http.Request) {
		raw, cursor, err := b.Store.SnapshotCurrent("conversation")
		if err != nil {
			apiError(w, err)
			return
		}
		convs := make([]model.Conversation, 0, len(raw))
		for _, record := range raw {
			var c model.Conversation
			if err = json.Unmarshal(record.Data, &c); err != nil {
				apiError(w, err)
				return
			}
			c.ReadOnly = !record.Current
			convs = append(convs, c)
		}
		sort.Slice(convs, func(i, j int) bool {
			if convs[i].Updated.Equal(convs[j].Updated) {
				return convs[i].ID > convs[j].ID
			}
			return convs[i].Updated.After(convs[j].Updated)
		})
		writeJSON(w, map[string]any{"conversations": convs, "cursor": cursor})
	})
	mux.HandleFunc("GET /v1/conversations/{id}/messages", func(w http.ResponseWriter, r *http.Request) {
		limit, ok := pageLimit(w, r)
		if !ok {
			return
		}
		if _, err := b.Store.Record("conversation", r.PathValue("id")); err != nil {
			apiError(w, err)
			return
		}
		raw, cursor, err := b.Store.ConversationMessages(r.PathValue("id"))
		if err != nil {
			apiError(w, err)
			return
		}
		messages := make([]model.Message, 0, len(raw))
		for _, record := range raw {
			var m model.Message
			if err = json.Unmarshal(record.Data, &m); err != nil {
				apiError(w, err)
				return
			}
			m.ReadOnly = !record.Current
			messages = append(messages, m)
		}
		sort.Slice(messages, func(i, j int) bool {
			if messages[i].Time.Equal(messages[j].Time) {
				return messages[i].ID > messages[j].ID
			}
			return messages[i].Time.After(messages[j].Time)
		})
		if before := r.URL.Query().Get("before"); before != "" {
			found := -1
			for i, m := range messages {
				if m.ID == before {
					found = i
					break
				}
			}
			if found < 0 {
				http.Error(w, "unknown before message", http.StatusBadRequest)
				return
			}
			messages = messages[found+1:]
		}
		more := len(messages) > limit
		if more {
			messages = messages[:limit]
		}
		next := ""
		if more {
			next = messages[len(messages)-1].ID
		}
		writeJSON(w, map[string]any{"messages": messages, "cursor": cursor, "next_before": next})
	})
	mux.HandleFunc("GET /v1/outbox", func(w http.ResponseWriter, r *http.Request) {
		records, cursor, err := b.Store.Snapshot("outbox")
		if err != nil {
			apiError(w, err)
			return
		}
		writeJSON(w, map[string]any{"outbox": records, "cursor": cursor})
	})
	mux.HandleFunc("GET /v1/outbox/{id}", func(w http.ResponseWriter, r *http.Request) {
		o, err := b.Store.Outbox(r.PathValue("id"))
		if err != nil {
			apiError(w, err)
			return
		}
		writeJSON(w, o)
	})
	mux.HandleFunc("POST /v1/messages", func(w http.ResponseWriter, r *http.Request) {
		var req model.SendRequest
		if !decode(w, r, &req) {
			return
		}
		o, created, err := b.Queue(r.Header.Get("Idempotency-Key"), req)
		if err != nil {
			apiError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if created {
			w.WriteHeader(http.StatusAccepted)
		}
		writeJSON(w, o)
	})
	mux.HandleFunc("POST /v1/outbox/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		o, err := b.Store.CancelQueued(r.PathValue("id"))
		if err != nil {
			apiError(w, err)
			return
		}
		b.Hub.Notify()
		writeJSON(w, o)
	})
	mux.HandleFunc("POST /v1/conversations/{id}/read", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			MessageID string `json:"message_id"`
		}
		if !decode(w, r, &req) {
			return
		}
		if err := b.MarkRead(r.Context(), r.PathValue("id"), req.MessageID); err != nil {
			apiError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /v1/sync", func(w http.ResponseWriter, r *http.Request) {
		if b.Status().State != "connected" {
			apiError(w, provider.ErrUnavailable)
			return
		}
		b.RequestSync()
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("GET /v1/attachments/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if len(id) != 64 || strings.ContainsAny(id, "/\\") {
			apiError(w, bridge.ErrInvalid)
			return
		}
		data, err := b.Attachment(r.Context(), id)
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) && !errors.Is(err, bridge.ErrInvalid) {
				fmt.Fprintf(os.Stderr, "attachment %s: %v\n", id, err)
			}
			apiError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="attachment"`)
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		_, _ = w.Write(data)
	})
}
