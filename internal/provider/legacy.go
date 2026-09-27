package provider

import (
	"encoding/json"
	"errors"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/model"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/util"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func NewTransactionID() string { return util.GenerateTmpID() }

// NormalizeLegacy upgrades the Google-only prototype format.
func NormalizeLegacy(kind string, data []byte) (Snapshot, bool, error) {
	var header struct {
		Schema int `json:"schema"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return Snapshot{}, false, err
	}
	if header.Schema == model.Schema {
		return Snapshot{}, false, nil
	}
	if header.Schema != 0 {
		return Snapshot{}, false, errors.New("unsupported event schema")
	}
	var msg proto.Message = &gmproto.Message{}
	if kind == "conversation" {
		msg = &gmproto.Conversation{}
	}
	if err := protojson.Unmarshal(data, msg); err != nil {
		return Snapshot{}, false, err
	}
	snap, err := SnapshotOf(msg)
	return snap, true, err
}
