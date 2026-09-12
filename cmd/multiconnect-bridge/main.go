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
	"os/signal"
	"syscall"
	"time"

	"github.com/colonelpanic8/multiconnect-bridge/internal/api"
	"github.com/colonelpanic8/multiconnect-bridge/internal/bridge"
	"github.com/colonelpanic8/multiconnect-bridge/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) < 2 {
		return errors.New("usage: multiconnect-bridge <pair|serve> [flags]")
	}
	command := os.Args[1]
	if command != "pair" && command != "serve" {
		return errors.New("expected pair or serve")
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	dbPath := flags.String("db", "data/multiconnect-bridge.db", "encrypted database path")
	listen := flags.String("listen", "127.0.0.1:0", "API listen address (port 0 selects a free port)")
	offline := flags.Bool("offline", false, "serve stored history without connecting to Google")
	if err := flags.Parse(os.Args[2:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	key, err := base64.StdEncoding.DecodeString(os.Getenv("MULTICONNECT_BRIDGE_STORAGE_KEY"))
	if err != nil || len(key) != 32 {
		return errors.New("MULTICONNECT_BRIDGE_STORAGE_KEY must be a base64-encoded 32-byte key")
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
			fmt.Fprintln(os.Stderr, "Encrypted pairing session saved. Start multiconnect-bridge serve.")
		}
		return err
	}
	token := os.Getenv("MULTICONNECT_BRIDGE_API_TOKEN")
	if len(token) < 32 {
		return errors.New("MULTICONNECT_BRIDGE_API_TOKEN must contain at least 32 characters")
	}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: api.New(b, token), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 16 << 10}
	serverErr := make(chan error, 1)
	go func() { serverErr <- server.Serve(listener) }()
	bridgeErr := make(chan error, 1)
	go func() { bridgeErr <- b.Run(ctx, *offline, nil, nil) }()
	fmt.Fprintln(os.Stderr, "Multiconnect Bridge listening on", listener.Addr())
	bridgeFinished := false
	select {
	case err = <-bridgeErr:
		bridgeFinished = true
	case err = <-serverErr:
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
	case <-ctx.Done():
	}
	cancel()
	_ = server.Close()
	if !bridgeFinished {
		if bridgeFailure := <-bridgeErr; err == nil {
			err = bridgeFailure
		}
	}
	return err
}
