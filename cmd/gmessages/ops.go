package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/client"
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/model"
)

// conversations indexes one fetch of the conversation list.
type conversations struct {
	list []model.Conversation
	byID map[string]model.Conversation
}

func loadConversations(ctx context.Context, c *client.Client) (*conversations, error) {
	list, err := c.Conversations(ctx)
	if err != nil {
		return nil, err
	}
	out := &conversations{list: list, byID: make(map[string]model.Conversation, len(list))}
	for _, conv := range list {
		out.byID[conv.ID] = conv
	}
	return out, nil
}

func digits(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsDigit(r) {
			return r
		}
		return -1
	}, s)
}

// others lists everyone but the owner, who Google sometimes also lists again
// under a second participant ID without the self flag.
func others(conv model.Conversation) []model.Participant {
	me := map[string]bool{}
	for _, p := range conv.Participants {
		if p.IsMe && p.Address != "" {
			me[p.Address] = true
		}
	}
	var out []model.Participant
	for _, p := range conv.Participants {
		if !p.IsMe && !me[p.Address] {
			out = append(out, p)
		}
	}
	return out
}

// matches reports whether filter names conv by ID, by a case-insensitive part
// of its name or a participant's name, or by the digits of an address.
func matches(conv model.Conversation, filter string) bool {
	if filter == "" || conv.ID == filter {
		return true
	}
	lower := strings.ToLower(filter)
	if strings.Contains(strings.ToLower(conv.Name), lower) {
		return true
	}
	want := digits(filter)
	for _, p := range others(conv) {
		if strings.Contains(strings.ToLower(p.Name), lower) {
			return true
		}
		if len(want) >= 4 && strings.Contains(digits(p.Address), want) {
			return true
		}
	}
	return false
}

// resolve picks the single conversation filter names, preferring an exact ID
// or name over partial matches.
func (cs *conversations) resolve(filter string) (model.Conversation, error) {
	if conv, ok := cs.byID[filter]; ok {
		return conv, nil
	}
	var exact, partial []model.Conversation
	for _, conv := range cs.list {
		if strings.EqualFold(conv.Name, filter) {
			exact = append(exact, conv)
		} else if matches(conv, filter) {
			partial = append(partial, conv)
		}
	}
	candidates := exact
	if len(candidates) == 0 {
		candidates = partial
	}
	switch len(candidates) {
	case 0:
		return model.Conversation{}, fmt.Errorf("no conversation matches %q", filter)
	case 1:
		return candidates[0], nil
	}
	var names []string
	for _, conv := range candidates[:min(len(candidates), 10)] {
		names = append(names, fmt.Sprintf("%s (%s)", conv.Name, conv.ID))
	}
	return model.Conversation{}, fmt.Errorf("%q matches %d conversations; use an ID: %s", filter, len(candidates), strings.Join(names, ", "))
}

// parseSince accepts a duration back from now ("15m", "2h", "3d") or an
// RFC 3339 time.
func parseSince(value string, now time.Time) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	if days, ok := strings.CutSuffix(value, "d"); ok {
		if d, err := time.ParseDuration(days + "h"); err == nil {
			return now.Add(-24 * d), nil
		}
	}
	if d, err := time.ParseDuration(value); err == nil {
		return now.Add(-d), nil
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("invalid since %q: use a duration like 15m or an RFC 3339 time", value)
}

type messageView struct {
	model.Message
	ConversationName string `json:"conversation_name"`
	SenderName       string `json:"sender_name"`
}

func (cs *conversations) view(m model.Message) messageView {
	v := messageView{Message: m}
	conv := cs.byID[m.ConversationID]
	v.ConversationName = conv.Name
	if m.Direction == "outgoing" {
		v.SenderName = "me"
	}
	for _, p := range conv.Participants {
		if p.ID == m.SenderID && v.SenderName == "" {
			v.SenderName = cmpOr(p.Name, p.Address)
		}
	}
	return v
}

func cmpOr(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

type searchOptions struct {
	Query        string
	Since        string
	Direction    string
	Conversation string
	Limit        int
}

func search(ctx context.Context, c *client.Client, o searchOptions) ([]messageView, error) {
	since, err := parseSince(o.Since, time.Now())
	if err != nil {
		return nil, err
	}
	limit := cmpLimit(o.Limit, 20)
	q := client.SearchQuery{Text: o.Query, Since: since, Direction: o.Direction, Limit: limit}
	if o.Conversation != "" {
		q.Limit = 500
	}
	messages, _, err := c.Search(ctx, q)
	if err != nil {
		return nil, err
	}
	cs, err := loadConversations(ctx, c)
	if err != nil {
		return nil, err
	}
	out := []messageView{}
	for _, m := range messages {
		if len(out) == limit {
			break
		}
		if o.Conversation != "" && !matches(cs.byID[m.ConversationID], o.Conversation) {
			continue
		}
		out = append(out, cs.view(m))
	}
	return out, nil
}

func cmpLimit(limit, fallback int) int {
	if limit <= 0 {
		return fallback
	}
	return min(limit, 500)
}

var errTimeout = errors.New("no matching message arrived before the timeout")

// wait returns the first message matching o. Messages already stored count
// only when o.Since reaches back to them; otherwise it watches for arrivals.
func wait(ctx context.Context, c *client.Client, o searchOptions, timeout time.Duration) (messageView, error) {
	start := time.Now()
	since, err := parseSince(o.Since, start)
	if err != nil {
		return messageView{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	q := client.SearchQuery{Text: o.Query, Since: since, Direction: o.Direction, Limit: 500}
	if since.IsZero() {
		q.Since, q.Limit = start, 1
	}
	existing, cursor, err := c.Search(ctx, q)
	if err != nil {
		return messageView{}, err
	}
	cs, err := loadConversations(ctx, c)
	if err != nil {
		return messageView{}, err
	}
	if !since.IsZero() {
		for _, m := range existing {
			if matches(cs.byID[m.ConversationID], o.Conversation) {
				return cs.view(m), nil
			}
		}
	} else {
		// The phone stamps messages with its own clock, so allow for skew
		// when deciding that an event is a new message rather than an update.
		since = start.Add(-2 * time.Minute)
	}
	query := strings.ToLower(o.Query)
	for {
		events, next, err := c.Events(ctx, cursor)
		if err != nil {
			if ctx.Err() != nil {
				return messageView{}, errTimeout
			}
			return messageView{}, err
		}
		cursor = next
		for _, e := range events {
			if e.Type != "message" {
				continue
			}
			var m model.Message
			if json.Unmarshal(e.Data, &m) != nil || m.Deleted || m.Time.Before(since) {
				continue
			}
			if o.Direction != "" && m.Direction != o.Direction {
				continue
			}
			if query != "" && !strings.Contains(strings.ToLower(m.Text), query) && !strings.Contains(strings.ToLower(m.Subject), query) {
				continue
			}
			if _, known := cs.byID[m.ConversationID]; !known {
				if fresh, err := loadConversations(ctx, c); err == nil {
					cs = fresh
				}
			}
			if matches(cs.byID[m.ConversationID], o.Conversation) {
				return cs.view(m), nil
			}
		}
		if len(events) > 0 {
			continue
		}
		select {
		case <-ctx.Done():
			return messageView{}, errTimeout
		case <-time.After(2 * time.Second):
		}
	}
}

type sendOptions struct {
	Conversation string
	To           []string
	Text         string
	Wait         bool
}

// send queues a text message, first creating the conversation when it is
// addressed by phone number, and optionally waits for Google to accept it.
func send(ctx context.Context, c *client.Client, o sendOptions) (model.Outbox, error) {
	if strings.TrimSpace(o.Text) == "" {
		return model.Outbox{}, errors.New("message text is empty")
	}
	if (o.Conversation == "") == (len(o.To) == 0) {
		return model.Outbox{}, errors.New("name exactly one of a conversation or recipient numbers")
	}
	conversationID := ""
	if o.Conversation != "" {
		cs, err := loadConversations(ctx, c)
		if err != nil {
			return model.Outbox{}, err
		}
		conv, err := cs.resolve(o.Conversation)
		if err != nil {
			return model.Outbox{}, err
		}
		conversationID = conv.ID
	} else {
		key := client.NewKey()
		if _, err := c.CreateConversation(ctx, key, o.To); err != nil {
			return model.Outbox{}, err
		}
		created, err := awaitWithin(ctx, c, key, 90*time.Second)
		if err != nil {
			return created, fmt.Errorf("conversation creation %s: %w", key, err)
		}
		if created.State != "accepted" || created.ConversationID == "" {
			return created, fmt.Errorf("conversation creation %s ended %s: %s", key, created.State, created.Detail)
		}
		conversationID = created.ConversationID
	}
	key := client.NewKey()
	queued, err := c.SendText(ctx, key, conversationID, o.Text)
	if err != nil || !o.Wait {
		return queued, err
	}
	return awaitWithin(ctx, c, key, 90*time.Second)
}

func awaitWithin(ctx context.Context, c *client.Client, key string, d time.Duration) (model.Outbox, error) {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	o, err := c.AwaitOutbox(ctx, key)
	if errors.Is(err, context.DeadlineExceeded) {
		return o, fmt.Errorf("still %s after %s; check `gmessages outbox %s`", o.State, d, key)
	}
	return o, err
}

func filterContacts(book model.ContactBook, query string) []model.Contact {
	if query == "" {
		return book.Contacts
	}
	lower, want := strings.ToLower(query), digits(query)
	return slices.DeleteFunc(slices.Clone(book.Contacts), func(c model.Contact) bool {
		if strings.Contains(strings.ToLower(c.Name), lower) {
			return false
		}
		return len(want) < 4 || !strings.Contains(digits(c.Address), want)
	})
}

func filterConversations(cs *conversations, query string, limit int) []model.Conversation {
	out := []model.Conversation{}
	for _, conv := range cs.list {
		if len(out) == limit {
			break
		}
		if matches(conv, query) {
			out = append(out, conv)
		}
	}
	return out
}

const timeLayout = "2006-01-02 15:04 MST"

func formatMessage(v messageView, withConversation bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[%s] ", v.Time.Local().Format(timeLayout))
	if withConversation {
		fmt.Fprintf(&b, "%s | ", cmpOr(v.ConversationName, v.ConversationID))
	}
	fmt.Fprintf(&b, "%s: %s", cmpOr(v.SenderName, v.SenderID, "unknown"), v.Text)
	for _, a := range v.Attachments {
		fmt.Fprintf(&b, " [attachment %s %s id=%s]", a.Name, a.MIME, a.ID)
	}
	if withConversation {
		fmt.Fprintf(&b, "  (conversation=%s message=%s)", v.ConversationID, v.ID)
	} else {
		fmt.Fprintf(&b, "  (message=%s)", v.ID)
	}
	return b.String()
}

func formatConversation(c model.Conversation) string {
	var people []string
	for _, p := range others(c) {
		people = append(people, cmpOr(p.Address, p.Name))
	}
	unread := ""
	if c.Unread {
		unread = " [unread]"
	}
	return fmt.Sprintf("%s  [%s] %s <%s>%s: %s", c.ID, c.Updated.Local().Format(timeLayout), c.Name, strings.Join(people, ", "), unread, c.Preview)
}

func formatContact(c model.Contact) string {
	return fmt.Sprintf("%s  %s", cmpOr(c.Address, c.Formatted, "(no number)"), c.Name)
}

func formatOutbox(o model.Outbox) string {
	s := fmt.Sprintf("%s: %s", o.ID, o.State)
	if o.ConversationID != "" || o.Request.ConversationID != "" {
		s += " conversation=" + cmpOr(o.ConversationID, o.Request.ConversationID)
	}
	if o.Detail != "" {
		s += " (" + o.Detail + ")"
	}
	return s
}
