package bridge

import (
	"encoding/json"
	"io"
)

// libgm logs message bodies and session material in some fields, so only
// connection metadata reaches the journal.
var libgmLogFields = map[string]bool{
	"level": true, "time": true, "message": true, "error": true,
	"action_type": true, "message_action": true, "request_id": true,
	"request_message_id": true, "response_message_id": true, "message_id": true,
	"message_ids": true, "conv_id": true, "is_old": true, "ping_id": true,
	"duration": true, "timeout_count": true, "urgent": true, "reason": true,
	"liveness_source": true, "not_responding_sent": true, "skip_count": true,
	"sleep_seconds": true, "listen_id": true, "bugle_default": true,
}

type libgmLogWriter struct{ out io.Writer }

func (w libgmLogWriter) Write(p []byte) (int, error) {
	var entry map[string]json.RawMessage
	if err := json.Unmarshal(p, &entry); err != nil {
		return len(p), nil
	}
	for key := range entry {
		if !libgmLogFields[key] {
			delete(entry, key)
		}
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return len(p), nil
	}
	if _, err = w.out.Write(append([]byte("libgm "), append(line, '\n')...)); err != nil {
		return 0, err
	}
	return len(p), nil
}
