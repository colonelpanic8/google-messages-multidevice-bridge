package libgm

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"go.mau.fi/util/exhttp"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/crypto"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/events"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/util"
)

type AuthData struct {
	// Keys used to encrypt communication with the phone
	RequestCrypto *crypto.AESCTRHelper `json:"request_crypto,omitempty"`
	// Key used to sign requests to refresh the tachyon auth token from the server
	RefreshKey *crypto.JWK `json:"refresh_key,omitempty"`
	// Identity of the paired phone and browser
	Browser *gmproto.Device `json:"browser,omitempty"`
	Mobile  *gmproto.Device `json:"mobile,omitempty"`
	// Key used to authenticate with the server
	TachyonAuthToken []byte    `json:"tachyon_token,omitempty"`
	TachyonExpiry    time.Time `json:"tachyon_expiry,omitempty"`
	TachyonTTL       int64     `json:"tachyon_ttl,omitempty"`
	// Unknown encryption key, not used for anything
	WebEncryptionKey []byte `json:"web_encryption_key,omitempty"`

	SessionID uuid.UUID `json:"session_id,omitempty"`
	DestRegID uuid.UUID `json:"dest_reg_id,omitempty"`
	PairingID uuid.UUID `json:"pairing_id,omitempty"`

	Cookies map[string]string `json:"cookies,omitempty"`
	// CookiesLock guards serialized AuthData fields that can change after a
	// client is published. Callers taking a JSON snapshot hold its read lock.
	CookiesLock sync.RWMutex `json:"-"`
}

// TachyonToken returns the current auth token under the lock. Callers must not
// hold CookiesLock.
func (ad *AuthData) TachyonToken() []byte {
	ad.CookiesLock.RLock()
	defer ad.CookiesLock.RUnlock()
	return ad.TachyonAuthToken
}

// requestCrypto returns the phone-session cipher. Pairing replaces it while the
// pairing long poll is already delivering encrypted data.
func (ad *AuthData) requestCrypto() *crypto.AESCTRHelper {
	ad.CookiesLock.RLock()
	defer ad.CookiesLock.RUnlock()
	return ad.RequestCrypto
}

func (ad *AuthData) finishPairing(aesKey, hmacKey []byte, pairingID uuid.UUID) {
	ad.CookiesLock.Lock()
	defer ad.CookiesLock.Unlock()
	ad.RequestCrypto.SetKeys(aesKey, hmacKey)
	ad.PairingID = pairingID
}

func (ad *AuthData) tachyonTTL() int64 {
	ad.CookiesLock.RLock()
	defer ad.CookiesLock.RUnlock()
	return ad.TachyonTTL
}

func (ad *AuthData) tachyonExpiry() time.Time {
	ad.CookiesLock.RLock()
	defer ad.CookiesLock.RUnlock()
	return ad.TachyonExpiry
}

func (ad *AuthData) devices() (mobile, browser *gmproto.Device) {
	ad.CookiesLock.RLock()
	defer ad.CookiesLock.RUnlock()
	return ad.Mobile, ad.Browser
}

func (ad *AuthData) setDevices(mobile, browser *gmproto.Device) {
	ad.CookiesLock.Lock()
	ad.Mobile, ad.Browser = mobile, browser
	ad.CookiesLock.Unlock()
}

func (ad *AuthData) sessionUUID() uuid.UUID {
	ad.CookiesLock.RLock()
	defer ad.CookiesLock.RUnlock()
	return ad.SessionID
}

func (ad *AuthData) setSessionUUID(sessionID uuid.UUID) {
	ad.CookiesLock.Lock()
	ad.SessionID = sessionID
	ad.CookiesLock.Unlock()
}

func (ad *AuthData) setDestRegID(destRegID uuid.UUID) {
	ad.CookiesLock.Lock()
	ad.DestRegID = destRegID
	ad.CookiesLock.Unlock()
}

func (ad *AuthData) destRegID() uuid.UUID {
	ad.CookiesLock.RLock()
	defer ad.CookiesLock.RUnlock()
	return ad.DestRegID
}

func (ad *AuthData) pairingID() uuid.UUID {
	ad.CookiesLock.RLock()
	defer ad.CookiesLock.RUnlock()
	return ad.PairingID
}

func (ad *AuthData) SetCookies(cookies map[string]string) {
	ad.CookiesLock.Lock()
	ad.Cookies = cookies
	ad.CookiesLock.Unlock()
}

func (ad *AuthData) AddCookiesToRequest(req *http.Request) {
	ad.CookiesLock.RLock()
	defer ad.CookiesLock.RUnlock()
	if ad.Cookies == nil {
		return
	}
	for name, value := range ad.Cookies {
		req.AddCookie(&http.Cookie{Name: name, Value: value})
	}
	sapisid, ok := ad.Cookies["SAPISID"]
	if ok {
		req.Header.Set("Authorization", SAPISIDHash(util.MessagesBaseURL, sapisid))
	}
}

func (ad *AuthData) UpdateCookiesFromResponse(resp *http.Response) {
	ad.CookiesLock.Lock()
	defer ad.CookiesLock.Unlock()
	if ad.Cookies == nil {
		return
	}
	for _, cookie := range resp.Cookies() {
		ad.Cookies[cookie.Name] = cookie.Value
	}
}

func (ad *AuthData) HasCookies() bool {
	if ad == nil {
		return false
	} else if !ad.IsGoogleAccount() {
		return true
	}
	ad.CookiesLock.RLock()
	defer ad.CookiesLock.RUnlock()
	return ad.Cookies != nil
}

func (ad *AuthData) IsGoogleAccount() bool {
	return ad.destRegID() != uuid.Nil
}

func (ad *AuthData) AuthNetwork() string {
	if ad.IsGoogleAccount() {
		return util.GoogleNetwork
	}
	return ""
}

const RefreshTachyonBuffer = 1 * time.Hour

type Proxy func(*http.Request) (*url.URL, error)
type EventHandler func(evt any)
type EventHandlerWithError func(evt any) error

var ErrClientDisconnecting = errors.New("client is disconnecting")

type clientLifecycle struct {
	ctx    context.Context
	cancel context.CancelFunc
}

type updateDedupItem struct {
	id   string
	hash [32]byte
}

const DefaultBugleDefaultCheckInterval = 2*time.Hour + 55*time.Minute
const minBugleDefaultCheckInterval = 1 * time.Hour

type Client struct {
	Logger          zerolog.Logger
	handlerLock     sync.RWMutex
	evHandler       EventHandler
	errEventHandler EventHandlerWithError
	sessionHandler  *SessionHandler

	lifecycleLock    sync.Mutex
	disconnectLock   sync.Mutex
	connectLock      sync.Mutex
	lifecycle        *clientLifecycle
	lifecycleClosing bool
	callbackLock     sync.RWMutex
	callbacksOpen    bool
	workers          sync.WaitGroup

	pollLock        sync.RWMutex
	longPollingConn io.Closer
	pollCancel      context.CancelFunc
	listenID        uint64
	pollRunning     bool
	disconnecting   bool
	skipCount       atomic.Int64

	settingsLock             sync.RWMutex
	pingInterval             time.Duration
	alertTimeoutCount        int
	pingShortCircuit         chan struct{}
	phone                    phoneLiveness
	dataReceiveCheckInterval time.Duration
	nextDataReceiveCheck     time.Time
	nextDataReceiveCheckLock sync.Mutex
	lastBugleDefaultCheck    time.Time
	bugleDefaultCheckLock    sync.Mutex

	recentUpdates     [8]updateDedupItem
	recentUpdatesPtr  int
	recentUpdatesLock sync.Mutex

	conversationsFetchedOnce bool
	conversationsFetchLock   sync.Mutex

	GaiaHackyDeviceSwitcher int

	PairCallback atomic.Pointer[func(data *gmproto.PairedData)]

	AuthData *AuthData
	PushKeys *PushKeys
	Config   *gmproto.Config

	http   *http.Client
	lphttp *http.Client
}

func NewAuthData() *AuthData {
	return &AuthData{
		RequestCrypto: crypto.NewAESCTRHelper(),
		RefreshKey:    crypto.GenerateECDSAKey(),
	}
}

func NewClient(authData *AuthData, pk *PushKeys, logger zerolog.Logger, httpSettings exhttp.ClientSettings) *Client {
	sessionHandler := &SessionHandler{
		responseWaiters: make(map[string]chan<- *IncomingRPCMessage),
	}
	cli := &Client{
		AuthData:       authData,
		PushKeys:       pk,
		Logger:         logger,
		sessionHandler: sessionHandler,

		http:   httpSettings.Compile(),
		lphttp: httpSettings.WithGlobalTimeout(30 * time.Minute).Compile(),

		pingShortCircuit:         make(chan struct{}),
		pingInterval:             1 * time.Minute,
		alertTimeoutCount:        4,
		dataReceiveCheckInterval: DefaultBugleDefaultCheckInterval,
		callbacksOpen:            true,
	}
	sessionHandler.client = cli
	// A redirected POST would replay the body; surface the 3xx as an error instead.
	cli.http.CheckRedirect = refusePOSTRedirect
	cli.lphttp.CheckRedirect = refusePOSTRedirect
	return cli
}

func refusePOSTRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > 0 && via[0].Method != http.MethodGet {
		return http.ErrUseLastResponse
	}
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	return nil
}

func (c *Client) CurrentSessionID() string {
	return c.sessionHandler.SessionID()
}

// SetEventHandler sets the global event handler for all received data.
// The method is called synchronously and must not make any outgoing requests or otherwise block for too long.
func (c *Client) SetEventHandler(eventHandler EventHandler) {
	c.handlerLock.Lock()
	c.evHandler = eventHandler
	c.errEventHandler = nil
	c.handlerLock.Unlock()
}

// SetEventHandlerWithError installs a synchronous handler that can reject ACK
// admission. An error leaves the incoming RPC unacknowledged for redelivery.
func (c *Client) SetEventHandlerWithError(eventHandler EventHandlerWithError) {
	c.handlerLock.Lock()
	c.evHandler = nil
	c.errEventHandler = eventHandler
	c.handlerLock.Unlock()
}

func (c *Client) SetPingInterval(interval time.Duration) {
	if interval >= 1*time.Minute && interval < 4*time.Hour {
		c.settingsLock.Lock()
		c.pingInterval = interval
		c.settingsLock.Unlock()
	}
}

func (c *Client) SetAlertTimeoutCount(count int) {
	if count > 0 {
		c.settingsLock.Lock()
		c.alertTimeoutCount = count
		c.settingsLock.Unlock()
	}
}

// SetDataReceiveCheckInterval sets how often to send an extra GET_UPDATES call
// (and emit a NoDataReceived event) when no data has been received recently.
// Intervals shorter than 5 minutes are ignored to avoid draining the phone's battery.
func (c *Client) SetDataReceiveCheckInterval(interval time.Duration) {
	if interval >= 5*time.Minute {
		c.settingsLock.Lock()
		c.dataReceiveCheckInterval = interval
		c.settingsLock.Unlock()
	}
}

func (c *Client) lifecycleFor(ctx context.Context) (*clientLifecycle, error) {
	c.lifecycleLock.Lock()
	defer c.lifecycleLock.Unlock()
	if c.lifecycleClosing {
		return nil, ErrClientDisconnecting
	}
	if c.lifecycle == nil {
		lifecycleCtx, cancel := context.WithCancel(ctx)
		c.lifecycle = &clientLifecycle{ctx: lifecycleCtx, cancel: cancel}
		c.callbackLock.Lock()
		c.callbacksOpen = true
		c.callbackLock.Unlock()
	}
	return c.lifecycle, nil
}

func (c *Client) goWorker(lifecycle *clientLifecycle, worker func(context.Context)) bool {
	c.lifecycleLock.Lock()
	if c.lifecycleClosing || c.lifecycle != lifecycle {
		c.lifecycleLock.Unlock()
		return false
	}
	c.workers.Add(1)
	c.lifecycleLock.Unlock()
	go func() {
		defer c.workers.Done()
		worker(lifecycle.ctx)
	}()
	return true
}

func (c *Client) goCurrentWorker(worker func(context.Context)) bool {
	c.lifecycleLock.Lock()
	lifecycle := c.lifecycle
	c.lifecycleLock.Unlock()
	if lifecycle == nil {
		return false
	}
	return c.goWorker(lifecycle, worker)
}

func sleepContext(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func (c *Client) pollSettings() (time.Duration, int) {
	c.settingsLock.RLock()
	defer c.settingsLock.RUnlock()
	return c.pingInterval, c.alertTimeoutCount
}

func (c *Client) dataCheckInterval() time.Duration {
	c.settingsLock.RLock()
	defer c.settingsLock.RUnlock()
	return c.dataReceiveCheckInterval
}

func (c *Client) consumeSkip() bool {
	for skipCount := c.skipCount.Load(); skipCount > 0; skipCount = c.skipCount.Load() {
		if c.skipCount.CompareAndSwap(skipCount, skipCount-1) {
			return true
		}
	}
	return false
}

func (c *Client) checkLoggedIn() error {
	if c.AuthData.TachyonToken() == nil {
		return fmt.Errorf("no auth token")
	} else if _, browser := c.AuthData.devices(); browser == nil {
		return fmt.Errorf("not logged in")
	}
	return nil
}

func (c *Client) pollIsRunning() bool {
	c.pollLock.RLock()
	defer c.pollLock.RUnlock()
	return c.pollRunning
}

func (c *Client) startLongPolling(lifecycle *clientLifecycle, loggedIn, background bool, onFirstConnect func(context.Context)) (<-chan bool, error) {
	c.bumpNextDataReceiveCheck(10 * time.Minute)
	c.pollLock.Lock()
	if c.pollRunning {
		c.pollLock.Unlock()
		return nil, nil
	}
	c.listenID++
	listenID := c.listenID
	pollCtx, pollCancel := context.WithCancel(lifecycle.ctx)
	c.pollCancel = pollCancel
	c.pollRunning = true
	c.disconnecting = false
	c.pollLock.Unlock()

	result := make(chan bool, 1)
	if !c.goWorker(lifecycle, func(context.Context) {
		clean := c.doLongPoll(lifecycle, pollCtx, listenID, loggedIn, background, onFirstConnect)
		result <- clean
		close(result)
	}) {
		c.finishLongPoll(listenID)
		return nil, ErrClientDisconnecting
	}
	return result, nil
}

func (c *Client) Connect(ctx context.Context) error {
	c.connectLock.Lock()
	defer c.connectLock.Unlock()
	if err := c.checkLoggedIn(); err != nil {
		return err
	}
	lifecycle, err := c.lifecycleFor(ctx)
	if err != nil {
		return err
	}
	if c.pollIsRunning() {
		c.sessionHandler.startAckInterval(lifecycle)
		return nil
	}

	// Refresh the auth token here rather than leaving it to the long polling loop, so that
	// callers connecting for the first time (i.e. right after logging in) find out about
	// bad credentials synchronously.
	err = c.refreshAuthToken(lifecycle.ctx, nil)
	if err != nil {
		if isFatalRefreshError(err) {
			return fmt.Errorf("failed to refresh auth token: %w", err)
		}
		c.Logger.Warn().Err(err).Msg("Transient error refreshing auth token on connect, will retry in long polling loop")
	}
	_, err = c.startLongPolling(lifecycle, true, false, c.postConnect)
	if err == nil {
		c.sessionHandler.startAckInterval(lifecycle)
	}
	return err
}

func (c *Client) ConnectBackground(ctx context.Context) error {
	c.connectLock.Lock()
	if err := c.checkLoggedIn(); err != nil {
		c.connectLock.Unlock()
		return err
	}
	lifecycle, err := c.lifecycleFor(ctx)
	if err != nil {
		c.connectLock.Unlock()
		return err
	}
	result, err := c.startLongPolling(lifecycle, true, true, nil)
	c.connectLock.Unlock()
	if err != nil {
		return err
	}
	if result == nil {
		return errors.New("client is already connected")
	}
	cleanExit := <-result
	c.sessionHandler.sendAckRequest(lifecycle.ctx)
	if !cleanExit {
		return fmt.Errorf("polling exited uncleanly")
	}
	return nil
}

func (c *Client) postConnect(ctx context.Context) {
	if !sleepContext(ctx, 2*time.Second) {
		return
	}
	if skipCount := c.skipCount.Load(); skipCount > 0 {
		c.Logger.Warn().Int64("skip_count", skipCount).Msg("Skip count is non-zero in postConnect, waiting longer")
		for i := 0; i < 3 && c.skipCount.Load() > 0; i++ {
			if !sleepContext(ctx, time.Second) {
				return
			}
		}
		if skipCount = c.skipCount.Load(); skipCount > 0 {
			c.Logger.Warn().Int64("skip_count", skipCount).Msg("Skip count is still non-zero")
		}
		_ = c.triggerEvent(&events.HackySetActiveMayFail{})
	}
	ctx = c.Logger.WithContext(ctx)
	c.Logger.Debug().Msg("Sending acks before get updates request")
	c.sessionHandler.sendAckRequest(ctx)
	if !sleepContext(ctx, time.Second) {
		return
	}
	c.Logger.Debug().Msg("Sending get updates request")
	err := c.SetActiveSession(ctx)
	if err != nil {
		c.Logger.Err(err).Msg("Failed to set active session")
		_ = c.triggerEvent(&events.PingFailed{
			Error: fmt.Errorf("failed to set active session: %w", err),
		})
		return
	}
	c.Logger.Debug().Msg("Sent set active session/get updates request")

	if !c.shouldCheckBugleDefault() {
		c.Logger.Debug().Msg("Skipping bugle default check, already checked recently")
		return
	}
	doneChan := make(chan struct{})
	c.goCurrentWorker(func(workerCtx context.Context) {
		select {
		case <-time.After(5 * time.Second):
			c.Logger.Warn().Msg("Checking bugle default on connect is taking long")
			select {
			case c.pingShortCircuit <- struct{}{}:
			default:
			}
		case <-doneChan:
		case <-workerCtx.Done():
		}
	})
	bugleRes, err := c.IsBugleDefault(ctx)
	close(doneChan)
	if err != nil {
		c.Logger.Err(err).Msg("Failed to check bugle default")
		return
	}
	c.Logger.Debug().Bool("bugle_default", bugleRes.Success).Msg("Got is bugle default response on connect")
}

func (c *Client) shouldCheckBugleDefault() bool {
	c.bugleDefaultCheckLock.Lock()
	defer c.bugleDefaultCheckLock.Unlock()
	if time.Since(c.lastBugleDefaultCheck) < minBugleDefaultCheckInterval {
		return false
	}
	c.lastBugleDefaultCheck = time.Now()
	return true
}

func (c *Client) Disconnect() {
	c.disconnectLock.Lock()
	defer c.disconnectLock.Unlock()
	c.lifecycleLock.Lock()
	lifecycle := c.lifecycle
	if lifecycle != nil {
		c.lifecycleClosing = true
		lifecycle.cancel()
	}
	c.lifecycleLock.Unlock()
	c.callbackLock.Lock()
	c.callbacksOpen = false
	c.callbackLock.Unlock()
	c.closeLongPolling()
	// Fail any requests that are still waiting for a response from the phone:
	// the responses are delivered over the long polling connection, so they can
	// never arrive after it has been torn down.
	c.sessionHandler.cancelAllResponseWaiters()
	c.workers.Wait()
	c.http.CloseIdleConnections()
	c.lphttp.CloseIdleConnections()
	c.lifecycleLock.Lock()
	if c.lifecycle == lifecycle {
		c.lifecycle = nil
	}
	c.lifecycleClosing = false
	c.lifecycleLock.Unlock()
}

func (c *Client) IsConnected() bool {
	c.pollLock.RLock()
	defer c.pollLock.RUnlock()
	return c.pollRunning && c.longPollingConn != nil
}

func (c *Client) IsLoggedIn() bool {
	if c == nil || c.AuthData == nil {
		return false
	}
	_, browser := c.AuthData.devices()
	return browser != nil && c.AuthData.HasCookies()
}

func (c *Client) Reconnect(ctx context.Context) error {
	c.connectLock.Lock()
	defer c.connectLock.Unlock()
	c.closeLongPolling()
	err := c.checkLoggedIn()
	if err != nil {
		c.Logger.Err(err).Msg("Failed to reconnect")
		_ = c.triggerEvent(&events.ListenFatalError{Error: fmt.Errorf("failed to reconnect: %w", err)})
		return err
	}
	lifecycle, err := c.lifecycleFor(ctx)
	if err != nil {
		return err
	}
	_, err = c.startLongPolling(lifecycle, true, false, c.postConnect)
	if err != nil {
		return err
	}
	c.sessionHandler.startAckInterval(lifecycle)
	c.Logger.Debug().Msg("Successfully reconnected to server")
	return nil
}

func (c *Client) triggerEvent(evt interface{}) error {
	if !c.beginCallback() {
		return ErrConnectionClosed
	}
	defer c.endCallback()
	c.handlerLock.RLock()
	handler, errHandler := c.evHandler, c.errEventHandler
	c.handlerLock.RUnlock()
	if errHandler != nil {
		return errHandler(evt)
	}
	if handler != nil {
		handler(evt)
	}
	return nil
}

func (c *Client) beginCallback() bool {
	c.callbackLock.RLock()
	if !c.callbacksOpen {
		c.callbackLock.RUnlock()
		return false
	}
	return true
}

func (c *Client) endCallback() {
	c.callbackLock.RUnlock()
}

func (c *Client) FetchConfig(ctx context.Context) error {
	config, err := c.fetchConfig(ctx)
	if err != nil {
		return fmt.Errorf("failed to fetch config: %w", err)
	}
	if deviceID := config.GetDeviceInfo().GetDeviceID(); deviceID != "" && c.AuthData != nil {
		var sessionID uuid.UUID
		sessionID, err = uuid.Parse(deviceID)
		if err != nil {
			c.Logger.Err(err).Str("device_id", deviceID).Msg("Failed to parse device ID")
		} else {
			c.AuthData.setSessionUUID(sessionID)
		}
	}
	c.Config = config
	return nil
}

func (c *Client) fetchConfig(ctx context.Context) (*gmproto.Config, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, util.ConfigURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare request: %w", err)
	}
	util.BuildRelayHeaders(req, "", "*/*")
	req.Header.Set("sec-fetch-site", "same-origin")
	req.Header.Del("x-user-agent")
	req.Header.Del("origin")
	c.AuthData.AddCookiesToRequest(req)

	resp, err := c.http.Do(req)
	if resp != nil {
		c.AuthData.UpdateCookiesFromResponse(resp)
	}
	config, err := typedHTTPResponse[*gmproto.Config](resp, err)
	if err != nil {
		return nil, err
	}

	version, parseErr := config.ParsedClientVersion()
	if parseErr != nil {
		return nil, fmt.Errorf("failed to parse client version: %w", err)
	}

	currVersion := util.ConfigMessage
	if version.Year != currVersion.Year || version.Month != currVersion.Month || version.Day != currVersion.Day {
		toLog := c.diffVersionFormat(currVersion, version)
		c.Logger.Trace().Any("version", toLog).Msg("Messages for web version is not latest")
	} else {
		c.Logger.Debug().Any("version", currVersion).Msg("Using latest messages for web version")
	}

	return config, nil
}

func (c *Client) diffVersionFormat(curr *gmproto.ConfigVersion, latest *gmproto.ConfigVersion) string {
	return fmt.Sprintf("%d.%d.%d -> %d.%d.%d", curr.Year, curr.Month, curr.Day, latest.Year, latest.Month, latest.Day)
}

func (c *Client) updateTachyonAuthToken(data *gmproto.TokenData) {
	validForDuration := time.Duration(data.GetTTL()) * time.Microsecond
	if validForDuration == 0 {
		validForDuration = 24 * time.Hour
	}
	expiry := time.Now().UTC().Add(validForDuration)
	c.AuthData.CookiesLock.Lock()
	c.AuthData.TachyonAuthToken = data.GetTachyonAuthToken()
	c.AuthData.TachyonExpiry = expiry
	c.AuthData.TachyonTTL = validForDuration.Microseconds()
	c.AuthData.CookiesLock.Unlock()
	c.Logger.Debug().
		Time("tachyon_expiry", expiry).
		Int64("valid_for", data.GetTTL()).
		Msg("Updated tachyon token")
}

type PushKeys struct {
	URL    string
	P256DH []byte
	Auth   []byte
}

func (c *Client) RegisterPush(ctx context.Context, keys *PushKeys) error {
	if c.PushKeys == nil || c.PushKeys.URL != keys.URL {
		err := c.refreshAuthToken(ctx, keys)
		if err != nil {
			return fmt.Errorf("failed to refresh auth token: %w", err)
		}
	}
	err := c.UpdateSettings(ctx, &gmproto.SettingsUpdateRequest{
		PushSettings: &gmproto.SettingsUpdateRequest_PushSettings{
			Enabled: true,
		},
	})
	if err != nil {
		return fmt.Errorf("failed to update settings to enable push: %w", err)
	}
	c.PushKeys = keys
	return nil
}

func isFatalRefreshError(err error) bool {
	if errors.Is(err, events.ErrInvalidCredentials) || errors.Is(err, events.ErrRequestedEntityNotFound) {
		return true
	}
	var httpErr events.HTTPError
	if errors.As(err, &httpErr) {
		switch httpErr.Resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
			return true
		}
	}
	return false
}

func (c *Client) refreshAuthToken(ctx context.Context, pushKeyOverride *PushKeys) error {
	_, browser := c.AuthData.devices()
	if browser == nil || (time.Until(c.AuthData.tachyonExpiry()) > RefreshTachyonBuffer && pushKeyOverride == nil) {
		return nil
	}
	jwk := c.AuthData.RefreshKey
	requestID := uuid.NewString()
	timestamp := time.Now().UnixMilli() * 1000

	signBytes := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", requestID, timestamp)))
	privKey, err := jwk.GetPrivateKey()
	if err != nil {
		return err
	}
	sig, err := ecdsa.SignASN1(rand.Reader, privKey, signBytes[:])
	if err != nil {
		return err
	}

	var moreParams *gmproto.RegisterRefreshRequest_MoreParameters
	keys := c.PushKeys
	if pushKeyOverride != nil {
		keys = pushKeyOverride
	}
	if keys != nil {
		moreParams = &gmproto.RegisterRefreshRequest_MoreParameters{
			Three: 3,
		}
		moreParams.PushReg = &gmproto.RegisterRefreshRequest_PushRegistration{
			Type:   "messages_web",
			Url:    keys.URL,
			P256Dh: base64.RawURLEncoding.EncodeToString(keys.P256DH),
			Auth:   base64.RawURLEncoding.EncodeToString(keys.Auth),
		}
	}
	c.Logger.Debug().
		Time("tachyon_expiry", c.AuthData.tachyonExpiry()).
		Bool("force_refresh", pushKeyOverride != nil).
		Bool("include_push_keys", moreParams.GetPushReg() != nil).
		Msg("Refreshing auth token")

	payload := &gmproto.RegisterRefreshRequest{
		MessageAuth: &gmproto.AuthMessage{
			RequestID:        requestID,
			TachyonAuthToken: c.AuthData.TachyonToken(),
			Network:          c.AuthData.AuthNetwork(),
			ConfigVersion:    util.ConfigMessage,
		},
		CurrBrowserDevice: browser,
		UnixTimestamp:     timestamp,
		Signature:         sig,
		Parameters: &gmproto.RegisterRefreshRequest_Parameters{
			EmptyArr:       &gmproto.EmptyArr{},
			MoreParameters: moreParams,
		},
		MessageType: 2, // hmm
	}

	resp, err := typedHTTPResponse[*gmproto.RegisterRefreshResponse](
		c.makeProtobufHTTPRequestContext(ctx, util.RegisterRefreshURL, payload, ContentTypePBLite, false, false),
	)
	if err != nil {
		return err
	}

	if resp.GetTokenData().GetTachyonAuthToken() == nil {
		return fmt.Errorf("no tachyon auth token in refresh response")
	}

	c.updateTachyonAuthToken(resp.GetTokenData())
	c.triggerEvent(&events.AuthTokenRefreshed{})
	return nil
}
