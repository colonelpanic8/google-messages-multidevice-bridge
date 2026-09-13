package libgm

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/events"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/util"
)

func (c *Client) StartLogin(ctx context.Context) (string, error) {
	lifecycle, err := c.lifecycleFor(ctx)
	if err != nil {
		return "", err
	}
	registered, err := c.RegisterPhoneRelayContext(lifecycle.ctx)
	if err != nil {
		return "", err
	}
	c.updateTachyonAuthToken(registered.GetAuthKeyData())
	c.closeLongPolling()
	if _, err = c.startLongPolling(lifecycle, false, false, nil); err != nil {
		return "", err
	}
	qr, err := c.GenerateQRCodeData(registered.GetPairingKey())
	if err != nil {
		return "", fmt.Errorf("failed to generate QR code: %w", err)
	}
	return qr, nil
}

func (c *Client) GenerateQRCodeData(pairingKey []byte) (string, error) {
	aesKey, hmacKey := c.AuthData.requestCrypto().Keys()
	urlData := &gmproto.URLData{
		PairingKey: pairingKey,
		AESKey:     aesKey,
		HMACKey:    hmacKey,
	}
	encodedURLData, err := proto.Marshal(urlData)
	if err != nil {
		return "", err
	}
	cData := base64.StdEncoding.EncodeToString(encodedURLData)
	return util.QRCodeURLBase + cData, nil
}

func (c *Client) handlePairingEvent(msg *IncomingRPCMessage) error {
	switch evt := msg.Pair.Event.(type) {
	case *gmproto.RPCPairData_Paired:
		return c.completePairing(evt.Paired)
	case *gmproto.RPCPairData_Revoked:
		return c.triggerEvent(evt.Revoked)
	default:
		c.Logger.Debug().Any("evt", evt).Msg("Unknown pair event type")
		return nil
	}
}

func (c *Client) completePairing(data *gmproto.PairedData) error {
	c.updateTachyonAuthToken(data.GetTokenData())
	c.AuthData.setDevices(data.Mobile, data.Browser)

	if cb := c.PairCallback.Load(); cb != nil {
		if !c.beginCallback() {
			return ErrConnectionClosed
		}
		func() {
			defer c.endCallback()
			(*cb)(data)
		}()
	} else {
		if err := c.triggerEvent(&events.PairSuccessful{PhoneID: data.GetMobile().GetSourceID(), QRData: data}); err != nil {
			return err
		}

		c.goCurrentWorker(func(ctx context.Context) {
			// Sleep for a bit to let the phone save the pair data. If we reconnect too quickly,
			// the phone won't recognize the session the bridge will get unpaired.
			if !sleepContext(ctx, 2*time.Second) {
				return
			}

			err := c.Reconnect(ctx)
			if err != nil {
				c.Logger.Err(err).Msg("Failed to reconnect after pair success")
			}
		})
	}
	return nil
}

func (c *Client) RegisterPhoneRelay() (*gmproto.RegisterPhoneRelayResponse, error) {
	return c.RegisterPhoneRelayContext(context.Background())
}

func (c *Client) RegisterPhoneRelayContext(ctx context.Context) (*gmproto.RegisterPhoneRelayResponse, error) {
	pubKey, err := c.AuthData.RefreshKey.GetPublicKey()
	if err != nil {
		return nil, err
	}
	key, err := x509.MarshalPKIXPublicKey(pubKey)
	if err != nil {
		return nil, err
	}

	payload := &gmproto.AuthenticationContainer{
		AuthMessage: &gmproto.AuthMessage{
			RequestID:     uuid.NewString(),
			Network:       util.QRNetwork,
			ConfigVersion: util.ConfigMessage,
		},
		BrowserDetails: util.BrowserDetailsMessage,
		Data: &gmproto.AuthenticationContainer_KeyData{
			KeyData: &gmproto.KeyData{
				EcdsaKeys: &gmproto.ECDSAKeys{
					Field1:        2,
					EncryptedKeys: key,
				},
			},
		},
	}
	return typedHTTPResponse[*gmproto.RegisterPhoneRelayResponse](
		c.makeProtobufHTTPRequestContext(ctx, util.RegisterPhoneRelayURL, payload, ContentTypeProtobuf, false, false),
	)
}

func (c *Client) RefreshPhoneRelay() (string, error) {
	return c.RefreshPhoneRelayContext(context.Background())
}

func (c *Client) RefreshPhoneRelayContext(ctx context.Context) (string, error) {
	payload := &gmproto.AuthenticationContainer{
		AuthMessage: &gmproto.AuthMessage{
			RequestID:        uuid.NewString(),
			Network:          util.QRNetwork,
			TachyonAuthToken: c.AuthData.TachyonToken(),
			ConfigVersion:    util.ConfigMessage,
		},
	}
	res, err := typedHTTPResponse[*gmproto.RefreshPhoneRelayResponse](
		c.makeProtobufHTTPRequestContext(ctx, util.RefreshPhoneRelayURL, payload, ContentTypeProtobuf, false, false),
	)
	if err != nil {
		return "", err
	}
	qr, err := c.GenerateQRCodeData(res.GetPairKey())
	if err != nil {
		return "", err
	}
	return qr, nil
}

func (c *Client) GetWebEncryptionKey() (*gmproto.WebEncryptionKeyResponse, error) {
	return c.GetWebEncryptionKeyContext(context.Background())
}

func (c *Client) GetWebEncryptionKeyContext(ctx context.Context) (*gmproto.WebEncryptionKeyResponse, error) {
	payload := &gmproto.AuthenticationContainer{
		AuthMessage: &gmproto.AuthMessage{
			RequestID:        uuid.NewString(),
			TachyonAuthToken: c.AuthData.TachyonToken(),
			ConfigVersion:    util.ConfigMessage,
		},
	}
	return typedHTTPResponse[*gmproto.WebEncryptionKeyResponse](
		c.makeProtobufHTTPRequestContext(ctx, util.GetWebEncryptionKeyURL, payload, ContentTypeProtobuf, false, false),
	)
}

func (c *Client) UnpairBugle() (*gmproto.RevokeRelayPairingResponse, error) {
	return c.UnpairBugleContext(context.Background())
}

func (c *Client) UnpairBugleContext(ctx context.Context) (*gmproto.RevokeRelayPairingResponse, error) {
	_, browser := c.AuthData.devices()
	if c.AuthData.TachyonToken() == nil || browser == nil {
		return nil, nil
	}
	payload := &gmproto.RevokeRelayPairingRequest{
		AuthMessage: &gmproto.AuthMessage{
			RequestID:        uuid.NewString(),
			TachyonAuthToken: c.AuthData.TachyonToken(),
			ConfigVersion:    util.ConfigMessage,
		},
		Browser: browser,
	}
	return typedHTTPResponse[*gmproto.RevokeRelayPairingResponse](
		c.makeProtobufHTTPRequestContext(ctx, util.RevokeRelayPairingURL, payload, ContentTypeProtobuf, false, false),
	)
}

func (c *Client) Unpair(ctx context.Context) (err error) {
	if c.AuthData.HasCookies() {
		err = c.UnpairGaia(ctx)
	} else {
		_, err = c.UnpairBugleContext(ctx)
	}
	return
}
