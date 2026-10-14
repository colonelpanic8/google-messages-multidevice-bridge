// Command gmessages reads and sends messages through a running bridge, and
// serves the same operations to agents over MCP (`gmessages mcp`).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/client"
)

const usage = `usage: gmessages [global flags] <command> [flags]

Commands:
  status                         connection and phone status
  conversations [--query Q]      recent conversations, newest first
  messages <conversation>        one conversation's messages, oldest first
  search [--query Q] [--since 15m] [--direction incoming] [--conversation C]
                                 messages across every conversation, newest first
  wait [--query Q] [--conversation C] [--since D] [--timeout 2m]
                                 block until a matching message arrives, then print it
  contacts [--query Q]           the phone's address book
  send (--conversation C | --to +15550100) [--no-wait] <text|->
                                 send a text message
  outbox <idempotency-key>       one queued operation
  attachment <id> [-o path]      download an attachment
  profiles                       configured bridge profiles
  mcp [--allow-send]             serve these operations over MCP on stdio

A conversation is named by ID, by part of its name, or by a phone number.

Global flags (also accepted after the command):
  --profile NAME   profile from the config file (env GOOGLE_MESSAGES_BRIDGE_PROFILE)
  --url URL        bridge URL (env GOOGLE_MESSAGES_BRIDGE_URL)
  --token-file F   API token file (env GOOGLE_MESSAGES_BRIDGE_TOKEN[_FILE])
  --json           print JSON records instead of text
`

type globals struct {
	profile, url, tokenFile string
	json                    bool
}

func (g *globals) register(fs *flag.FlagSet) {
	fs.StringVar(&g.profile, "profile", g.profile, "")
	fs.StringVar(&g.url, "url", g.url, "")
	fs.StringVar(&g.tokenFile, "token-file", g.tokenFile, "")
	fs.BoolVar(&g.json, "json", g.json, "")
}

func (g *globals) client() (*client.Client, error) {
	s, err := client.Resolve(g.profile, client.Settings{URL: g.url, TokenFile: g.tokenFile})
	if err != nil {
		return nil, err
	}
	return client.New(s)
}

// parse lets flags and positional arguments interleave, which the flag
// package alone does not.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return positional, nil
		}
		positional = append(positional, args[0])
		args = args[1:]
	}
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdin, os.Stdout)
	cancel()
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "gmessages:", err)
		if errors.Is(err, errTimeout) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	g := &globals{profile: os.Getenv("GOOGLE_MESSAGES_BRIDGE_PROFILE")}
	top := flag.NewFlagSet("gmessages", flag.ContinueOnError)
	top.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	g.register(top)
	if err := top.Parse(args); err != nil {
		return err
	}
	if top.NArg() == 0 {
		top.Usage()
		return errors.New("missing command")
	}
	command, args := top.Arg(0), top.Args()[1:]
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	g.register(fs)
	print := func(value any, text func() string) error {
		if g.json {
			enc := json.NewEncoder(stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(value)
		}
		if s := text(); s != "" {
			fmt.Fprintln(stdout, s)
		}
		return nil
	}

	switch command {
	case "status":
		if _, err := parse(fs, args); err != nil {
			return err
		}
		c, err := g.client()
		if err != nil {
			return err
		}
		status, err := c.Status(ctx)
		if err != nil {
			return err
		}
		return print(status, func() string { return formatStatus(status) })

	case "conversations":
		query := fs.String("query", "", "")
		limit := fs.Int("limit", 20, "")
		if _, err := parse(fs, args); err != nil {
			return err
		}
		c, err := g.client()
		if err != nil {
			return err
		}
		cs, err := loadConversations(ctx, c)
		if err != nil {
			return err
		}
		list := filterConversations(cs, *query, cmpLimit(*limit, 20))
		return print(list, func() string { return joinLines(list, formatConversation) })

	case "messages":
		limit := fs.Int("limit", 20, "")
		before := fs.String("before", "", "")
		positional, err := parse(fs, args)
		if err != nil {
			return err
		}
		if len(positional) != 1 {
			return errors.New("messages takes one conversation")
		}
		c, err := g.client()
		if err != nil {
			return err
		}
		views, next, err := conversationMessages(ctx, c, positional[0], *limit, *before)
		if err != nil {
			return err
		}
		return print(map[string]any{"messages": views, "next_before": next}, func() string {
			return formatConversationMessages(views, next)
		})

	case "search":
		var o searchOptions
		searchFlags(fs, &o)
		fs.IntVar(&o.Limit, "limit", 20, "")
		if _, err := parse(fs, args); err != nil {
			return err
		}
		c, err := g.client()
		if err != nil {
			return err
		}
		views, err := search(ctx, c, o)
		if err != nil {
			return err
		}
		return print(views, func() string {
			return joinLines(views, func(v messageView) string { return formatMessage(v, true) })
		})

	case "wait":
		var o searchOptions
		searchFlags(fs, &o)
		timeout := fs.Duration("timeout", 2*time.Minute, "")
		if _, err := parse(fs, args); err != nil {
			return err
		}
		c, err := g.client()
		if err != nil {
			return err
		}
		v, err := wait(ctx, c, o, *timeout)
		if err != nil {
			return err
		}
		return print(v, func() string { return formatMessage(v, true) })

	case "contacts":
		query := fs.String("query", "", "")
		refresh := fs.Bool("refresh", false, "")
		if _, err := parse(fs, args); err != nil {
			return err
		}
		c, err := g.client()
		if err != nil {
			return err
		}
		book, err := c.Contacts(ctx, *refresh)
		if err != nil {
			return err
		}
		list := filterContacts(book, *query)
		return print(list, func() string { return joinLines(list, formatContact) })

	case "send":
		var o sendOptions
		var to string
		fs.StringVar(&o.Conversation, "conversation", "", "")
		fs.StringVar(&to, "to", "", "")
		noWait := fs.Bool("no-wait", false, "")
		positional, err := parse(fs, args)
		if err != nil {
			return err
		}
		if len(positional) != 1 {
			return errors.New("send takes the message text, or - to read it from stdin")
		}
		o.Text = positional[0]
		if o.Text == "-" {
			data, err := io.ReadAll(io.LimitReader(stdin, 16001))
			if err != nil {
				return err
			}
			o.Text = strings.TrimRight(string(data), "\n")
		}
		if to != "" {
			o.To = strings.Split(to, ",")
		}
		o.Wait = !*noWait
		c, err := g.client()
		if err != nil {
			return err
		}
		outbox, err := send(ctx, c, o)
		if err != nil {
			return err
		}
		return print(outbox, func() string { return formatOutbox(outbox) })

	case "outbox":
		positional, err := parse(fs, args)
		if err != nil {
			return err
		}
		if len(positional) != 1 {
			return errors.New("outbox takes one idempotency key")
		}
		c, err := g.client()
		if err != nil {
			return err
		}
		o, err := c.Outbox(ctx, positional[0])
		if err != nil {
			return err
		}
		return print(o, func() string { return formatOutbox(o) })

	case "attachment":
		output := fs.String("o", "", "")
		positional, err := parse(fs, args)
		if err != nil {
			return err
		}
		if len(positional) != 1 {
			return errors.New("attachment takes one attachment ID")
		}
		c, err := g.client()
		if err != nil {
			return err
		}
		data, err := c.Attachment(ctx, positional[0])
		if err != nil {
			return err
		}
		if *output == "" || *output == "-" {
			_, err = stdout.Write(data)
			return err
		}
		return os.WriteFile(*output, data, 0o600)

	case "profiles":
		if _, err := parse(fs, args); err != nil {
			return err
		}
		profiles, err := client.Profiles()
		if err != nil {
			return err
		}
		return print(profiles, func() string {
			names := make([]string, 0, len(profiles))
			for name := range profiles {
				names = append(names, name)
			}
			sort.Strings(names)
			return joinLines(names, func(name string) string { return name + "  " + profiles[name].URL })
		})

	case "mcp":
		allowSend := fs.Bool("allow-send", false, "")
		if _, err := parse(fs, args); err != nil {
			return err
		}
		c, err := g.client()
		if err != nil {
			return err
		}
		return serveMCP(ctx, c, *allowSend, stdin, stdout)

	case "help":
		fmt.Fprint(stdout, usage)
		return nil
	}
	return fmt.Errorf("unknown command %q; run gmessages help", command)
}

func searchFlags(fs *flag.FlagSet, o *searchOptions) {
	fs.StringVar(&o.Query, "query", "", "")
	fs.StringVar(&o.Since, "since", "", "")
	fs.StringVar(&o.Direction, "direction", "", "")
	fs.StringVar(&o.Conversation, "conversation", "", "")
}

func conversationMessages(ctx context.Context, c *client.Client, filter string, limit int, before string) ([]messageView, string, error) {
	cs, err := loadConversations(ctx, c)
	if err != nil {
		return nil, "", err
	}
	conv, err := cs.resolve(filter)
	if err != nil {
		return nil, "", err
	}
	messages, next, err := c.Messages(ctx, conv.ID, cmpLimit(limit, 20), before)
	if err != nil {
		return nil, "", err
	}
	views := make([]messageView, len(messages))
	for i, m := range messages {
		views[len(messages)-1-i] = cs.view(m)
	}
	return views, next, nil
}

func formatConversationMessages(views []messageView, next string) string {
	if len(views) == 0 {
		return "no messages"
	}
	s := fmt.Sprintf("%s (%s)\n", views[0].ConversationName, views[0].ConversationID)
	s += joinLines(views, func(v messageView) string { return formatMessage(v, false) })
	if next != "" {
		s += "\n(older messages: --before " + next + ")"
	}
	return s
}

func formatStatus(status map[string]any) string {
	keys := make([]string, 0, len(status))
	for key := range status {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	lines := make([]string, 0, len(keys))
	for _, key := range keys {
		if value := status[key]; value != nil && value != "" {
			lines = append(lines, fmt.Sprintf("%s: %v", key, value))
		}
	}
	return strings.Join(lines, "\n")
}

func joinLines[T any](items []T, format func(T) string) string {
	lines := make([]string, len(items))
	for i, item := range items {
		lines[i] = format(item)
	}
	return strings.Join(lines, "\n")
}
