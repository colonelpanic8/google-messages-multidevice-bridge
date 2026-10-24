package bridge

import (
	"bytes"
	"testing"

	"github.com/rs/zerolog"
)

func TestLibgmLogKeepsOnlyConnectionMetadata(t *testing.T) {
	var out bytes.Buffer
	log := zerolog.New(libgmLogWriter{&out})
	log.Debug().Str("request_id", "r1").Str("data", "c2VjcmV0").Str("evt_data", "c2VjcmV0").Msg("Got unexpected response")
	if got, want := out.String(), `libgm {"level":"debug","message":"Got unexpected response","request_id":"r1"}`+"\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
