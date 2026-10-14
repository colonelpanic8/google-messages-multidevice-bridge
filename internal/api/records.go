package api

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/bridge"
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/model"
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/provider"
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/store"
)

// publicEvent normalizes legacy prototype records without exposing provider keys.
func publicEvent(e store.Event) (store.Event, error) {
	if e.Type != "message" && e.Type != "conversation" {
		return e, nil
	}
	s, legacy, err := provider.NormalizeLegacy(e.Type, e.Data)
	if err != nil {
		return e, err
	}
	if !legacy {
		return e, nil
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
		q, limit, ok := searchOptions(w, r)
		if !ok {
			return
		}
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
			if b.Network() == "whatsapp" && (strings.HasSuffix(c.ID, "@broadcast") || strings.HasSuffix(c.ID, "@newsletter") || (c.ReadOnly && c.Updated.IsZero() && c.Preview == "")) {
				continue
			}
			names, addresses := []string{c.Name}, []string{}
			for _, p := range c.Participants {
				names = append(names, p.Name)
				addresses = append(addresses, p.Address)
			}
			if searchMatch(q, names, addresses) {
				convs = append(convs, c)
			}
		}
		sort.Slice(convs, func(i, j int) bool {
			if convs[i].Updated.Equal(convs[j].Updated) {
				return convs[i].ID > convs[j].ID
			}
			return convs[i].Updated.After(convs[j].Updated)
		})
		if limit > 0 && len(convs) > limit {
			convs = convs[:limit]
		}
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
		messages, err := newestMessages(b, raw)
		if err != nil {
			apiError(w, err)
			return
		}
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
	mux.HandleFunc("GET /v1/messages", func(w http.ResponseWriter, r *http.Request) {
		limit, ok := pageLimit(w, r)
		if !ok {
			return
		}
		query := r.URL.Query()
		var since time.Time
		if value := query.Get("since"); value != "" {
			var err error
			if since, err = time.Parse(time.RFC3339, value); err != nil {
				http.Error(w, "since must be an RFC 3339 time", http.StatusBadRequest)
				return
			}
		}
		direction := query.Get("direction")
		if direction != "" && direction != "incoming" && direction != "outgoing" {
			http.Error(w, "direction must be incoming or outgoing", http.StatusBadRequest)
			return
		}
		q := strings.ToLower(strings.TrimSpace(query.Get("q")))
		raw, cursor, err := recentMessageRecords(b.Store, since)
		if err != nil {
			apiError(w, err)
			return
		}
		all, err := newestMessages(b, raw)
		if err != nil {
			apiError(w, err)
			return
		}
		messages := make([]model.Message, 0, limit)
		for _, m := range all {
			if m.Time.Before(since) || m.Deleted || direction != "" && m.Direction != direction {
				continue
			}
			if q != "" && !strings.Contains(strings.ToLower(m.Text), q) && !strings.Contains(strings.ToLower(m.Subject), q) {
				continue
			}
			if len(messages) == limit {
				break
			}
			messages = append(messages, m)
		}
		writeJSON(w, map[string]any{"messages": messages, "cursor": cursor})
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
		if st := b.Status(); !st.Transport || (st.State != "connected" && st.State != "degraded") {
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
			apiError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="attachment"`)
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		_, _ = w.Write(data)
	})
}

// recentMessageRecords narrows the scan to conversations updated since since,
// because a conversation's updated time never precedes its newest message.
func recentMessageRecords(s *store.Store, since time.Time) ([]store.SnapshotRecord, uint64, error) {
	if since.IsZero() {
		return s.SnapshotCurrent("message")
	}
	conversations, cursor, err := s.SnapshotCurrent("conversation")
	if err != nil {
		return nil, 0, err
	}
	var result []store.SnapshotRecord
	for _, record := range conversations {
		var c model.Conversation
		if err := json.Unmarshal(record.Data, &c); err != nil {
			return nil, 0, err
		}
		if c.Updated.Before(since) {
			continue
		}
		raw, _, err := s.ConversationMessages(c.ID)
		if err != nil {
			return nil, 0, err
		}
		result = append(result, raw...)
	}
	return result, cursor, nil
}

// newestMessages decodes stored messages newest first, leaving out the empty
// protocol records WhatsApp stores alongside real messages.
func newestMessages(b *bridge.Bridge, raw []store.SnapshotRecord) ([]model.Message, error) {
	messages := make([]model.Message, 0, len(raw))
	for _, record := range raw {
		var m model.Message
		if err := json.Unmarshal(record.Data, &m); err != nil {
			return nil, err
		}
		m.ReadOnly = !record.Current
		if b.Network() == "whatsapp" && !m.Deleted && m.Text == "" && len(m.Attachments) == 0 {
			continue
		}
		messages = append(messages, m)
	}
	sort.Slice(messages, func(i, j int) bool {
		if messages[i].Time.Equal(messages[j].Time) {
			return messages[i].ID > messages[j].ID
		}
		return messages[i].Time.After(messages[j].Time)
	})
	return messages, nil
}
