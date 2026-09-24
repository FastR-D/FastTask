package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// barkCall records one request to the fake Bark server.
type barkCall struct {
	Method string
	Path   string
	Body   map[string]any
}

func fakeBark(t *testing.T, calls *[]barkCall, answer func(path string) (int, string, map[string]any)) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if r.Body != nil {
			raw, _ := io.ReadAll(r.Body)
			if len(raw) > 0 {
				_ = json.Unmarshal(raw, &body)
			}
		}
		*calls = append(*calls, barkCall{Method: r.Method, Path: r.URL.Path, Body: body})
		status, text, payload := answer(r.URL.Path)
		w.WriteHeader(status)
		if payload != nil {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(payload)
			return
		}
		_, _ = io.WriteString(w, text)
	}))
	t.Cleanup(server.Close)
	return server
}

func okBark(string) (int, string, map[string]any) {
	return http.StatusOK, "", map[string]any{"code": 200, "message": "success"}
}

func TestBarkVerifyPingsTheServer(t *testing.T) {
	var calls []barkCall
	server := fakeBark(t, &calls, func(string) (int, string, map[string]any) {
		return http.StatusOK, "success", nil
	})
	sender, err := NewSender(Spec{Kind: KindBark, Endpoint: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err := sender.Verify(context.Background()); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if len(calls) != 1 || calls[0].Method != http.MethodGet || calls[0].Path != "/ping" {
		t.Fatalf("calls=%+v", calls)
	}
}

func TestBarkVerifyRefusesSomethingThatIsNotABarkServer(t *testing.T) {
	var calls []barkCall
	server := fakeBark(t, &calls, func(string) (int, string, map[string]any) {
		return http.StatusOK, "<html>nginx</html>", nil
	})
	sender, err := NewSender(Spec{Kind: KindBark, Endpoint: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err := sender.Verify(context.Background()); err == nil {
		t.Fatal("a proxy answering 200 with HTML is not a Bark server")
	}
}

// TestBarkSendKeepsTheDeviceKeyOutOfThePath pins the reason /push is used instead of
// /{key}: the device key is the only credential the user has, and a key in a path is
// copied into every access log between here and the phone.
func TestBarkSendKeepsTheDeviceKeyOutOfThePath(t *testing.T) {
	var calls []barkCall
	server := fakeBark(t, &calls, okBark)
	sender, err := NewSender(Spec{
		Kind: KindBark, Endpoint: server.URL,
		Settings: map[string]string{SettingBarkSound: "glass", SettingBarkLevel: "timeSensitive"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = sender.Send(context.Background(), "bark-device-key-123", Message{
		Topic: "daily_plan.created", Title: "今天的三件事已经排好", Body: "打开今日计划", URL: "https://fasttask.example/today",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].Path != "/push" || calls[0].Method != http.MethodPost {
		t.Fatalf("calls=%+v", calls)
	}
	body := calls[0].Body
	if body["device_key"] != "bark-device-key-123" {
		t.Fatalf("device_key=%v", body["device_key"])
	}
	if body["title"] != "今天的三件事已经排好" || body["body"] != "打开今日计划" {
		t.Fatalf("body=%+v", body)
	}
	if body["url"] != "https://fasttask.example/today" {
		t.Fatalf("url=%v", body["url"])
	}
	if body["group"] != DefaultBarkGroup {
		t.Fatalf("group=%v, want the default so FastTask is one adjustable group in iOS", body["group"])
	}
	if body["sound"] != "glass" || body["level"] != "timeSensitive" {
		t.Fatalf("settings were not applied: %+v", body)
	}
	if body["action"] != "daily_plan.created" {
		t.Fatalf("topic=%v", body["action"])
	}
}

func TestBarkSendClassifiesADeadDeviceKey(t *testing.T) {
	var calls []barkCall
	server := fakeBark(t, &calls, func(string) (int, string, map[string]any) {
		return http.StatusNotFound, "", map[string]any{"code": 404, "message": "device key not found"}
	})
	sender, err := NewSender(Spec{Kind: KindBark, Endpoint: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sender.Send(context.Background(), "gone", Message{Title: "x"}); !IsInvalidTarget(err) {
		t.Fatalf("err=%v, want ErrInvalidTarget", err)
	}
}

// TestBarkSendReadsAStringCode covers the deployments that answer {"code":"400"}
// rather than {"code":400}.
func TestBarkSendReadsAStringCode(t *testing.T) {
	var calls []barkCall
	server := fakeBark(t, &calls, func(string) (int, string, map[string]any) {
		return http.StatusOK, "", map[string]any{"code": "400", "message": "invalid device key"}
	})
	sender, err := NewSender(Spec{Kind: KindBark, Endpoint: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sender.Send(context.Background(), "bad", Message{Title: "x"}); !IsInvalidTarget(err) {
		t.Fatalf("err=%v, want ErrInvalidTarget", err)
	}
}

func TestBarkSendKeepsAServerFaultRetryable(t *testing.T) {
	var calls []barkCall
	server := fakeBark(t, &calls, func(string) (int, string, map[string]any) {
		return http.StatusBadGateway, "", map[string]any{"code": 502, "message": "upstream"}
	})
	sender, err := NewSender(Spec{Kind: KindBark, Endpoint: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	_, err = sender.Send(context.Background(), "device", Message{Title: "x"})
	if err == nil || IsInvalidTarget(err) || IsUndeliverable(err) {
		t.Fatalf("err=%v, want a retryable failure", err)
	}
}

func TestBarkRequiresAServerAndAKnownLevel(t *testing.T) {
	if _, err := NewSender(Spec{Kind: KindBark}); err == nil {
		t.Fatal("a channel without a server must not build")
	}
	if _, err := NewSender(Spec{Kind: KindBark, Endpoint: "https://api.day.app", Settings: map[string]string{SettingBarkLevel: "loud"}}); err == nil {
		t.Fatal("an unknown level must be refused at configuration time")
	}
	sender, err := NewSender(Spec{Kind: KindBark, Settings: map[string]string{SettingBarkServer: "https://api.day.app/"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := sender.(*barkSender).server; got != "https://api.day.app" {
		t.Fatalf("server=%s, want the trailing slash trimmed", got)
	}
}
