package notify

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// The APNs adapter (HTTP/2, token-based authentication,
// https://developer.apple.com/documentation/usernotifications/sending-notification-requests-to-apns).
//
// A channel is one Apple Push Notification service key: the .p8 private key plus
// its key id and team id, and the bundle id used as the apns-topic. A target
// address is one device token, as hex, exactly as the app received it.
//
//	secret                the .p8 private key (PEM, PKCS#8 or SEC1 EC key)
//	endpoint              API base override; defaults from environment
//	settings["key_id"]    the key's id (required)
//	settings["team_id"]   the developer team id (required)
//	settings["topic"]     the app bundle id (required)
//	settings["environment"] production (default) | sandbox
//	settings["sound"]     alert sound, default "default"
//
// APNs speaks HTTP/2 only, so the client must negotiate h2; NewHTTPClient sets
// ForceAttemptHTTP2 for exactly this adapter. The provider token is signed locally
// and reused for up to an hour, which is what Apple asks for: more than one
// signature per key per second is itself rate limited.

const (
	defaultAPNSProduction = "https://api.push.apple.com"
	defaultAPNSSandbox    = "https://api.sandbox.push.apple.com"

	SettingAPNSKeyID       = "key_id"
	SettingAPNSTeamID      = "team_id"
	SettingAPNSTopic       = "topic"
	SettingAPNSEnvironment = "environment"
	SettingAPNSSound       = "sound"

	APNSEnvironmentProduction = "production"
	APNSEnvironmentSandbox    = "sandbox"

	// apnsTokenTTL is how long a signed provider token is reused. Apple allows an
	// hour and rate limits signing; 50 minutes leaves room for clock skew.
	apnsTokenTTL = 50 * time.Minute
)

type apnsSender struct {
	client  *http.Client
	timeout time.Duration
	base    string
	topic   string
	sound   string
	keyID   string
	teamID  string
	key     *ecdsa.PrivateKey

	mu          sync.Mutex
	signed      string
	signedUntil time.Time
}

func newAPNS(spec Spec) (Sender, error) {
	keyID := spec.setting(SettingAPNSKeyID)
	teamID := spec.setting(SettingAPNSTeamID)
	topic := spec.setting(SettingAPNSTopic)
	if keyID == "" || teamID == "" || topic == "" {
		return nil, errors.New("apns: key_id, team_id and topic are all required")
	}
	environment := spec.setting(SettingAPNSEnvironment)
	switch environment {
	case "", APNSEnvironmentProduction:
		environment = APNSEnvironmentProduction
	case APNSEnvironmentSandbox:
	default:
		return nil, fmt.Errorf("apns: %q is not one of production, sandbox", environment)
	}
	base := strings.TrimRight(strings.TrimSpace(spec.Endpoint), "/")
	if base == "" {
		base = defaultAPNSProduction
		if environment == APNSEnvironmentSandbox {
			base = defaultAPNSSandbox
		}
	}
	parsed, err := url.Parse(base)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("apns: %q is not an http(s) API base", base)
	}
	key, err := parseECPrivateKey(strings.TrimSpace(spec.Secret))
	if err != nil {
		return nil, err
	}
	sound := spec.setting(SettingAPNSSound)
	if sound == "" {
		sound = "default"
	}
	timeout := spec.timeout()
	return &apnsSender{
		client: spec.client(timeout), timeout: timeout, base: base, topic: topic, sound: sound,
		keyID: keyID, teamID: teamID, key: key,
	}, nil
}

func (a *apnsSender) Kind() string { return KindAPNS }

// Verify proves the credentials without touching a real device: it posts to a
// device token that cannot exist. APNs authenticates before it looks at the token,
// so BadDeviceToken means the key, team and topic were accepted, while
// InvalidProviderToken means they were not.
func (a *apnsSender) Verify(ctx context.Context) error {
	_, err := a.push(ctx, strings.Repeat("0", 64), Message{Topic: "notification.verify", Title: "FastTask", Body: "APNs credential check"})
	switch {
	case err == nil:
		return errors.New("apns: the check token was accepted, which should be impossible")
	case errors.Is(err, ErrInvalidTarget):
		return nil
	default:
		return err
	}
}

func (a *apnsSender) Send(ctx context.Context, address string, msg Message) (Receipt, error) {
	token := strings.TrimSpace(address)
	if token == "" {
		return Receipt{}, ErrNoAddress
	}
	return a.push(ctx, token, msg)
}

func (a *apnsSender) push(ctx context.Context, deviceToken string, msg Message) (Receipt, error) {
	providerToken, err := a.providerToken()
	if err != nil {
		return Receipt{}, err
	}
	payload, err := json.Marshal(apnsPayload(a.sound, msg))
	if err != nil {
		return Receipt{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+"/3/device/"+url.PathEscape(deviceToken), bytes.NewReader(payload))
	if err != nil {
		return Receipt{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "bearer "+providerToken)
	request.Header.Set("apns-topic", a.topic)
	request.Header.Set("apns-push-type", "alert")
	request.Header.Set("apns-priority", "10")
	if msg.CollapseID != "" {
		request.Header.Set("apns-collapse-id", msg.CollapseID)
	}
	var body struct {
		Reason    string `json:"reason"`
		Status    int    `json:"status"`
		Timestamp int64  `json:"timestamp"`
	}
	response, err := exchange(a.client, request, &body)
	if err != nil {
		return Receipt{}, err
	}
	if response.Status != http.StatusOK {
		if containsAny(body.Reason, "providertoken") {
			// The signed token was refused, so the cached one is worthless: drop it and
			// let the next attempt sign again.
			a.forgetToken()
		}
		return Receipt{}, classifyAPNS(response.Status, body.Reason)
	}
	return Receipt{ProviderID: response.Header.Get("apns-id")}, nil
}

// apnsPayload builds the notification body. Everything FastTask adds sits under one
// `fasttask` key so an app extension can read metadata without knowing the shape of
// `aps`, and so a key a caller puts in Data cannot shadow the alert.
//
// The alert always carries text: APNs accepts a payload whose alert has no title and
// no body and then shows nothing, so a caller that supplied only a topic still gets a
// readable banner. The bundle id is not used as that text — it is the routing topic,
// not something a user should read.
func apnsPayload(sound string, msg Message) map[string]any {
	alert := map[string]string{}
	if title := strings.TrimSpace(msg.Title); title != "" {
		alert["title"] = title
	}
	body := strings.TrimSpace(msg.Body)
	if body == "" {
		body = strings.TrimSpace(msg.Topic)
	}
	if body == "" {
		body = "FastTask"
	}
	alert["body"] = body
	aps := map[string]any{"alert": alert}
	if sound != "" {
		aps["sound"] = sound
	}
	payload := map[string]any{"aps": aps}
	extra := map[string]string{}
	if msg.Topic != "" {
		extra["topic"] = msg.Topic
	}
	if msg.URL != "" {
		extra["url"] = msg.URL
	}
	for key, value := range msg.Data {
		extra[key] = value
	}
	if len(extra) > 0 {
		payload["fasttask"] = extra
	}
	return payload
}

// providerToken signs the ES256 JWT APNs authenticates with, reusing it until it is
// close to the hour Apple allows.
func (a *apnsSender) providerToken() (string, error) {
	a.mu.Lock()
	if a.signed != "" && time.Now().Before(a.signedUntil) {
		token := a.signed
		a.mu.Unlock()
		return token, nil
	}
	a.mu.Unlock()

	token := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{"iss": a.teamID, "iat": time.Now().Unix()})
	token.Header["kid"] = a.keyID
	signed, err := token.SignedString(a.key)
	if err != nil {
		return "", fmt.Errorf("apns: the .p8 key could not sign a provider token: %w", err)
	}
	a.mu.Lock()
	a.signed, a.signedUntil = signed, time.Now().Add(apnsTokenTTL)
	a.mu.Unlock()
	return signed, nil
}

func (a *apnsSender) forgetToken() {
	a.mu.Lock()
	a.signed, a.signedUntil = "", time.Time{}
	a.mu.Unlock()
}

// classifyAPNS maps the documented reasons to the three outcomes a dispatcher acts
// on differently: a dead device token, a fault in the channel's own credentials or
// payload, and anything worth another attempt.
func classifyAPNS(status int, reason string) error {
	switch reason {
	case "BadDeviceToken", "Unregistered", "DeviceTokenNotForTopic", "DeviceTokenNotActive":
		return fmt.Errorf("%w: apns refused the device token (%s)", ErrInvalidTarget, reason)
	case "InvalidProviderToken", "ExpiredProviderToken", "MissingProviderToken", "InvalidProviderTokenSignature":
		return fmt.Errorf("%w: apns rejected the .p8 credentials (%s)", ErrUndeliverable, reason)
	case "TopicDisallowed", "BadMessageId", "DuplicateHeaders":
		return fmt.Errorf("%w: apns rejected the request (%s)", ErrUndeliverable, reason)
	case "PayloadTooLarge":
		return fmt.Errorf("%w: apns refused the payload size (%s)", ErrUndeliverable, reason)
	case "Shutdown", "InternalServiceError", "ServiceUnavailable":
		return providerError(KindAPNS, status, reason)
	}
	if status == http.StatusGone {
		return fmt.Errorf("%w: apns reports the device token is unregistered", ErrInvalidTarget)
	}
	return providerError(KindAPNS, status, reason)
}

// parseECPrivateKey reads the .p8 Apple issues. Those are PKCS#8, but a key that
// travelled through openssl often arrives as SEC1, and both are one ECDSA key.
func parseECPrivateKey(pemBlock string) (*ecdsa.PrivateKey, error) {
	if pemBlock == "" {
		return nil, errors.New("apns: the .p8 private key is required")
	}
	block, _ := pem.Decode([]byte(pemBlock))
	if block == nil {
		return nil, errors.New("apns: the private key is not a PEM block")
	}
	var (
		key any
		err error
	)
	if key, err = x509.ParsePKCS8PrivateKey(block.Bytes); err != nil {
		if key, err = x509.ParseECPrivateKey(block.Bytes); err != nil {
			return nil, errors.New("apns: the private key is neither PKCS#8 nor an EC key")
		}
	}
	ecdsaKey, ok := key.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("apns: the private key is not an ECDSA key")
	}
	return ecdsaKey, nil
}
