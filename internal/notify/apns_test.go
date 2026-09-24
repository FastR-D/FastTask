package notify

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"
)

// testP8Key returns a real ECDSA P-256 key in the PEM shape Apple's .p8 file uses.
func testP8Key(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

// apnsCall records one request to the fake APNs endpoint.
type apnsCall struct {
	Proto         string
	Path          string
	Authorization string
	Topic         string
	PushType      string
	Priority      string
	CollapseID    string
	Body          map[string]any
}

// fakeAPNS serves an HTTP/2 TLS listener, because APNs speaks nothing else and an
// adapter that quietly fell back to HTTP/1.1 would pass every test and deliver
// nothing in production.
func fakeAPNS(t *testing.T, calls *[]apnsCall, answer func(path string) (int, string, string)) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		*calls = append(*calls, apnsCall{
			Proto: r.Proto, Path: r.URL.Path, Authorization: r.Header.Get("Authorization"),
			Topic: r.Header.Get("apns-topic"), PushType: r.Header.Get("apns-push-type"),
			Priority: r.Header.Get("apns-priority"), CollapseID: r.Header.Get("apns-collapse-id"), Body: body,
		})
		status, reason, id := answer(r.URL.Path)
		if id != "" {
			w.Header().Set("apns-id", id)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if reason != "" {
			_ = json.NewEncoder(w).Encode(map[string]any{"reason": reason, "status": status})
		}
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

func apnsSpec(server *httptest.Server, extra map[string]string) Spec {
	settings := map[string]string{
		SettingAPNSKeyID: "KEYID12345", SettingAPNSTeamID: "TEAMID67890", SettingAPNSTopic: "com.fasttask.app",
	}
	for key, value := range extra {
		settings[key] = value
	}
	return Spec{Kind: KindAPNS, Endpoint: server.URL, Settings: settings, Client: server.Client()}
}

func TestAPNSSendSpeaksHTTP2AndSignsAProviderToken(t *testing.T) {
	var calls []apnsCall
	server := fakeAPNS(t, &calls, func(string) (int, string, string) {
		return http.StatusOK, "", "1d1b1c1e-2f3a-4b5c-8d9e-0f1a2b3c4d5e"
	})
	spec := apnsSpec(server, nil)
	spec.Secret = testP8Key(t)
	sender, err := NewSender(spec)
	if err != nil {
		t.Fatal(err)
	}

	receipt, err := sender.Send(context.Background(), strings.Repeat("a", 64), Message{
		Topic: "proposal.pending", Title: "有待确认的提案", Body: "拆解《论文初稿》",
		URL: "https://fasttask.example/goals/g1", CollapseID: "proposal-1", Data: map[string]string{"goal_id": "g1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.ProviderID != "1d1b1c1e-2f3a-4b5c-8d9e-0f1a2b3c4d5e" {
		t.Fatalf("receipt=%+v", receipt)
	}
	if len(calls) != 1 {
		t.Fatalf("calls=%+v", calls)
	}
	call := calls[0]
	if call.Proto != "HTTP/2.0" {
		t.Fatalf("proto=%s, APNs serves HTTP/2 only", call.Proto)
	}
	if call.Path != "/3/device/"+strings.Repeat("a", 64) {
		t.Fatalf("path=%s", call.Path)
	}
	if call.Topic != "com.fasttask.app" || call.PushType != "alert" || call.Priority != "10" || call.CollapseID != "proposal-1" {
		t.Fatalf("headers=%+v", call)
	}

	// The bearer must be a real ES256 JWT carrying the team id and the key id: that
	// pair is what APNs authenticates, and a wrong kid is a silent 403 in production.
	raw, ok := strings.CutPrefix(call.Authorization, "bearer ")
	if !ok {
		t.Fatalf("authorization=%q", call.Authorization)
	}
	claims := jwt.MapClaims{}
	token, _, err := jwt.NewParser().ParseUnverified(raw, claims)
	if err != nil {
		t.Fatalf("the provider token is not a JWT: %v", err)
	}
	if token.Method.Alg() != "ES256" {
		t.Fatalf("alg=%s", token.Method.Alg())
	}
	if token.Header["kid"] != "KEYID12345" {
		t.Fatalf("kid=%v", token.Header["kid"])
	}
	if claims["iss"] != "TEAMID67890" {
		t.Fatalf("iss=%v", claims["iss"])
	}

	aps, _ := call.Body["aps"].(map[string]any)
	alert, _ := aps["alert"].(map[string]any)
	if alert["title"] != "有待确认的提案" || alert["body"] != "拆解《论文初稿》" {
		t.Fatalf("alert=%+v", alert)
	}
	if aps["sound"] != "default" {
		t.Fatalf("aps=%+v", aps)
	}
	extra, _ := call.Body["fasttask"].(map[string]any)
	if extra["topic"] != "proposal.pending" || extra["url"] != "https://fasttask.example/goals/g1" || extra["goal_id"] != "g1" {
		t.Fatalf("fasttask=%+v", extra)
	}
}

func TestAPNSReusesTheProviderToken(t *testing.T) {
	var calls []apnsCall
	server := fakeAPNS(t, &calls, func(string) (int, string, string) { return http.StatusOK, "", "id" })
	spec := apnsSpec(server, nil)
	spec.Secret = testP8Key(t)
	sender, err := NewSender(spec)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := sender.Send(context.Background(), strings.Repeat("b", 64), Message{Title: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	if len(calls) != 3 {
		t.Fatalf("calls=%d", len(calls))
	}
	// The iat claim is in seconds, so three sends inside one second produce an
	// identical JWT only if the token was reused rather than re-signed.
	if calls[0].Authorization != calls[1].Authorization || calls[1].Authorization != calls[2].Authorization {
		t.Fatal("a new provider token was signed per message; Apple rate limits that")
	}
}

func TestAPNSSendClassifiesADeviceToken(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		status int
		reason string
	}{
		{"unregistered", http.StatusGone, "Unregistered"},
		{"bad device token", http.StatusBadRequest, "BadDeviceToken"},
		{"token not for topic", http.StatusBadRequest, "DeviceTokenNotForTopic"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var calls []apnsCall
			server := fakeAPNS(t, &calls, func(string) (int, string, string) {
				return testCase.status, testCase.reason, ""
			})
			spec := apnsSpec(server, nil)
			spec.Secret = testP8Key(t)
			sender, err := NewSender(spec)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := sender.Send(context.Background(), strings.Repeat("c", 64), Message{Title: "x"}); !IsInvalidTarget(err) {
				t.Fatalf("err=%v, want ErrInvalidTarget", err)
			}
		})
	}
}

func TestAPNSReportsBadCredentialsWithoutRetiringTheDevice(t *testing.T) {
	var calls []apnsCall
	server := fakeAPNS(t, &calls, func(string) (int, string, string) {
		return http.StatusForbidden, "InvalidProviderToken", ""
	})
	spec := apnsSpec(server, nil)
	spec.Secret = testP8Key(t)
	sender, err := NewSender(spec)
	if err != nil {
		t.Fatal(err)
	}
	_, err = sender.Send(context.Background(), strings.Repeat("d", 64), Message{Title: "x"})
	if !IsUndeliverable(err) {
		t.Fatalf("err=%v, want ErrUndeliverable", err)
	}
	if IsInvalidTarget(err) {
		t.Fatal("a bad .p8 key must not retire the user's device token")
	}
	// The refused token must be dropped so the next attempt signs a fresh one.
	if sender.(*apnsSender).signed != "" {
		t.Fatal("the refused provider token was kept")
	}
}

func TestAPNSKeepsAnInternalFaultRetryable(t *testing.T) {
	var calls []apnsCall
	server := fakeAPNS(t, &calls, func(string) (int, string, string) {
		return http.StatusInternalServerError, "InternalServiceError", ""
	})
	spec := apnsSpec(server, nil)
	spec.Secret = testP8Key(t)
	sender, err := NewSender(spec)
	if err != nil {
		t.Fatal(err)
	}
	_, err = sender.Send(context.Background(), strings.Repeat("e", 64), Message{Title: "x"})
	if err == nil || IsInvalidTarget(err) || IsUndeliverable(err) {
		t.Fatalf("err=%v, want a retryable failure", err)
	}
}

// TestAPNSVerifyUsesAnImpossibleToken documents the trick this adapter's credential
// check depends on: APNs authenticates before it looks at the device token, so
// BadDeviceToken proves the key, team id and topic were accepted.
func TestAPNSVerifyUsesAnImpossibleToken(t *testing.T) {
	var calls []apnsCall
	server := fakeAPNS(t, &calls, func(string) (int, string, string) {
		return http.StatusBadRequest, "BadDeviceToken", ""
	})
	spec := apnsSpec(server, nil)
	spec.Secret = testP8Key(t)
	sender, err := NewSender(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := sender.Verify(context.Background()); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if len(calls) != 1 || calls[0].Path != "/3/device/"+strings.Repeat("0", 64) {
		t.Fatalf("calls=%+v", calls)
	}
}

func TestAPNSVerifyReportsRefusedCredentials(t *testing.T) {
	var calls []apnsCall
	server := fakeAPNS(t, &calls, func(string) (int, string, string) {
		return http.StatusForbidden, "InvalidProviderToken", ""
	})
	spec := apnsSpec(server, nil)
	spec.Secret = testP8Key(t)
	sender, err := NewSender(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := sender.Verify(context.Background()); err == nil {
		t.Fatal("refused credentials must fail verification")
	}
}

func TestAPNSSelectsTheEnvironmentHost(t *testing.T) {
	for _, testCase := range []struct{ environment, want string }{
		{"", defaultAPNSProduction},
		{APNSEnvironmentProduction, defaultAPNSProduction},
		{APNSEnvironmentSandbox, defaultAPNSSandbox},
	} {
		spec := Spec{
			Kind: KindAPNS, Secret: testP8Key(t),
			Settings: map[string]string{
				SettingAPNSKeyID: "K", SettingAPNSTeamID: "T", SettingAPNSTopic: "com.fasttask.app",
				SettingAPNSEnvironment: testCase.environment,
			},
		}
		sender, err := NewSender(spec)
		if err != nil {
			t.Fatalf("environment %q: %v", testCase.environment, err)
		}
		if got := sender.(*apnsSender).base; got != testCase.want {
			t.Fatalf("environment %q base=%s, want %s", testCase.environment, got, testCase.want)
		}
	}
}

func TestAPNSRequiresCredentialsAndAParsableKey(t *testing.T) {
	key := testP8Key(t)
	if _, err := NewSender(Spec{Kind: KindAPNS, Secret: key, Settings: map[string]string{SettingAPNSTeamID: "T", SettingAPNSTopic: "t"}}); err == nil {
		t.Fatal("a missing key_id must be refused")
	}
	if _, err := NewSender(Spec{Kind: KindAPNS, Secret: key, Settings: map[string]string{SettingAPNSKeyID: "K", SettingAPNSTopic: "t"}}); err == nil {
		t.Fatal("a missing team_id must be refused")
	}
	if _, err := NewSender(Spec{Kind: KindAPNS, Secret: key, Settings: map[string]string{SettingAPNSKeyID: "K", SettingAPNSTeamID: "T"}}); err == nil {
		t.Fatal("a missing topic must be refused")
	}
	if _, err := NewSender(Spec{
		Kind: KindAPNS, Secret: key,
		Settings: map[string]string{SettingAPNSKeyID: "K", SettingAPNSTeamID: "T", SettingAPNSTopic: "t", SettingAPNSEnvironment: "staging"},
	}); err == nil {
		t.Fatal("an unknown environment must be refused")
	}
	if _, err := NewSender(Spec{
		Kind: KindAPNS, Secret: "not a pem",
		Settings: map[string]string{SettingAPNSKeyID: "K", SettingAPNSTeamID: "T", SettingAPNSTopic: "t"},
	}); err == nil {
		t.Fatal("an unparsable .p8 must be refused at configuration time")
	}
	if _, err := NewSender(Spec{
		Kind:     KindAPNS,
		Settings: map[string]string{SettingAPNSKeyID: "K", SettingAPNSTeamID: "T", SettingAPNSTopic: "t"},
	}); err == nil {
		t.Fatal("a missing .p8 must be refused")
	}
}

// TestAPNSAcceptsASEC1Key covers the key that went through openssl on its way here.
func TestAPNSAcceptsASEC1Key(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	spec := Spec{
		Kind: KindAPNS, Secret: string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})),
		Settings: map[string]string{SettingAPNSKeyID: "K", SettingAPNSTeamID: "T", SettingAPNSTopic: "t"},
	}
	if _, err := NewSender(spec); err != nil {
		t.Fatalf("a SEC1 key must be accepted: %v", err)
	}
}

// TestAPNSPayloadFallsBackToTheTopicAsTheBody checks the wire shape, not the Go map:
// APNs drops a notification whose alert has no text at all, so a message with only a
// topic must still arrive readable.
func TestAPNSPayloadFallsBackToTheTopicAsTheBody(t *testing.T) {
	raw, err := json.Marshal(apnsPayload("default", Message{Topic: "notification.test"}))
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		APS struct {
			Alert map[string]string `json:"alert"`
			Sound string            `json:"sound"`
		} `json:"aps"`
		FastTask map[string]string `json:"fasttask"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.APS.Alert["body"] != "notification.test" {
		t.Fatalf("alert=%+v; APNs drops a notification with an empty alert body", payload.APS.Alert)
	}
	if _, present := payload.APS.Alert["title"]; present {
		t.Fatal("an empty title must be omitted rather than sent as an empty string")
	}
	if payload.APS.Sound != "default" {
		t.Fatalf("sound=%q", payload.APS.Sound)
	}
	if payload.FastTask["topic"] != "notification.test" {
		t.Fatalf("fasttask=%+v", payload.FastTask)
	}
}

func TestAPNSRefusesAnEmptyAddress(t *testing.T) {
	var calls []apnsCall
	server := fakeAPNS(t, &calls, func(string) (int, string, string) { return http.StatusOK, "", "id" })
	spec := apnsSpec(server, nil)
	spec.Secret = testP8Key(t)
	sender, err := NewSender(spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sender.Send(context.Background(), "", Message{Title: "x"}); !IsInvalidTarget(err) {
		t.Fatalf("err=%v, want ErrNoAddress", err)
	}
	if len(calls) != 0 {
		t.Fatalf("a call was made without an address: %+v", calls)
	}
}
