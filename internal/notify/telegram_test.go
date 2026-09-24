package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testTelegramToken = "123456789:AA-test-bot-token"

// telegramCall records what one request to the fake Bot API carried.
type telegramCall struct {
	Method string
	Path   string
	Body   map[string]any
}

// fakeTelegram serves the two Bot API methods this adapter uses, answering with the
// canned envelope for the path suffix it is given.
func fakeTelegram(t *testing.T, calls *[]telegramCall, answer func(path string) (int, map[string]any)) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if r.Body != nil {
			raw, _ := io.ReadAll(r.Body)
			if len(raw) > 0 {
				_ = json.Unmarshal(raw, &body)
			}
		}
		*calls = append(*calls, telegramCall{Method: r.Method, Path: r.URL.Path, Body: body})
		status, payload := answer(r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(payload)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestTelegramVerifyReadsGetMe(t *testing.T) {
	var calls []telegramCall
	server := fakeTelegram(t, &calls, func(string) (int, map[string]any) {
		return http.StatusOK, map[string]any{"ok": true, "result": map[string]any{"username": "fasttask_bot"}}
	})
	sender, err := NewSender(Spec{Kind: KindTelegram, Endpoint: server.URL, Secret: testTelegramToken})
	if err != nil {
		t.Fatal(err)
	}
	if err := sender.Verify(context.Background()); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if len(calls) != 1 || calls[0].Method != http.MethodGet || calls[0].Path != "/bot"+testTelegramToken+"/getMe" {
		t.Fatalf("calls=%+v", calls)
	}
}

func TestTelegramVerifyReportsARefusedToken(t *testing.T) {
	var calls []telegramCall
	server := fakeTelegram(t, &calls, func(string) (int, map[string]any) {
		return http.StatusUnauthorized, map[string]any{"ok": false, "error_code": 401, "description": "Unauthorized"}
	})
	sender, err := NewSender(Spec{Kind: KindTelegram, Endpoint: server.URL, Secret: testTelegramToken})
	if err != nil {
		t.Fatal(err)
	}
	err = sender.Verify(context.Background())
	if err == nil {
		t.Fatal("a refused token must fail verification")
	}
	if !IsUndeliverable(err) {
		t.Fatalf("a bad bot token is a channel fault: %v", err)
	}
	if strings.Contains(err.Error(), testTelegramToken) {
		t.Fatalf("the error carried the bot token: %v", err)
	}
}

func TestTelegramSendPostsPlainTextToTheChat(t *testing.T) {
	var calls []telegramCall
	server := fakeTelegram(t, &calls, func(string) (int, map[string]any) {
		return http.StatusOK, map[string]any{"ok": true, "result": map[string]any{"message_id": 4217}}
	})
	sender, err := NewSender(Spec{Kind: KindTelegram, Endpoint: server.URL, Secret: testTelegramToken})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := sender.Send(context.Background(), "-1001234567890", Message{
		Topic: "proposal.pending", Title: "有待确认的任务树提案", Body: "拆解《论文初稿》", URL: "https://fasttask.example/goals/g1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.ProviderID != "4217" {
		t.Fatalf("receipt=%+v", receipt)
	}
	if len(calls) != 1 || calls[0].Path != "/bot"+testTelegramToken+"/sendMessage" {
		t.Fatalf("calls=%+v", calls)
	}
	if calls[0].Body["chat_id"] != "-1001234567890" {
		t.Fatalf("chat_id=%v", calls[0].Body["chat_id"])
	}
	text, _ := calls[0].Body["text"].(string)
	if !strings.Contains(text, "有待确认的任务树提案") || !strings.Contains(text, "拆解《论文初稿》") || !strings.Contains(text, "https://fasttask.example/goals/g1") {
		t.Fatalf("text=%q", text)
	}
	// Plain text on purpose: a title a user typed must never be interpreted as markup.
	if _, present := calls[0].Body["parse_mode"]; present {
		t.Fatal("parse_mode must not be set")
	}
}

func TestTelegramSendClassifiesADeadChat(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		status      int
		description string
	}{
		{"chat not found", http.StatusBadRequest, "Bad Request: chat not found"},
		{"bot was blocked", http.StatusForbidden, "Forbidden: bot was blocked by the user"},
		{"user deactivated", http.StatusForbidden, "Forbidden: user is deactivated"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var calls []telegramCall
			server := fakeTelegram(t, &calls, func(string) (int, map[string]any) {
				return testCase.status, map[string]any{"ok": false, "error_code": testCase.status, "description": testCase.description}
			})
			sender, err := NewSender(Spec{Kind: KindTelegram, Secret: testTelegramToken, Endpoint: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			_, err = sender.Send(context.Background(), "12345", Message{Title: "x"})
			if !IsInvalidTarget(err) {
				t.Fatalf("err=%v, want ErrInvalidTarget", err)
			}
		})
	}
}

func TestTelegramSendKeepsARateLimitRetryable(t *testing.T) {
	var calls []telegramCall
	server := fakeTelegram(t, &calls, func(string) (int, map[string]any) {
		return http.StatusTooManyRequests, map[string]any{
			"ok": false, "error_code": 429, "description": "Too Many Requests: retry after 17",
			"parameters": map[string]any{"retry_after": 17},
		}
	})
	sender, err := NewSender(Spec{Kind: KindTelegram, Endpoint: server.URL, Secret: testTelegramToken})
	if err != nil {
		t.Fatal(err)
	}
	_, err = sender.Send(context.Background(), "12345", Message{Title: "x"})
	if err == nil {
		t.Fatal("a rate limit must be reported")
	}
	if IsInvalidTarget(err) || IsUndeliverable(err) {
		t.Fatalf("a rate limit is retryable: %v", err)
	}
	if !strings.Contains(err.Error(), "retry after 17") {
		t.Fatalf("the operator lost the retry hint: %v", err)
	}
}

func TestTelegramSendRefusesAnEmptyAddress(t *testing.T) {
	var calls []telegramCall
	server := fakeTelegram(t, &calls, func(string) (int, map[string]any) {
		return http.StatusOK, map[string]any{"ok": true}
	})
	sender, err := NewSender(Spec{Kind: KindTelegram, Endpoint: server.URL, Secret: testTelegramToken})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sender.Send(context.Background(), "   ", Message{Title: "x"}); !IsInvalidTarget(err) {
		t.Fatalf("err=%v, want ErrNoAddress", err)
	}
	if len(calls) != 0 {
		t.Fatalf("a call was made without an address: %+v", calls)
	}
}

func TestTelegramRequiresATokenAndAnHTTPBase(t *testing.T) {
	if _, err := NewSender(Spec{Kind: KindTelegram}); err == nil {
		t.Fatal("a channel without a bot token must not build")
	}
	if _, err := NewSender(Spec{Kind: KindTelegram, Secret: "token", Endpoint: "api.telegram.org"}); err == nil {
		t.Fatal("a base without a scheme must be refused")
	}
	if _, err := NewSender(Spec{Kind: KindTelegram, Secret: "token", Endpoint: "file:///etc/passwd"}); err == nil {
		t.Fatal("a non-http scheme must be refused")
	}
}

func TestTelegramAcceptsTheAPIBaseSetting(t *testing.T) {
	var calls []telegramCall
	server := fakeTelegram(t, &calls, func(string) (int, map[string]any) {
		return http.StatusOK, map[string]any{"ok": true, "result": map[string]any{"message_id": 1}}
	})
	sender, err := NewSender(Spec{Kind: KindTelegram, Secret: testTelegramToken, Settings: map[string]string{SettingTelegramAPIBase: server.URL + "/"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sender.Send(context.Background(), "1", Message{Title: "x"}); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Fatalf("calls=%+v", calls)
	}
}

func TestTelegramDefaultsToThePublicAPI(t *testing.T) {
	sender, err := NewSender(Spec{Kind: KindTelegram, Secret: testTelegramToken})
	if err != nil {
		t.Fatal(err)
	}
	configured := sender.(*telegramSender)
	if configured.base != defaultTelegramAPI {
		t.Fatalf("base=%s", configured.base)
	}
}
