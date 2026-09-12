package libgm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"go.mau.fi/util/exhttp"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
)

func testClient() *Client {
	return NewClient(NewAuthData(), nil, zerolog.Nop(), exhttp.SensibleClientSettings)
}

func TestNoRetrySendsExactlyOneRequestOnServerError(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()
	c := testClient()
	ctx := context.Background()
	res, err := c.makeProtobufHTTPRequestContext(ctx, server.URL, &gmproto.EmptyArr{}, ContentTypePBLite, false, true)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadGateway || hits.Load() != 1 {
		t.Fatalf("status %d hits %d", res.StatusCode, hits.Load())
	}
	res, err = c.makeProtobufHTTPRequestContext(ctx, server.URL, &gmproto.EmptyArr{}, ContentTypePBLite, false, false)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if hits.Load() != 1+ServerErrorMaxAttempts {
		t.Fatalf("background requests should still retry: hits %d", hits.Load())
	}
}

func TestPOSTRedirectIsNotFollowed(t *testing.T) {
	var target atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/target", func(w http.ResponseWriter, r *http.Request) { target.Add(1) })
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/target", http.StatusTemporaryRedirect)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	c := testClient()
	res, err := c.makeProtobufHTTPRequestContext(context.Background(), server.URL+"/send", &gmproto.EmptyArr{}, ContentTypePBLite, false, true)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusTemporaryRedirect || target.Load() != 0 {
		t.Fatalf("status %d target hits %d", res.StatusCode, target.Load())
	}
	get, err := c.http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	get.Body.Close()
	if target.Load() != 1 {
		t.Fatal("GET redirects should still be followed")
	}
}

func TestTokenRefreshDoesNotRaceRequestBuilding(t *testing.T) {
	c := testClient()
	c.AuthData.Browser = &gmproto.Device{}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			c.updateTachyonAuthToken(&gmproto.TokenData{TachyonAuthToken: []byte{byte(i)}, TTL: 1000})
		}
	}()
	for i := 0; i < 200; i++ {
		if _, _, err := c.sessionHandler.buildMessage(SendMessageParams{Action: gmproto.ActionType_GET_UPDATES}); err != nil {
			t.Fatal(err)
		}
		c.AuthData.CookiesLock.RLock()
		_, err := json.Marshal(c.AuthData)
		c.AuthData.CookiesLock.RUnlock()
		if err != nil {
			t.Fatal(err)
		}
		_ = c.checkLoggedIn()
	}
	close(stop)
	wg.Wait()
}

func TestDisconnectJoinsAckTicker(t *testing.T) {
	c := testClient()
	c.sessionHandler.startAckInterval()
	c.sessionHandler.startAckInterval()
	c.sessionHandler.ackRunLock.Lock()
	done := c.sessionHandler.ackDone
	c.sessionHandler.ackRunLock.Unlock()
	c.Disconnect()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ack goroutine still running after Disconnect")
	}
	c.sessionHandler.queueMessageAck("m1")
	c.sessionHandler.startAckInterval()
	c.Disconnect()
	c.sessionHandler.ackMapLock.Lock()
	queued := len(c.sessionHandler.ackMap)
	c.sessionHandler.ackMapLock.Unlock()
	if queued != 1 {
		t.Fatalf("queued acks lost on stop: %d", queued)
	}
}

type rewriteTransport struct {
	target string
	hits   atomic.Int32
	inner  http.RoundTripper
}

func (r *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodPost {
		r.hits.Add(1)
	}
	clone := req.Clone(req.Context())
	clone.URL.Scheme, clone.URL.Host = "http", r.target
	clone.Host = r.target
	return r.inner.RoundTrip(clone)
}

func TestSendMessageIssuesOnePOSTOnServerError(t *testing.T) {
	var redirectResponse atomic.Bool
	var redirectTargetHits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirected" {
			redirectTargetHits.Add(1)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if redirectResponse.Load() {
			http.Redirect(w, r, "/redirected", http.StatusTemporaryRedirect)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	transport := &rewriteTransport{target: server.Listener.Addr().String(), inner: http.DefaultTransport}
	settings := exhttp.SensibleClientSettings
	settings.TransportOverride = func(exhttp.ClientSettings) http.RoundTripper { return transport }
	c := NewClient(NewAuthData(), nil, zerolog.Nop(), settings)
	c.AuthData.Browser = &gmproto.Device{}
	c.AuthData.Mobile = &gmproto.Device{}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := c.SendMessage(ctx, &gmproto.SendMessageRequest{ConversationID: "c1", TmpID: "tmp"})
	if err == nil {
		t.Fatal("server error must surface")
	}
	if got := transport.hits.Load(); got != 1 {
		t.Fatalf("SendMessage issued %d POSTs", got)
	}

	transport.hits.Store(0)
	redirectResponse.Store(true)
	_, err = c.SendMessage(ctx, &gmproto.SendMessageRequest{ConversationID: "c1", TmpID: "tmp-redirect"})
	if err == nil {
		t.Fatal("redirect must surface as an error")
	}
	if got := transport.hits.Load(); got != 1 {
		t.Fatalf("SendMessage issued %d POSTs after redirect", got)
	}
	if got := redirectTargetHits.Load(); got != 0 {
		t.Fatalf("redirect replay reached target %d times", got)
	}

	transport.hits.Store(0)
	redirectResponse.Store(false)
	if err := c.MarkRead(ctx, "c1", "m1"); err == nil {
		t.Fatal("server error must surface")
	}
	if got := transport.hits.Load(); got != ServerErrorMaxAttempts {
		t.Fatalf("background request issued %d POSTs, want %d", got, ServerErrorMaxAttempts)
	}
}

func TestPairingKeySwapDoesNotRaceDecryption(t *testing.T) {
	c := testClient()
	payload, err := c.AuthData.requestCrypto().Encrypt([]byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	errCh := make(chan error, 1)
	wg.Add(2)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_, _ = c.AuthData.requestCrypto().Decrypt(payload)
			_, _, _ = c.sessionHandler.buildMessage(SendMessageParams{Action: gmproto.ActionType_GET_UPDATES, Data: &gmproto.EmptyArr{}})
		}
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			c.AuthData.CookiesLock.RLock()
			_, err := json.Marshal(c.AuthData)
			c.AuthData.CookiesLock.RUnlock()
			if err != nil {
				select {
				case errCh <- err:
				default:
				}
				return
			}
		}
	}()
	for i := 0; i < 100; i++ {
		fresh := NewAuthData().RequestCrypto
		aesKey, hmacKey := fresh.Keys()
		c.AuthData.finishPairing(aesKey, hmacKey, uuid.New())
	}
	close(stop)
	wg.Wait()
	select {
	case err := <-errCh:
		t.Fatal(err)
	default:
	}
}

func TestDownloadMediaContextCancelsBeforeHeaders(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	transport := &rewriteTransport{target: server.Listener.Addr().String(), inner: http.DefaultTransport}
	settings := exhttp.SensibleClientSettings
	settings.TransportOverride = func(exhttp.ClientSettings) http.RoundTripper { return transport }
	c := NewClient(NewAuthData(), nil, zerolog.Nop(), settings)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := c.DownloadMediaContext(ctx, "media", make([]byte, 32))
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("download did not reach local server")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("download returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("download did not stop after cancellation")
	}
}
