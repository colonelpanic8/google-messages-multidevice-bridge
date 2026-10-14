package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/client"
)

// The MCP stdio transport: newline-delimited JSON-RPC 2.0.

var protocolVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

const mcpInstructions = `Read and send SMS/RCS messages through the user's Google Messages bridge.
To fetch a verification code, call wait_for_message with a query such as "code" (and
since set a few minutes back if the code may already have arrived), or search_messages.
Never repeat verification codes or message contents beyond what the task needs.`

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations,omitempty"`
	send        bool
	call        func(context.Context, *client.Client, json.RawMessage) (string, error)
}

func object(required []string, properties map[string]any) map[string]any {
	schema := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func prop(kind, description string) map[string]any {
	return map[string]any{"type": kind, "description": description}
}

var (
	conversationProp = prop("string", "Conversation ID, part of its name or a participant's name, or a phone number")
	queryProp        = prop("string", "Case-insensitive text the message must contain")
	sinceProp        = prop("string", "Only messages at or after this: a duration back from now (15m, 2h, 3d) or an RFC 3339 time")
	directionProp    = map[string]any{"type": "string", "enum": []string{"incoming", "outgoing"}}
	limitProp        = prop("integer", "Maximum results, default 20, at most 500")
)

func decodeArgs(raw json.RawMessage, out any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	return d.Decode(out)
}

func textResult[T any](items []T, empty string, format func(T) string) string {
	if len(items) == 0 {
		return empty
	}
	return joinLines(items, format)
}

var tools = []tool{
	{
		Name:        "bridge_status",
		Description: "Connection state of the bridge and the phone behind it.",
		InputSchema: object(nil, map[string]any{}),
		call: func(ctx context.Context, c *client.Client, _ json.RawMessage) (string, error) {
			status, err := c.Status(ctx)
			return formatStatus(status), err
		},
	},
	{
		Name:        "list_conversations",
		Description: "Conversations, most recently active first, with their IDs, participants' numbers, and latest message preview.",
		InputSchema: object(nil, map[string]any{"query": prop("string", "Filter by name, participant, or phone number"), "limit": limitProp}),
		call: func(ctx context.Context, c *client.Client, raw json.RawMessage) (string, error) {
			var args struct {
				Query string `json:"query"`
				Limit int    `json:"limit"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return "", err
			}
			cs, err := loadConversations(ctx, c)
			if err != nil {
				return "", err
			}
			return textResult(filterConversations(cs, args.Query, cmpLimit(args.Limit, 20)), "no conversations", formatConversation), nil
		},
	},
	{
		Name:        "read_messages",
		Description: "Recent messages in one conversation, oldest first. Pass the returned before value to page further back.",
		InputSchema: object([]string{"conversation"}, map[string]any{"conversation": conversationProp, "limit": limitProp, "before": prop("string", "Message ID to page back from")}),
		call: func(ctx context.Context, c *client.Client, raw json.RawMessage) (string, error) {
			var args struct {
				Conversation string `json:"conversation"`
				Limit        int    `json:"limit"`
				Before       string `json:"before"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return "", err
			}
			views, next, err := conversationMessages(ctx, c, args.Conversation, args.Limit, args.Before)
			if err != nil {
				return "", err
			}
			return formatConversationMessages(views, next), nil
		},
	},
	{
		Name:        "search_messages",
		Description: "Search stored messages across all conversations, newest first.",
		InputSchema: object(nil, map[string]any{"query": queryProp, "since": sinceProp, "direction": directionProp, "conversation": conversationProp, "limit": limitProp}),
		call: func(ctx context.Context, c *client.Client, raw json.RawMessage) (string, error) {
			var o searchOptions
			if err := decodeArgs(raw, &o); err != nil {
				return "", err
			}
			views, err := search(ctx, c, o)
			if err != nil {
				return "", err
			}
			return textResult(views, "no matching messages", func(v messageView) string { return formatMessage(v, true) }), nil
		},
	},
	{
		Name: "wait_for_message",
		Description: "Block until a matching message arrives and return it; use it right after triggering an SMS such as a verification code. " +
			"Without since, only messages arriving after the call count; with since, a match already stored in that window returns at once.",
		InputSchema: object(nil, map[string]any{
			"query": queryProp, "since": sinceProp, "conversation": conversationProp,
			"direction":       map[string]any{"type": "string", "enum": []string{"incoming", "outgoing"}, "default": "incoming"},
			"timeout_seconds": prop("integer", "How long to wait, default 120, at most 600"),
		}),
		call: func(ctx context.Context, c *client.Client, raw json.RawMessage) (string, error) {
			var args struct {
				Query          string `json:"query"`
				Since          string `json:"since"`
				Conversation   string `json:"conversation"`
				Direction      string `json:"direction"`
				TimeoutSeconds int    `json:"timeout_seconds"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return "", err
			}
			timeout := 120 * time.Second
			if args.TimeoutSeconds > 0 {
				timeout = time.Duration(min(args.TimeoutSeconds, 600)) * time.Second
			}
			o := searchOptions{Query: args.Query, Since: args.Since, Conversation: args.Conversation, Direction: cmpOr(args.Direction, "incoming")}
			v, err := wait(ctx, c, o, timeout)
			if err != nil {
				return "", err
			}
			return formatMessage(v, true), nil
		},
	},
	{
		Name:        "list_contacts",
		Description: "Contacts from the phone's address book with their dialable numbers.",
		InputSchema: object(nil, map[string]any{"query": prop("string", "Filter by name or number")}),
		call: func(ctx context.Context, c *client.Client, raw json.RawMessage) (string, error) {
			var args struct {
				Query string `json:"query"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return "", err
			}
			book, err := c.Contacts(ctx, false)
			if err != nil {
				return "", err
			}
			return textResult(filterContacts(book, args.Query), "no matching contacts", formatContact), nil
		},
	},
	{
		Name:        "send_message",
		Description: "Send a text message from the user's phone, to an existing conversation or to E.164 phone numbers. Only send when the user asked for it.",
		InputSchema: object([]string{"text"}, map[string]any{
			"text":         prop("string", "Message text"),
			"conversation": conversationProp,
			"to":           map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "E.164 recipients for a new or existing conversation, instead of conversation"},
		}),
		Annotations: map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": false, "openWorldHint": true},
		send:        true,
		call: func(ctx context.Context, c *client.Client, raw json.RawMessage) (string, error) {
			var o sendOptions
			if err := decodeArgs(raw, &o); err != nil {
				return "", err
			}
			o.Wait = true
			outbox, err := send(ctx, c, o)
			if err != nil {
				return "", err
			}
			return formatOutbox(outbox), nil
		},
	},
}

type mcpServer struct {
	client  *client.Client
	tools   []tool
	out     *json.Encoder
	writeMu sync.Mutex
	callsMu sync.Mutex
	calls   map[string]context.CancelFunc
}

func serveMCP(ctx context.Context, c *client.Client, allowSend bool, in io.Reader, out io.Writer) error {
	s := &mcpServer{client: c, out: json.NewEncoder(out), calls: map[string]context.CancelFunc{}}
	for _, t := range tools {
		if t.send && !allowSend {
			continue
		}
		if t.Annotations == nil {
			t.Annotations = map[string]any{"readOnlyHint": true, "openWorldHint": true}
		}
		s.tools = append(s.tools, t)
	}
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	var inflight sync.WaitGroup
	defer inflight.Wait()
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			s.write(rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{Code: -32700, Message: "parse error"}})
			continue
		}
		if req.Method == "tools/call" && len(req.ID) > 0 {
			inflight.Add(1)
			go func() { defer inflight.Done(); s.handle(ctx, req) }()
			continue
		}
		s.handle(ctx, req)
	}
	return scanner.Err()
}

func (s *mcpServer) write(resp rpcResponse) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_ = s.out.Encode(resp)
}

func (s *mcpServer) handle(ctx context.Context, req rpcRequest) {
	result, err := s.dispatch(ctx, req)
	if len(req.ID) == 0 {
		return
	}
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result}
	if err != nil {
		resp.Result, resp.Error = nil, err
	}
	s.write(resp)
}

func (s *mcpServer) dispatch(ctx context.Context, req rpcRequest) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &params)
		version := protocolVersions[0]
		if slices.Contains(protocolVersions, params.ProtocolVersion) {
			version = params.ProtocolVersion
		}
		return map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "gmessages", "version": "0.1.0"},
			"instructions":    mcpInstructions,
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": s.tools}, nil
	case "tools/call":
		return s.callTool(ctx, req)
	case "notifications/cancelled":
		var params struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		if json.Unmarshal(req.Params, &params) == nil {
			s.callsMu.Lock()
			if cancel := s.calls[string(params.RequestID)]; cancel != nil {
				cancel()
			}
			s.callsMu.Unlock()
		}
		return nil, nil
	}
	if strings.HasPrefix(req.Method, "notifications/") {
		return nil, nil
	}
	return nil, &rpcError{Code: -32601, Message: "method not found: " + req.Method}
}

func (s *mcpServer) callTool(ctx context.Context, req rpcRequest) (any, *rpcError) {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return nil, &rpcError{Code: -32602, Message: "invalid params"}
	}
	i := slices.IndexFunc(s.tools, func(t tool) bool { return t.Name == params.Name })
	if i < 0 {
		return nil, &rpcError{Code: -32602, Message: "unknown tool: " + params.Name}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.callsMu.Lock()
	s.calls[string(req.ID)] = cancel
	s.callsMu.Unlock()
	defer func() {
		s.callsMu.Lock()
		delete(s.calls, string(req.ID))
		s.callsMu.Unlock()
	}()
	text, err := s.tools[i].call(ctx, s.client, params.Arguments)
	if err != nil {
		var httpErr *client.HTTPError
		if errors.As(err, &httpErr) && httpErr.Status == 401 {
			err = fmt.Errorf("%w (check the configured API token)", err)
		}
		return map[string]any{"content": []map[string]any{{"type": "text", "text": err.Error()}}, "isError": true}, nil
	}
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}, nil
}
