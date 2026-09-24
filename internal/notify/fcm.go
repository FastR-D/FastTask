package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/jwt"
)

// The Firebase Cloud Messaging adapter, HTTP v1 API
// (https://firebase.google.com/docs/reference/fcm/rest/v1/projects.messages/send).
//
// A channel is one Firebase project: its secret is the service-account JSON
// downloaded from the Firebase console, and a target address is one registration
// token from an app instance. The legacy server key API is not supported — Google
// turned it off, and v1 authenticates with a short-lived OAuth token that this
// adapter obtains and caches per channel.
//
//	secret                 service-account JSON (required)
//	endpoint               API base, default https://fcm.googleapis.com
//	settings["project_id"] override; defaults to the JSON's project_id
//
// Verify proves the service account can authenticate. It cannot prove a token is
// reachable, because that is per-device and FCM offers no dry run without one.

const (
	defaultFCMAPI      = "https://fcm.googleapis.com"
	defaultFCMTokenURL = "https://oauth2.googleapis.com/token"
	fcmScope           = "https://www.googleapis.com/auth/firebase.messaging"
	// settingFCMProject overrides the project id read from the service-account key.
	settingFCMProject = "project_id"
	// tokenRefreshSkew is how long before expiry a cached access token is replaced.
	tokenRefreshSkew = 30 * time.Second
)

type fcmSender struct {
	client    *http.Client
	timeout   time.Duration
	base      string
	projectID string
	config    *jwt.Config

	// The access token is cached here rather than left to a long-lived
	// oauth2.TokenSource so that the exchange is bounded by the caller's context and
	// uses the injected client. Google issues these for an hour; refreshing 30 seconds
	// early avoids a token that expires mid-request.
	mu     sync.Mutex
	cached *oauth2.Token
}

// serviceAccount is the key file Firebase hands out. It is read here rather than
// through the google subpackage of oauth2 because that package drags in the Google
// Cloud metadata client, and the only thing needed from it is a JWT the oauth2/jwt
// package can already sign.
type serviceAccount struct {
	Type         string `json:"type"`
	ProjectID    string `json:"project_id"`
	PrivateKeyID string `json:"private_key_id"`
	PrivateKey   string `json:"private_key"`
	ClientEmail  string `json:"client_email"`
	TokenURL     string `json:"token_url"`
}

func newFCM(spec Spec) (Sender, error) {
	raw := strings.TrimSpace(spec.Secret)
	if raw == "" {
		return nil, errors.New("fcm: a service-account JSON key is required")
	}
	var account serviceAccount
	if err := json.Unmarshal([]byte(raw), &account); err != nil {
		return nil, errors.New("fcm: the service-account key is not valid JSON")
	}
	if account.Type != "" && account.Type != "service_account" {
		return nil, fmt.Errorf("fcm: the key file is a %q, not a service account", account.Type)
	}
	if strings.TrimSpace(account.PrivateKey) == "" || strings.TrimSpace(account.ClientEmail) == "" {
		return nil, errors.New("fcm: the service-account key carries no private_key or client_email")
	}
	tokenURL := strings.TrimSpace(account.TokenURL)
	if tokenURL == "" {
		tokenURL = defaultFCMTokenURL
	}
	config := &jwt.Config{
		Email: account.ClientEmail, PrivateKey: []byte(account.PrivateKey), PrivateKeyID: account.PrivateKeyID,
		TokenURL: tokenURL, Scopes: []string{fcmScope},
	}
	projectID := spec.setting(settingFCMProject)
	if projectID == "" {
		projectID = strings.TrimSpace(account.ProjectID)
	}
	if projectID == "" {
		return nil, errors.New("fcm: no project id, and the service-account key carries none")
	}
	base := strings.TrimSpace(spec.Endpoint)
	if base == "" {
		base = defaultFCMAPI
	}
	base = strings.TrimRight(base, "/")
	parsed, err := url.Parse(base)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("fcm: %q is not an http(s) API base", base)
	}
	timeout := spec.timeout()
	return &fcmSender{
		client: spec.client(timeout), timeout: timeout, base: base, projectID: projectID, config: config,
	}, nil
}

func (f *fcmSender) Kind() string { return KindFCM }

func (f *fcmSender) Verify(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()
	if _, err := f.token(ctx); err != nil {
		return fmt.Errorf("fcm: the service account could not authenticate: %w", redactGoogleError(err))
	}
	return nil
}

func (f *fcmSender) Send(ctx context.Context, address string, msg Message) (Receipt, error) {
	token := strings.TrimSpace(address)
	if token == "" {
		return Receipt{}, ErrNoAddress
	}
	access, err := f.token(ctx)
	if err != nil {
		return Receipt{}, fmt.Errorf("fcm: %w", redactGoogleError(err))
	}
	payload := map[string]any{
		"token":        token,
		"notification": map[string]string{"title": strings.TrimSpace(msg.Title), "body": strings.TrimSpace(msg.Body)},
	}
	if len(msg.Data) > 0 {
		data := make(map[string]string, len(msg.Data))
		for key, value := range msg.Data {
			data[key] = value
		}
		if msg.Topic != "" {
			data["topic"] = msg.Topic
		}
		payload["data"] = data
	}
	android := map[string]any{"priority": "high"}
	if msg.CollapseID != "" {
		android["collapse_key"] = msg.CollapseID
	}
	payload["android"] = android
	if msg.URL != "" {
		payload["fcm_options"] = map[string]string{"link": msg.URL}
	}
	encoded, err := json.Marshal(map[string]any{"validate_only": false, "message": payload})
	if err != nil {
		return Receipt{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, f.base+"/v1/projects/"+url.PathEscape(f.projectID)+"/messages:send", bytes.NewReader(encoded))
	if err != nil {
		return Receipt{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+access)
	var body struct {
		Name  string   `json:"name"`
		Error fcmError `json:"error"`
	}
	response, err := exchange(f.client, request, &body)
	if err != nil {
		return Receipt{}, err
	}
	if response.Status != http.StatusOK {
		return Receipt{}, classifyFCM(response.Status, body.Error)
	}
	return Receipt{ProviderID: body.Name}, nil
}

// token returns a valid access token, exchanging the service-account key for one
// only when the cached token is missing or about to expire. The exchange is bounded
// by ctx and travels on the adapter's own client, so a test that points the token URL
// at a local listener never reaches Google.
func (f *fcmSender) token(ctx context.Context) (string, error) {
	f.mu.Lock()
	cached := f.cached
	f.mu.Unlock()
	if cached != nil && cached.AccessToken != "" && time.Until(cached.Expiry) > tokenRefreshSkew {
		return cached.AccessToken, nil
	}
	ctx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()
	ctx = context.WithValue(ctx, oauth2.HTTPClient, f.client)
	token, err := f.config.TokenSource(ctx).Token()
	if err != nil {
		return "", err
	}
	f.mu.Lock()
	f.cached = token
	f.mu.Unlock()
	return token.AccessToken, nil
}

type fcmError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
}

// classifyFCM separates a dead registration token from everything else. FCM
// reports an unknown token as NOT_FOUND ("Requested entity was not found") or as
// INVALID_ARGUMENT naming the token; both mean retrying cannot help, so the target
// is retired. UNAUTHENTICATED and PERMISSION_DENIED are the channel's fault and are
// reported as such, without the token in the message.
func classifyFCM(status int, body fcmError) error {
	detail := strings.TrimSpace(body.Message)
	if detail == "" {
		detail = body.Status
	}
	switch {
	case body.Status == "NOT_FOUND" || containsAny(detail, "not a registered token", "requested entity was not found"):
		return fmt.Errorf("%w: fcm does not know this registration token", ErrInvalidTarget)
	case body.Status == "INVALID_ARGUMENT" && containsAny(detail, "token"):
		return fmt.Errorf("%w: fcm refused the registration token: %s", ErrInvalidTarget, detail)
	case body.Status == "UNAUTHENTICATED", body.Status == "PERMISSION_DENIED":
		return fmt.Errorf("%w: the service account is not authorized for this project", ErrUndeliverable)
	}
	code := body.Code
	if code == 0 {
		code = status
	}
	return providerError(KindFCM, code, detail)
}

// redactGoogleError keeps a credential out of an oauth2 failure. The package wraps
// the token endpoint's response, which for a service account can echo the signed
// assertion; only the error's own text is kept, truncated.
func redactGoogleError(err error) error {
	if err == nil {
		return nil
	}
	text := err.Error()
	if idx := strings.Index(text, "private_key"); idx >= 0 {
		text = text[:idx] + "private_key=<redacted>"
	}
	if len(text) > 240 {
		text = text[:240] + "…"
	}
	return errors.New(text)
}
