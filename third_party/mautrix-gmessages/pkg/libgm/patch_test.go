package libgm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"go.mau.fi/util/exhttp"
	"google.golang.org/protobuf/proto"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/events"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
)

func testClient() *Client {
	return NewClient(NewAuthData(), nil, zerolog.Nop(), exhttp.SensibleClientSettings)
}

func testClientWithServer(server *httptest.Server) (*Client, *rewriteTransport) {
	transport := &rewriteTransport{target: server.Listener.Addr().String(), inner: http.DefaultTransport}
	settings := exhttp.SensibleClientSettings
	settings.TransportOverride = func(exhttp.ClientSettings) http.RoundTripper { return transport }
	return NewClient(NewAuthData(), nil, zerolog.Nop(), settings), transport
}

func loggedInTestClient(server *httptest.Server) (*Client, *rewriteTransport) {
	c, transport := testClientWithServer(server)
	c.AuthData.setDevices(&gmproto.Device{}, &gmproto.Device{})
	c.updateTachyonAuthToken(&gmproto.TokenData{
		TachyonAuthToken: []byte("test-token"),
		TTL:              int64((24 * time.Hour) / time.Microsecond),
	})
	return c, transport
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
	lifecycle, err := c.lifecycleFor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c.sessionHandler.startAckInterval(lifecycle)
	c.sessionHandler.startAckInterval(lifecycle)
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
	lifecycle, err = c.lifecycleFor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c.sessionHandler.startAckInterval(lifecycle)
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

	transport.hits.Store(0)
	_, err = c.GetOrCreateConversation(ctx, &gmproto.GetOrCreateConversationRequest{})
	if err == nil {
		t.Fatal("server error must surface")
	}
	if got := transport.hits.Load(); got != 1 {
		t.Fatalf("GetOrCreateConversation issued %d POSTs", got)
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

func TestUploadMediaContextCancellationAndBoundedResponse(t *testing.T) {
	t.Run("start before headers", func(t *testing.T) {
		started := make(chan struct{})
		release := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(started)
			select {
			case <-r.Context().Done():
			case <-release:
			}
		}))
		defer func() {
			close(release)
			server.Close()
		}()
		c, _ := testClientWithServer(server)
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() {
			_, err := c.StartUploadMediaContext(ctx, []byte("encrypted"), "image/png")
			result <- err
		}()
		<-started
		cancel()
		select {
		case err := <-result:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("start upload returned %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("start upload did not stop after cancellation")
		}
	})

	t.Run("finalize before headers", func(t *testing.T) {
		started := make(chan struct{})
		release := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(started)
			select {
			case <-r.Context().Done():
			case <-release:
			}
		}))
		defer func() {
			close(release)
			server.Close()
		}()
		c, _ := testClientWithServer(server)
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() {
			_, err := c.FinalizeUploadMediaContext(ctx, &StartGoogleUpload{
				UploadURL:           server.URL,
				MimeType:            "image/png",
				EncryptedMediaBytes: []byte("encrypted"),
			})
			result <- err
		}()
		<-started
		cancel()
		select {
		case err := <-result:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("finalize upload returned %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("finalize upload did not stop after cancellation")
		}
	})

	t.Run("full upload finalize cancellation", func(t *testing.T) {
		finalizeStarted := make(chan struct{})
		release := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/finalize" {
				w.Header().Set("x-goog-upload-chunk-granularity", "1")
				w.Header().Set("x-goog-upload-url", "http://"+r.Host+"/finalize")
				w.WriteHeader(http.StatusOK)
				return
			}
			close(finalizeStarted)
			select {
			case <-r.Context().Done():
			case <-release:
			}
		}))
		defer func() {
			close(release)
			server.Close()
		}()
		c, _ := testClientWithServer(server)
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() {
			_, err := c.UploadMediaContext(ctx, []byte("media"), "photo.png", "image/png")
			result <- err
		}()
		<-finalizeStarted
		cancel()
		select {
		case err := <-result:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("upload returned %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("upload did not stop after cancellation")
		}
	})

	t.Run("bounded finalize response", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, strings.Repeat("x", maxUploadResponseBytes+1))
		}))
		defer server.Close()
		c, _ := testClientWithServer(server)
		_, err := c.FinalizeUploadMediaContext(context.Background(), &StartGoogleUpload{
			UploadURL:           server.URL,
			MimeType:            "image/png",
			EncryptedMediaBytes: []byte("encrypted"),
		})
		if err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("expected bounded response error, got %v", err)
		}
	})
}

func TestConnectDisconnectLifecycleIsReusable(t *testing.T) {
	started := make(chan int32, 4)
	release := make(chan struct{})
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := requests.Add(1)
		started <- request
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer func() {
		close(release)
		server.Close()
	}()
	c, transport := loggedInTestClient(server)

	const callers = 8
	var connects sync.WaitGroup
	connects.Add(callers)
	errs := make(chan error, callers)
	for range callers {
		go func() {
			defer connects.Done()
			errs <- c.Connect(context.Background())
		}()
	}
	connects.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Connect returned %v", err)
		}
	}
	if request := <-started; request != 1 {
		t.Fatalf("first poll request number is %d", request)
	}
	if got := transport.hits.Load(); got != 1 {
		t.Fatalf("concurrent Connect issued %d poll requests", got)
	}

	var disconnects sync.WaitGroup
	disconnects.Add(callers)
	for range callers {
		go func() {
			defer disconnects.Done()
			c.Disconnect()
		}()
	}
	disconnects.Wait()
	if c.pollIsRunning() {
		t.Fatal("poll still marked running after Disconnect")
	}

	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("reusable Connect returned %v", err)
	}
	if request := <-started; request != 2 {
		t.Fatalf("second poll request number is %d", request)
	}
	c.Disconnect()
}

func TestDisconnectCancelsAndJoinsInflightAck(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer func() {
		close(release)
		server.Close()
	}()
	c, _ := loggedInTestClient(server)
	lifecycle, err := c.lifecycleFor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c.sessionHandler.queueMessageAck("ack-1")
	if !c.goWorker(lifecycle, func(ctx context.Context) { c.sessionHandler.sendAckRequest(ctx) }) {
		t.Fatal("failed to start ack worker")
	}
	<-started
	c.Disconnect()
	c.sessionHandler.ackMapLock.Lock()
	queued := append([]string(nil), c.sessionHandler.ackMap...)
	c.sessionHandler.ackMapLock.Unlock()
	if len(queued) != 1 || queued[0] != "ack-1" {
		t.Fatalf("canceled ack was not requeued: %v", queued)
	}
}

func TestDisconnectJoinsCallbacksAndClosesAdmission(t *testing.T) {
	c := testClient()
	_, err := c.lifecycleFor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	c.SetEventHandler(func(any) {
		calls.Add(1)
		close(started)
		<-release
	})
	callbackDone := make(chan struct{})
	go func() {
		_ = c.triggerEvent("event")
		close(callbackDone)
	}()
	<-started
	disconnected := make(chan struct{})
	go func() {
		c.Disconnect()
		close(disconnected)
	}()
	select {
	case <-disconnected:
		t.Fatal("Disconnect returned while a callback was still running")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case <-disconnected:
	case <-time.After(time.Second):
		t.Fatal("Disconnect did not join callback")
	}
	<-callbackDone
	if err := c.triggerEvent("late"); !errors.Is(err, ErrConnectionClosed) {
		t.Fatalf("late callback admission returned %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler called %d times", got)
	}
	var pairCalls atomic.Int32
	pairCallback := func(*gmproto.PairedData) { pairCalls.Add(1) }
	c.PairCallback.Store(&pairCallback)
	if err := c.completePairing(&gmproto.PairedData{}); !errors.Is(err, ErrConnectionClosed) {
		t.Fatalf("late pair callback admission returned %v", err)
	}
	if got := pairCalls.Load(); got != 0 {
		t.Fatalf("pair callback called %d times after Disconnect", got)
	}
}

func TestHandlerErrorDefersAck(t *testing.T) {
	c := testClient()
	pairData, err := proto.Marshal(&gmproto.RPCPairData{
		Event: &gmproto.RPCPairData_Revoked{Revoked: &gmproto.RevokePairData{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw := &gmproto.IncomingRPCMessage{
		ResponseID:  "incoming-1",
		BugleRoute:  gmproto.BugleRoute_PairEvent,
		MessageData: pairData,
	}
	rejected := errors.New("not persisted")
	c.SetEventHandlerWithError(func(any) error { return rejected })
	c.HandleRPCMsg(raw)
	c.sessionHandler.ackMapLock.Lock()
	queued := len(c.sessionHandler.ackMap)
	c.sessionHandler.ackMapLock.Unlock()
	if queued != 0 {
		t.Fatalf("rejected event queued %d acks", queued)
	}
	c.SetEventHandler(func(any) {})
	c.HandleRPCMsg(raw)
	c.sessionHandler.ackMapLock.Lock()
	queued = len(c.sessionHandler.ackMap)
	c.sessionHandler.ackMapLock.Unlock()
	if queued != 1 {
		t.Fatalf("accepted event queued %d acks", queued)
	}
}

func TestHandlerAndFirstListStateAreRaceSafe(t *testing.T) {
	c := testClient()
	var annotations atomic.Int32
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if c.nextListConversationsMessageType() == gmproto.MessageType_BUGLE_ANNOTATION {
				annotations.Add(1)
			}
		}()
	}
	for range 100 {
		wg.Add(3)
		go func() {
			defer wg.Done()
			c.SetEventHandler(func(any) {})
		}()
		go func() {
			defer wg.Done()
			c.SetEventHandlerWithError(func(any) error { return nil })
		}()
		go func() {
			defer wg.Done()
			if err := c.triggerEvent("event"); err != nil {
				t.Errorf("triggerEvent returned %v", err)
			}
		}()
	}
	wg.Wait()
	if got := annotations.Load(); got != 1 {
		t.Fatalf("annotation message type selected %d times", got)
	}
}

func TestStartLoginCancellationBeforeHeaders(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer func() {
		close(release)
		server.Close()
	}()
	c, _ := testClientWithServer(server)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := c.StartLogin(ctx)
		result <- err
	}()
	<-started
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("StartLogin returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("StartLogin did not stop after cancellation")
	}
	c.Disconnect()
}

func TestAuthFailureClassificationSeparatesCredentialsFromTransport(t *testing.T) {
	for _, err := range []error{
		ErrNoAuthToken,
		ErrNotLoggedIn,
		events.ErrInvalidCredentials,
		events.ErrRequestedEntityNotFound,
		events.HTTPError{Resp: &http.Response{StatusCode: http.StatusUnauthorized}},
		events.HTTPError{Resp: &http.Response{StatusCode: http.StatusForbidden}},
		events.HTTPError{Resp: &http.Response{StatusCode: http.StatusNotFound}},
	} {
		if !IsAuthFailure(err) {
			t.Errorf("expected authentication failure: %v", err)
		}
	}
	for _, err := range []error{
		context.DeadlineExceeded,
		errors.New("network unavailable"),
		events.HTTPError{Resp: &http.Response{StatusCode: http.StatusInternalServerError}},
	} {
		if IsAuthFailure(err) {
			t.Errorf("transient failure classified as authentication: %v", err)
		}
	}
}

func TestGaiaPairingResponseCorrelatesByActionWhenSessionIDDiffers(t *testing.T) {
	c := testClient()
	const requestID = "client-request-id"
	ch := c.sessionHandler.waitResponse(requestID)
	c.sessionHandler.responseWaitersLock.Lock()
	c.sessionHandler.gaiaPairingWaiters[gmproto.ActionType_CREATE_GAIA_PAIRING_CLIENT_INIT] = requestID
	c.sessionHandler.responseWaitersLock.Unlock()

	// Google answers pairing requests with a session ID it generates itself
	// rather than echoing the request ID back.
	msg := &IncomingRPCMessage{
		IncomingRPCMessage: &gmproto.IncomingRPCMessage{ResponseID: "server-response"},
		Message: &gmproto.RPCMessageData{
			SessionID: "server-generated-session-id",
			Action:    gmproto.ActionType_CREATE_GAIA_PAIRING_CLIENT_INIT,
		},
	}
	if !c.sessionHandler.receiveResponse(msg) {
		t.Fatal("pairing response was not delivered to the waiting request")
	}
	select {
	case got := <-ch:
		if got != msg {
			t.Fatal("waiter received a different message")
		}
	default:
		t.Fatal("waiter channel was empty")
	}

	// The correlation is one-shot: a duplicate must not be claimed again.
	if c.sessionHandler.receiveResponse(msg) {
		t.Fatal("duplicate pairing response was claimed twice")
	}
}

func TestNonPairingResponseStillRequiresMatchingSessionID(t *testing.T) {
	c := testClient()
	const requestID = "client-request-id"
	c.sessionHandler.waitResponse(requestID)
	msg := &IncomingRPCMessage{
		IncomingRPCMessage: &gmproto.IncomingRPCMessage{ResponseID: "server-response"},
		Message: &gmproto.RPCMessageData{
			SessionID: "unrelated-session-id",
			Action:    gmproto.ActionType_LIST_MESSAGES,
		},
	}
	if c.sessionHandler.receiveResponse(msg) {
		t.Fatal("unrelated response was matched to a pending request")
	}
}

func TestOldLoggedOutEventDoesNotCancelPairing(t *testing.T) {
	for _, tc := range []struct {
		name      string
		isOld     bool
		wantEvent bool
	}{
		{name: "replayed from previous session", isOld: true, wantEvent: false},
		{name: "live logout", isOld: false, wantEvent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient()
			var loggedOut bool
			c.SetEventHandler(func(evt any) {
				if _, ok := evt.(*events.GaiaLoggedOut); ok {
					loggedOut = true
				}
			})
			msg := &IncomingRPCMessage{
				IncomingRPCMessage: &gmproto.IncomingRPCMessage{ResponseID: "incoming-logout"},
				IsOld:              tc.isOld,
				Message: &gmproto.RPCMessageData{
					Action:          gmproto.ActionType_GET_UPDATES,
					UnencryptedData: hackyLoggedOutBytes,
				},
			}
			if err := c.handleUpdatesEvent(msg); err != nil {
				t.Fatalf("handleUpdatesEvent: %v", err)
			}
			if loggedOut != tc.wantEvent {
				t.Fatalf("GaiaLoggedOut fired = %v, want %v", loggedOut, tc.wantEvent)
			}
		})
	}
}
