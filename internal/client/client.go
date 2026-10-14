// Package client is a typed HTTP client for the bridge API, shared by the
// command-line client and its MCP server.
package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/model"
)

// Settings locate one bridge. Empty fields fall through to the next source.
type Settings struct {
	URL            string `json:"url"`
	Token          string `json:"-"`
	TokenFile      string `json:"token_file"`
	TokenPassEntry string `json:"token_pass_entry"`
}

type configFile struct {
	DefaultProfile string              `json:"default_profile"`
	Profiles       map[string]Settings `json:"profiles"`
}

// ConfigPaths lists the profile files in precedence order.
func ConfigPaths() []string {
	if path := os.Getenv("GOOGLE_MESSAGES_BRIDGE_CONFIG"); path != "" {
		return []string{path}
	}
	var paths []string
	if dir, err := os.UserConfigDir(); err == nil {
		paths = append(paths, filepath.Join(dir, "gmessages", "config.json"))
	}
	return append(paths, "/etc/gmessages/config.json")
}

func loadConfig() (configFile, error) {
	for _, path := range ConfigPaths() {
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return configFile{}, err
		}
		var c configFile
		if err := json.Unmarshal(data, &c); err != nil {
			return configFile{}, fmt.Errorf("%s: %w", path, err)
		}
		return c, nil
	}
	return configFile{}, nil
}

func Profiles() (map[string]Settings, error) {
	config, err := loadConfig()
	if config.Profiles == nil {
		config.Profiles = map[string]Settings{}
	}
	return config.Profiles, err
}

// Resolve layers explicit flags over a named profile, or over the environment
// and then the default profile when no profile is named. The environment uses
// the same variables as the desktop client.
func Resolve(profile string, flags Settings) (Settings, error) {
	config, err := loadConfig()
	if err != nil {
		return Settings{}, err
	}
	var layers []Settings
	layers = append(layers, flags)
	if profile != "" {
		p, ok := config.Profiles[profile]
		if !ok {
			return Settings{}, fmt.Errorf("unknown profile %q", profile)
		}
		layers = append(layers, p)
	} else {
		layers = append(layers, Settings{
			URL:            os.Getenv("GOOGLE_MESSAGES_BRIDGE_URL"),
			Token:          os.Getenv("GOOGLE_MESSAGES_BRIDGE_TOKEN"),
			TokenFile:      os.Getenv("GOOGLE_MESSAGES_BRIDGE_TOKEN_FILE"),
			TokenPassEntry: os.Getenv("GOOGLE_MESSAGES_BRIDGE_TOKEN_PASS_ENTRY"),
		})
		name := config.DefaultProfile
		if name == "" {
			name = "default"
		}
		layers = append(layers, config.Profiles[name])
	}
	var out Settings
	tokenSet := false
	for _, layer := range layers {
		if out.URL == "" {
			out.URL = layer.URL
		}
		// The first layer naming any token source owns the token, so a flag's
		// token file is never shadowed by a profile's pass entry.
		if !tokenSet && (layer.Token != "" || layer.TokenFile != "" || layer.TokenPassEntry != "") {
			out.Token, out.TokenFile, out.TokenPassEntry = layer.Token, layer.TokenFile, layer.TokenPassEntry
			tokenSet = true
		}
	}
	if out.URL == "" {
		return Settings{}, fmt.Errorf("no bridge URL: set --url, GOOGLE_MESSAGES_BRIDGE_URL, or a profile in %s", strings.Join(ConfigPaths(), " or "))
	}
	return out, nil
}

func (s Settings) token() (string, error) {
	switch {
	case s.Token != "":
		return s.Token, nil
	case s.TokenFile != "":
		data, err := os.ReadFile(s.TokenFile)
		if err != nil {
			return "", fmt.Errorf("read token file: %w", err)
		}
		return strings.TrimSpace(string(data)), nil
	case s.TokenPassEntry != "":
		if strings.HasPrefix(s.TokenPassEntry, "-") {
			return "", errors.New("invalid pass entry")
		}
		out, err := exec.Command("pass", "show", s.TokenPassEntry).Output()
		if err != nil {
			return "", errors.New("could not read the token pass entry; ensure the password store is unlocked")
		}
		first, _, _ := strings.Cut(string(out), "\n")
		return strings.TrimSpace(first), nil
	}
	return "", errors.New("no API token: set --token-file, GOOGLE_MESSAGES_BRIDGE_TOKEN(_FILE), or a profile token")
}

type Client struct {
	base  *url.URL
	token string
	http  *http.Client
}

func New(s Settings) (*Client, error) {
	base, err := url.Parse(strings.TrimRight(s.URL, "/"))
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("invalid bridge URL %q", s.URL)
	}
	token, err := s.token()
	if err != nil {
		return nil, err
	}
	return &Client{base: base, token: token, http: &http.Client{Timeout: 60 * time.Second}}, nil
}

// HTTPError carries the status and body of a non-2xx response.
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("bridge returned %d: %s", e.Status, strings.TrimSpace(e.Body))
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any, key string, out any) error {
	u := *c.base
	u.Path += path
	u.RawQuery = query.Encode()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &HTTPError{Status: resp.StatusCode, Body: string(data)}
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if raw, ok := out.(*[]byte); ok {
		*raw, err = io.ReadAll(resp.Body)
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) Status(ctx context.Context) (map[string]any, error) {
	var out map[string]any
	return out, c.do(ctx, "GET", "/v1/status", nil, nil, "", &out)
}

func (c *Client) Conversations(ctx context.Context) ([]model.Conversation, error) {
	var out struct {
		Conversations []model.Conversation `json:"conversations"`
	}
	return out.Conversations, c.do(ctx, "GET", "/v1/conversations", nil, nil, "", &out)
}

// Messages returns one page of a conversation, newest first, and the message
// ID to pass as before for the next page.
func (c *Client) Messages(ctx context.Context, conversationID string, limit int, before string) ([]model.Message, string, error) {
	q := url.Values{"limit": {strconv.Itoa(limit)}}
	if before != "" {
		q.Set("before", before)
	}
	var out struct {
		Messages   []model.Message `json:"messages"`
		NextBefore string          `json:"next_before"`
	}
	err := c.do(ctx, "GET", "/v1/conversations/"+url.PathEscape(conversationID)+"/messages", q, nil, "", &out)
	return out.Messages, out.NextBefore, err
}

type SearchQuery struct {
	Text      string
	Since     time.Time
	Direction string
	Limit     int
}

// Search returns matching messages across conversations, newest first, and
// the event cursor that later events can be replayed from.
func (c *Client) Search(ctx context.Context, s SearchQuery) ([]model.Message, uint64, error) {
	q := url.Values{}
	if s.Text != "" {
		q.Set("q", s.Text)
	}
	if !s.Since.IsZero() {
		q.Set("since", s.Since.UTC().Format(time.RFC3339))
	}
	if s.Direction != "" {
		q.Set("direction", s.Direction)
	}
	if s.Limit > 0 {
		q.Set("limit", strconv.Itoa(s.Limit))
	}
	var out struct {
		Messages []model.Message `json:"messages"`
		Cursor   uint64          `json:"cursor"`
	}
	err := c.do(ctx, "GET", "/v1/messages", q, nil, "", &out)
	return out.Messages, out.Cursor, err
}

type Event struct {
	ID       uint64          `json:"id"`
	Type     string          `json:"type"`
	EntityID string          `json:"entity_id"`
	Data     json.RawMessage `json:"data"`
}

func (c *Client) Events(ctx context.Context, after uint64) ([]Event, uint64, error) {
	q := url.Values{"after": {strconv.FormatUint(after, 10)}, "limit": {"1000"}}
	var out struct {
		Events     []Event `json:"events"`
		NextCursor uint64  `json:"next_cursor"`
	}
	err := c.do(ctx, "GET", "/v1/events", q, nil, "", &out)
	return out.Events, out.NextCursor, err
}

func (c *Client) Contacts(ctx context.Context, refresh bool) (model.ContactBook, error) {
	q := url.Values{}
	if refresh {
		q.Set("refresh", "1")
	}
	var out model.ContactBook
	return out, c.do(ctx, "GET", "/v1/contacts", q, nil, "", &out)
}

func (c *Client) Outbox(ctx context.Context, key string) (model.Outbox, error) {
	var out model.Outbox
	return out, c.do(ctx, "GET", "/v1/outbox/"+url.PathEscape(key), nil, nil, "", &out)
}

func (c *Client) CreateConversation(ctx context.Context, key string, recipients []string) (model.Outbox, error) {
	var out model.Outbox
	body := map[string]any{"recipients": recipients}
	return out, c.do(ctx, "POST", "/v1/conversations", nil, body, key, &out)
}

func (c *Client) SendText(ctx context.Context, key, conversationID, text string) (model.Outbox, error) {
	var out model.Outbox
	body := map[string]any{"conversation_id": conversationID, "text": text}
	return out, c.do(ctx, "POST", "/v1/messages", nil, body, key, &out)
}

func (c *Client) Attachment(ctx context.Context, id string) ([]byte, error) {
	var out []byte
	return out, c.do(ctx, "GET", "/v1/attachments/"+url.PathEscape(id), nil, nil, "", &out)
}

// Settled reports whether an outbox state will not change without the phone.
func Settled(state string) bool {
	switch state {
	case "accepted", "confirmed", "rejected", "ambiguous", "canceled":
		return true
	}
	return false
}

// AwaitOutbox polls an operation until it settles or ctx ends.
func (c *Client) AwaitOutbox(ctx context.Context, key string) (model.Outbox, error) {
	for {
		o, err := c.Outbox(ctx, key)
		if err != nil || Settled(o.State) {
			return o, err
		}
		select {
		case <-ctx.Done():
			return o, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// NewKey returns a fresh idempotency key.
func NewKey() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "gmessages-" + hex.EncodeToString(b)
}
