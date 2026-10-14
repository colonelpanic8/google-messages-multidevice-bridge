package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/api"
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/bridge"
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/model"
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/store"
)

const testToken = "test-token-with-at-least-32-characters"

func fixture(t *testing.T) (*store.Store, []string) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "bridge.db"), bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	server := httptest.NewServer(api.New(bridge.New(s), testToken, nil))
	t.Cleanup(server.Close)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(testToken), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOGLE_MESSAGES_BRIDGE_CONFIG", filepath.Join(t.TempDir(), "missing.json"))
	t.Setenv("GOOGLE_MESSAGES_BRIDGE_PROFILE", "")
	t.Setenv("GOOGLE_MESSAGES_BRIDGE_TOKEN", "")
	appendRecord(t, s, "conversation", model.Conversation{Schema: 1, ID: "c1", Name: "Bank", Updated: time.Now(), Participants: []model.Participant{
		{ID: "me", IsMe: true, Address: "+15550000000"},
		{ID: "me-again", Address: "+15550000000"},
		{ID: "bank", Name: "Bank", Address: "+15125550111"},
	}})
	appendRecord(t, s, "message", model.Message{Schema: 1, ID: "m1", ConversationID: "c1", SenderID: "bank", Time: time.Now().Add(-5 * time.Minute), Direction: "incoming", Text: "Your code is 123456"})
	return s, []string{"--url", server.URL, "--token-file", tokenFile}
}

func appendRecord(t *testing.T, s *store.Store, kind string, value any) {
	t.Helper()
	data, _ := json.Marshal(value)
	id := ""
	switch v := value.(type) {
	case model.Conversation:
		id = v.ID
	case model.Message:
		id = v.ID
	}
	if _, err := s.Append(store.Event{Type: kind, EntityID: id, Data: data}); err != nil {
		t.Fatal(err)
	}
}

func cli(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := run(context.Background(), args, strings.NewReader(stdin), &out)
	return out.String(), err
}

func TestSearchAndConversationsResolveNames(t *testing.T) {
	_, flags := fixture(t)
	out, err := cli(t, "", append([]string{"search"}, append(flags, "--query", "CODE", "--conversation", "5125550111")...)...)
	if err != nil || !strings.Contains(out, "Bank | Bank: Your code is 123456") {
		t.Fatalf("search: %q %v", out, err)
	}
	out, err = cli(t, "", append([]string{"conversations"}, flags...)...)
	if err != nil || !strings.Contains(out, "Bank <+15125550111>") {
		t.Fatalf("conversations should hide the duplicate owner: %q %v", out, err)
	}
	if _, err = cli(t, "", append([]string{"messages", "nobody"}, flags...)...); err == nil {
		t.Fatal("unknown conversation resolved")
	}
}

func TestWaitSeesOnlyNewMessagesUnlessSinceReachesBack(t *testing.T) {
	s, flags := fixture(t)
	out, err := cli(t, "", append([]string{"wait", "--query", "code", "--since", "10m"}, flags...)...)
	if err != nil || !strings.Contains(out, "123456") {
		t.Fatalf("wait --since: %q %v", out, err)
	}
	if _, err = cli(t, "", append([]string{"wait", "--query", "code", "--timeout", "100ms"}, flags...)...); err != errTimeout {
		t.Fatalf("stored message satisfied a wait for new ones: %v", err)
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		appendRecord(t, s, "message", model.Message{Schema: 1, ID: "m2", ConversationID: "c1", SenderID: "bank", Time: time.Now(), Direction: "incoming", Text: "Your code is 654321"})
	}()
	out, err = cli(t, "", append([]string{"wait", "--query", "code", "--timeout", "10s"}, flags...)...)
	if err != nil || !strings.Contains(out, "654321") {
		t.Fatalf("wait: %q %v", out, err)
	}
}

func TestMCPListsReadOnlyToolsAndCallsThem(t *testing.T) {
	_, flags := fixture(t)
	requests := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"search_messages","arguments":{"query":"code","since":"1h"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"send_message","arguments":{"conversation":"c1","text":"hi"}}}`,
	}
	out, err := cli(t, strings.Join(requests, "\n")+"\n", append([]string{"mcp"}, flags...)...)
	if err != nil {
		t.Fatal(err)
	}
	responses := map[string]map[string]any{}
	decoder := json.NewDecoder(strings.NewReader(out))
	for {
		var resp map[string]any
		if err := decoder.Decode(&resp); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		id, _ := json.Marshal(resp["id"])
		responses[string(id)] = resp
	}
	if len(responses) != 4 {
		t.Fatalf("want 4 responses, got %d: %s", len(responses), out)
	}
	if v := responses["1"]["result"].(map[string]any)["protocolVersion"]; v != "2025-06-18" {
		t.Fatalf("protocol version %v", v)
	}
	listed, _ := json.Marshal(responses["2"]["result"])
	if strings.Contains(string(listed), "send_message") || !strings.Contains(string(listed), "wait_for_message") {
		t.Fatalf("tools without --allow-send: %s", listed)
	}
	searched, _ := json.Marshal(responses["3"]["result"])
	if !strings.Contains(string(searched), "123456") {
		t.Fatalf("search_messages: %s", searched)
	}
	if responses["4"]["error"] == nil {
		t.Fatal("send_message was callable without --allow-send")
	}
}
