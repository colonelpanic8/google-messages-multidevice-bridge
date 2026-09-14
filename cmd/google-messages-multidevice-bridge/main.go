package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/api"
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/bridge"
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/push"
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) < 2 {
		return errors.New("usage: google-messages-multidevice-bridge <pair|serve> [flags]")
	}
	command := os.Args[1]
	if command != "pair" && command != "serve" {
		return errors.New("expected pair or serve")
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	dbPath := flags.String("db", "data/google-messages-multidevice-bridge.db", "encrypted database path")
	listen := flags.String("listen", "127.0.0.1:0", "API listen address (port 0 selects a free port)")
	storagePass := flags.String("storage-key-pass-entry", "", "pass entry containing the base64 storage key")
	tokenPass := flags.String("api-token-pass-entry", "", "pass entry containing the API token")
	storageFile := flags.String("storage-key-file", "", "file containing the base64 storage key, for deployments without pass")
	tokenFile := flags.String("api-token-file", "", "file containing the API token, for deployments without pass")
	pushSubject := flags.String("push-subject", "https://github.com/colonelpanic8/google-messages-multidevice-bridge", "VAPID subject identifying this deployment to push services")
	offline := flags.Bool("offline", false, "serve stored history without connecting to Google")
	if err := flags.Parse(os.Args[2:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	storageValue, err := secretValue("GOOGLE_MESSAGES_MULTIDEVICE_BRIDGE_STORAGE_KEY", *storagePass, *storageFile)
	if err != nil {
		return err
	}
	key, err := base64.StdEncoding.DecodeString(storageValue)
	if err != nil || len(key) != 32 {
		return errors.New("GOOGLE_MESSAGES_MULTIDEVICE_BRIDGE_STORAGE_KEY must be a base64-encoded 32-byte key")
	}
	s, err := store.Open(*dbPath, key)
	if err != nil {
		return err
	}
	defer s.Close()
	b := bridge.New(s)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if command == "pair" {
		data, err := io.ReadAll(io.LimitReader(os.Stdin, 65537))
		if err != nil {
			return err
		}
		if len(data) > 65536 {
			return errors.New("cookie JSON exceeds 64 KiB")
		}
		var cookies map[string]string
		if err := json.Unmarshal(data, &cookies); err != nil || len(cookies) == 0 {
			return errors.New("pipe a Google cookie JSON object to stdin")
		}
		err = b.Run(ctx, false, cookies, func(code string) { fmt.Fprintln(os.Stderr, "Choose this emoji in Google Messages:", code) })
		if err == nil && ctx.Err() == nil {
			fmt.Fprintln(os.Stderr, "Encrypted pairing session saved. Start google-messages-multidevice-bridge serve.")
		}
		return err
	}
	token, err := secretValue("GOOGLE_MESSAGES_MULTIDEVICE_BRIDGE_API_TOKEN", *tokenPass, *tokenFile)
	if err != nil {
		return err
	}
	if len(token) < 32 {
		return errors.New("GOOGLE_MESSAGES_MULTIDEVICE_BRIDGE_API_TOKEN must contain at least 32 characters")
	}
	if err := b.Prepare(); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	sender, err := push.New(b.Store, *pushSubject)
	if err != nil {
		_ = listener.Close()
		return err
	}
	return serve(ctx, b, *offline, token, listener, sender)
}

func serve(parent context.Context, b *bridge.Bridge, offline bool, token string, listener net.Listener, sender *push.Sender) error {
	if err := b.Prepare(); err != nil {
		_ = listener.Close()
		return err
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	var err error
	var handlers sync.WaitGroup
	var handlerMu sync.Mutex
	closing := false
	b.SetOfflineOnly(offline)
	var pushService api.PushService
	if sender != nil {
		pushService = pushAdapter{sender}
	}
	handler := api.New(b, token, pushService)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerMu.Lock()
		if closing {
			handlerMu.Unlock()
			http.Error(w, "service stopping", http.StatusServiceUnavailable)
			return
		}
		handlers.Add(1)
		handlerMu.Unlock()
		defer handlers.Done()
		handler.ServeHTTP(w, r)
	}), BaseContext: func(net.Listener) context.Context { return ctx }, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 16 << 10}
	serverErr := make(chan error, 1)
	go func() { serverErr <- server.Serve(listener) }()
	bridgeErr := make(chan error, 1)
	go func() { bridgeErr <- b.ServeConnection(ctx, offline) }()
	var watcher sync.WaitGroup
	if sender != nil {
		watcher.Add(1)
		go func() { defer watcher.Done(); _ = b.WatchForPush(ctx, pushAdapter{sender}) }()
	}
	defer watcher.Wait()
	fmt.Fprintln(os.Stderr, "Google Messages Multi-Device Bridge listening on", listener.Addr())
	bridgeFinished := false
serveLoop:
	for {
		select {
		case failure := <-bridgeErr:
			bridgeFinished = true
			bridgeErr = nil
			if errors.Is(failure, bridge.ErrStorage) {
				err = failure
				break serveLoop
			}
			if failure != nil {
				fmt.Fprintln(os.Stderr, "Google connection stopped; stored history remains available. Check status and re-pair or restart when ready.")
			}
		case err = <-serverErr:
			if errors.Is(err, http.ErrServerClosed) {
				err = nil
			}
			break serveLoop
		case <-ctx.Done():
			break serveLoop
		}
	}

	cancel()
	shutdownCtx, finishShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	if shutdownErr := server.Shutdown(shutdownCtx); shutdownErr != nil {
		_ = server.Close()
	}
	finishShutdown()
	handlerMu.Lock()
	closing = true
	handlerMu.Unlock()
	handlers.Wait()
	if !bridgeFinished {
		if bridgeFailure := <-bridgeErr; err == nil {
			err = bridgeFailure
		}
	}
	return err
}

// A secret file wins over a pass entry, which wins over the environment, so an
// agenix/sops-nix deployment never needs an unlocked GPG agent.
func secretValue(env, entry, file string) (string, error) {
	if file != "" {
		return secretFileValue(file)
	}
	if entry == "" {
		return os.Getenv(env), nil
	}
	if strings.HasPrefix(entry, "-") || strings.ContainsAny(entry, "\r\n\x00") {
		return "", errors.New("invalid pass entry")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "pass", "show", entry)
	var output boundedSecret
	cmd.Stdout = &output
	if err := cmd.Run(); err != nil {
		return "", errors.New("could not read pass entry; ensure the password store is unlocked")
	}
	return strings.TrimSpace(output.value.String()), nil
}

func secretFileValue(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", errors.New("could not read secret file")
	}
	defer f.Close()
	var output boundedSecret
	if _, err := io.Copy(&output, io.LimitReader(f, 4097)); err != nil {
		return "", err
	}
	return strings.TrimSpace(output.value.String()), nil
}

type boundedSecret struct{ value strings.Builder }

func (w *boundedSecret) Write(data []byte) (int, error) {
	if w.value.Len()+len(data) > 4096 {
		return 0, errors.New("secret exceeds size limit")
	}
	return w.value.Write(data)
}

// pushAdapter bridges the notifier's positional arguments to the push payload.
type pushAdapter struct{ sender *push.Sender }

func (a pushAdapter) PublicKey() string                    { return a.sender.PublicKey() }
func (a pushAdapter) Subscribe(raw []byte) (string, error) { return a.sender.Subscribe(raw) }
func (a pushAdapter) Unsubscribe(endpoint string) error    { return a.sender.Unsubscribe(endpoint) }
func (a pushAdapter) Count() (int, error)                  { return a.sender.Count() }
func (a pushAdapter) Send(ctx context.Context, title, body, tag, conversation string) error {
	return a.sender.Send(ctx, push.Notification{Title: title, Body: body, Tag: tag, Conversation: conversation})
}
