package whatsapp

import (
	"context"
	"encoding/json"
	"fmt"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waVnameCert"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

type pendingEvent struct {
	Version  int
	Kind     string
	Data     json.RawMessage
	Payloads map[string][]byte `json:",omitempty"`
}

func encodePending(kind string, event any) (pendingEvent, error) {
	e := pendingEvent{Version: 1, Kind: kind, Payloads: map[string][]byte{}}
	payloads := map[string]proto.Message{}
	var metadata any
	switch kind {
	case "message":
		m := *event.(*events.Message)
		payloads["message"], payloads["raw"], payloads["web"] = m.Message, m.RawMessage, m.SourceWebMsg
		m.Message, m.RawMessage, m.SourceWebMsg = nil, nil, nil
		if m.Info.VerifiedName != nil {
			v := *m.Info.VerifiedName
			payloads["certificate"], payloads["details"] = v.Certificate, v.Details
			v.Certificate, v.Details = nil, nil
			m.Info.VerifiedName = &v
		}
		metadata = &m
	case "history":
		h := *event.(*events.HistorySync)
		payloads["history"], payloads["notification"] = h.Data, h.Notification
		h.Data, h.Notification = nil, nil
		metadata = &h
	case "history-chat":
		h := event.(historyChat)
		payloads["conversation"] = h.Conversation
		h.Conversation = nil
		metadata = h
	case "receipt":
		metadata = event.(*events.Receipt)
	default:
		return e, fmt.Errorf("unknown inbox kind %q", kind)
	}
	for key, message := range payloads {
		if !message.ProtoReflect().IsValid() {
			continue
		}
		raw, err := proto.Marshal(message)
		if err != nil {
			return e, err
		}
		e.Payloads[key] = raw
	}
	var err error
	e.Data, err = json.Marshal(metadata)
	return e, err
}

func decodePending(raw []byte) (string, any, error) {
	var e pendingEvent
	if err := json.Unmarshal(raw, &e); err != nil {
		return "", nil, err
	}
	if e.Version != 0 && e.Version != 1 {
		return e.Kind, nil, fmt.Errorf("unsupported inbox version %d", e.Version)
	}
	var target any
	switch e.Kind {
	case "message":
		target = &events.Message{}
	case "history":
		target = &events.HistorySync{}
	case "history-chat":
		target = &historyChat{}
	case "receipt":
		target = &events.Receipt{}
	default:
		return e.Kind, nil, fmt.Errorf("unknown inbox kind %q", e.Kind)
	}
	if err := json.Unmarshal(e.Data, target); err != nil {
		return e.Kind, nil, err
	}
	if e.Version == 0 {
		return e.Kind, target, nil
	}
	payloads := map[string]proto.Message{}
	switch m := target.(type) {
	case *events.Message:
		if _, ok := e.Payloads["message"]; ok {
			m.Message = &waE2E.Message{}
			payloads["message"] = m.Message
		}
		if _, ok := e.Payloads["raw"]; ok {
			m.RawMessage = &waE2E.Message{}
			payloads["raw"] = m.RawMessage
		}
		if _, ok := e.Payloads["web"]; ok {
			m.SourceWebMsg = &waWeb.WebMessageInfo{}
			payloads["web"] = m.SourceWebMsg
		}
		if m.Info.VerifiedName != nil {
			if _, ok := e.Payloads["certificate"]; ok {
				m.Info.VerifiedName.Certificate = &waVnameCert.VerifiedNameCertificate{}
				payloads["certificate"] = m.Info.VerifiedName.Certificate
			}
			if _, ok := e.Payloads["details"]; ok {
				m.Info.VerifiedName.Details = &waVnameCert.VerifiedNameCertificate_Details{}
				payloads["details"] = m.Info.VerifiedName.Details
			}
		}
	case *events.HistorySync:
		if _, ok := e.Payloads["history"]; ok {
			m.Data = &waHistorySync.HistorySync{}
			payloads["history"] = m.Data
		}
		if _, ok := e.Payloads["notification"]; ok {
			m.Notification = &waE2E.HistorySyncNotification{}
			payloads["notification"] = m.Notification
		}
	case *historyChat:
		if _, ok := e.Payloads["conversation"]; ok {
			m.Conversation = &waHistorySync.Conversation{}
			payloads["conversation"] = m.Conversation
		}
	}
	for key, message := range payloads {
		if err := proto.Unmarshal(e.Payloads[key], message); err != nil {
			return e.Kind, nil, err
		}
	}
	if len(payloads) != len(e.Payloads) {
		return e.Kind, nil, fmt.Errorf("unknown inbox payload")
	}
	return e.Kind, target, nil
}

func (p *Provider) quarantine(ctx context.Context, id string, raw []byte) error {
	return p.Keys.txn(ctx, func(ctx context.Context) error {
		if err := p.Keys.failed(p.DB.NetworkPut(ctx, p.Keys.key("quarantine", id), raw)); err != nil {
			return err
		}
		count, err := p.Quarantined(ctx)
		if err != nil {
			return err
		}
		if err = p.Keys.put(ctx, "meta", "quarantined", count+1); err != nil {
			return err
		}
		return p.Keys.del(ctx, "inbox", id)
	})
}

func (p *Provider) Quarantined(ctx context.Context) (uint64, error) {
	var count uint64
	_, err := p.Keys.get(ctx, "meta", "quarantined", &count)
	return count, err
}
