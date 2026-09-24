package notify

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// testServiceAccount builds a key file shaped like the one Firebase hands out. The
// private key is real: oauth2/jwt signs the assertion with it, so a test that faked
// the PEM would not exercise the parsing this adapter does in production.
func testServiceAccount(t *testing.T, tokenURL, projectID string) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	block := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	document := map[string]any{
		"type": "service_account", "project_id": projectID, "private_key_id": "key-id-1",
		"private_key": string(block), "client_email": "fasttask@testing.iam.gserviceaccount.com",
	}
	if tokenURL != "" {
		document["token_url"] = tokenURL
	}
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// fakeTokenEndpoint counts the exchanges so a test can assert the access token is
// cached rather than re-signed per message.
func fakeTokenEndpoint(t *testing.T, calls *int32, status int) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status != http.StatusOK {
			_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
			return
		}
		_, _ = io.WriteString(w, `{"access_token":"fcm-test-token","expires_in":3600,"token_type":"Bearer"}`)
	}))
	t.Cleanup(server.Close)
	return server
}

// fcmCall records one request to the fake FCM endpoint.
type fcmCall struct {
	Path          string
	Authorization string
	Body          map[string]any
}

func fakeFCM(t *testing.T, calls *[]fcmCall, answer func() (int, map[string]any)) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		*calls = append(*calls, fcmCall{Path: r.URL.Path, Authorization: r.Header.Get("Authorization"), Body: body})
		status, payload := answer()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(payload)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestFCMSendUsesTheV1EndpointAndTheServiceAccountToken(t *testing.T) {
	var tokenCalls int32
	tokens := fakeTokenEndpoint(t, &tokenCalls, http.StatusOK)
	var calls []fcmCall
	api := fakeFCM(t, &calls, func() (int, map[string]any) {
		return http.StatusOK, map[string]any{"name": "projects/testing/messages/0001"}
	})

	sender, err := NewSender(Spec{Kind: KindFCM, Endpoint: api.URL, Secret: testServiceAccount(t, tokens.URL, "testing")})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := sender.Send(context.Background(), "registration-token-1", Message{
		Topic: "proposal.pending", Title: "有待确认的提案", Body: "拆解《论文初稿》",
		URL: "https://fasttask.example/goals/g1", CollapseID: "proposal-1", Data: map[string]string{"goal_id": "g1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.ProviderID != "projects/testing/messages/0001" {
		t.Fatalf("receipt=%+v", receipt)
	}
	if len(calls) != 1 || calls[0].Path != "/v1/projects/testing/messages:send" {
		t.Fatalf("calls=%+v", calls)
	}
	if calls[0].Authorization != "Bearer fcm-test-token" {
		t.Fatalf("authorization=%q", calls[0].Authorization)
	}
	message, _ := calls[0].Body["message"].(map[string]any)
	if message == nil || message["token"] != "registration-token-1" {
		t.Fatalf("message=%+v", calls[0].Body)
	}
	notification, _ := message["notification"].(map[string]any)
	if notification["title"] != "有待确认的提案" || notification["body"] != "拆解《论文初稿》" {
		t.Fatalf("notification=%+v", notification)
	}
	data, _ := message["data"].(map[string]any)
	if data["goal_id"] != "g1" || data["topic"] != "proposal.pending" {
		t.Fatalf("data=%+v", data)
	}
	options, _ := message["fcm_options"].(map[string]any)
	if options["link"] != "https://fasttask.example/goals/g1" {
		t.Fatalf("fcm_options=%+v", options)
	}
	android, _ := message["android"].(map[string]any)
	if android["collapse_key"] != "proposal-1" {
		t.Fatalf("android=%+v", android)
	}
}

func TestFCMCachesTheAccessTokenBetweenMessages(t *testing.T) {
	var tokenCalls int32
	tokens := fakeTokenEndpoint(t, &tokenCalls, http.StatusOK)
	var calls []fcmCall
	api := fakeFCM(t, &calls, func() (int, map[string]any) {
		return http.StatusOK, map[string]any{"name": "projects/testing/messages/0002"}
	})
	sender, err := NewSender(Spec{Kind: KindFCM, Endpoint: api.URL, Secret: testServiceAccount(t, tokens.URL, "testing")})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := sender.Send(context.Background(), "token", Message{Title: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	if got := atomic.LoadInt32(&tokenCalls); got != 1 {
		t.Fatalf("token exchanges=%d, want 1 — Google rate limits signing", got)
	}
}

func TestFCMSendClassifiesAnUnregisteredToken(t *testing.T) {
	var tokenCalls int32
	tokens := fakeTokenEndpoint(t, &tokenCalls, http.StatusOK)
	var calls []fcmCall
	api := fakeFCM(t, &calls, func() (int, map[string]any) {
		return http.StatusNotFound, map[string]any{"error": map[string]any{
			"code": 404, "message": "Requested entity was not found.", "status": "NOT_FOUND",
		}}
	})
	sender, err := NewSender(Spec{Kind: KindFCM, Endpoint: api.URL, Secret: testServiceAccount(t, tokens.URL, "testing")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sender.Send(context.Background(), "expired-token", Message{Title: "x"}); !IsInvalidTarget(err) {
		t.Fatalf("err=%v, want ErrInvalidTarget", err)
	}
}

func TestFCMSendReportsAProjectMismatchAsAChannelFault(t *testing.T) {
	var tokenCalls int32
	tokens := fakeTokenEndpoint(t, &tokenCalls, http.StatusOK)
	var calls []fcmCall
	api := fakeFCM(t, &calls, func() (int, map[string]any) {
		return http.StatusForbidden, map[string]any{"error": map[string]any{
			"code": 403, "message": "The caller does not have permission", "status": "PERMISSION_DENIED",
		}}
	})
	sender, err := NewSender(Spec{Kind: KindFCM, Endpoint: api.URL, Secret: testServiceAccount(t, tokens.URL, "testing")})
	if err != nil {
		t.Fatal(err)
	}
	_, err = sender.Send(context.Background(), "token", Message{Title: "x"})
	if !IsUndeliverable(err) {
		t.Fatalf("err=%v, want ErrUndeliverable", err)
	}
	if IsInvalidTarget(err) {
		t.Fatal("a project fault must not retire the user's device")
	}
}

func TestFCMVerifyProvesTheServiceAccount(t *testing.T) {
	var tokenCalls int32
	tokens := fakeTokenEndpoint(t, &tokenCalls, http.StatusOK)
	sender, err := NewSender(Spec{Kind: KindFCM, Secret: testServiceAccount(t, tokens.URL, "testing")})
	if err != nil {
		t.Fatal(err)
	}
	if err := sender.Verify(context.Background()); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if atomic.LoadInt32(&tokenCalls) != 1 {
		t.Fatal("verify did not exchange the service account")
	}
}

func TestFCMVerifyReportsARefusedServiceAccount(t *testing.T) {
	var tokenCalls int32
	tokens := fakeTokenEndpoint(t, &tokenCalls, http.StatusBadRequest)
	sender, err := NewSender(Spec{Kind: KindFCM, Secret: testServiceAccount(t, tokens.URL, "testing")})
	if err != nil {
		t.Fatal(err)
	}
	err = sender.Verify(context.Background())
	if err == nil {
		t.Fatal("a refused service account must fail verification")
	}
	if strings.Contains(err.Error(), "PRIVATE KEY") {
		t.Fatalf("the error carried the key: %v", err)
	}
}

func TestFCMRequiresAUsableKeyFile(t *testing.T) {
	if _, err := NewSender(Spec{Kind: KindFCM}); err == nil {
		t.Fatal("a channel without a service-account key must not build")
	}
	if _, err := NewSender(Spec{Kind: KindFCM, Secret: "{not json"}); err == nil {
		t.Fatal("malformed JSON must be refused")
	}
	if _, err := NewSender(Spec{Kind: KindFCM, Secret: `{"type":"authorized_user","client_email":"a@b","private_key":"k"}`}); err == nil {
		t.Fatal("an authorized-user key file must be refused")
	}
	if _, err := NewSender(Spec{Kind: KindFCM, Secret: `{"type":"service_account","client_email":"a@b","private_key":"k"}`}); err == nil {
		t.Fatal("a key file with no project id must be refused when none is configured")
	}
}

func TestFCMProjectIDSettingOverridesTheKeyFile(t *testing.T) {
	var tokenCalls int32
	tokens := fakeTokenEndpoint(t, &tokenCalls, http.StatusOK)
	var calls []fcmCall
	api := fakeFCM(t, &calls, func() (int, map[string]any) {
		return http.StatusOK, map[string]any{"name": "projects/override/messages/1"}
	})
	sender, err := NewSender(Spec{
		Kind: KindFCM, Endpoint: api.URL, Secret: testServiceAccount(t, tokens.URL, "from-key-file"),
		Settings: map[string]string{settingFCMProject: "override"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sender.Send(context.Background(), "token", Message{Title: "x"}); err != nil {
		t.Fatal(err)
	}
	if calls[0].Path != "/v1/projects/override/messages:send" {
		t.Fatalf("path=%s", calls[0].Path)
	}
}
