package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewSenderRejectsUnknownProvider(t *testing.T) {
	if _, err := NewSender(Spec{Kind: "wechat", Secret: "x"}); err == nil {
		t.Fatal("an unsupported provider must not build a sender")
	}
	if _, err := NewSender(Spec{Kind: "", Secret: "x"}); err == nil {
		t.Fatal("an empty provider must not build a sender")
	}
}

func TestKindsAreTheDocumentedProviders(t *testing.T) {
	got := strings.Join(Kinds(), ",")
	if got != "apns,bark,fcm,telegram" {
		t.Fatalf("kinds=%s; the migration CHECK and the admin select follow this list", got)
	}
}

func TestMessageTextIsPlainAndOrdered(t *testing.T) {
	msg := Message{Title: "  今日三件事  ", Body: "写完实验记录", URL: "https://fasttask.example/today"}
	if got := msg.text(); got != "今日三件事\n写完实验记录\nhttps://fasttask.example/today" {
		t.Fatalf("text=%q", got)
	}
	if got := (Message{Title: "只有标题"}).text(); got != "只有标题" {
		t.Fatalf("text=%q", got)
	}
	if got := (Message{}).text(); got != "" {
		t.Fatalf("text=%q", got)
	}
}

// TestHTTPClientRefusesRedirects pins the rule from the package doc: a credential in
// a header or a path must not be replayed to a host the operator did not configure.
func TestHTTPClientRefusesRedirects(t *testing.T) {
	var followed bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		followed = true
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, upstream.URL, http.StatusFound)
	}))
	defer redirector.Close()

	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, redirector.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := NewHTTPClient(0).Do(request)
	if err != nil {
		t.Fatalf("the client must return the redirect itself, not an error: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusFound {
		t.Fatalf("status=%d, want the un-followed 302", response.StatusCode)
	}
	if followed {
		t.Fatal("the client followed a redirect")
	}
}

func TestErrNoAddressIsAnInvalidTarget(t *testing.T) {
	if !IsInvalidTarget(ErrNoAddress) {
		t.Fatal("an empty address must retire the target like any rejected one")
	}
	if IsUndeliverable(ErrNoAddress) {
		t.Fatal("an empty address is not a channel fault")
	}
}

func TestProviderErrorCarriesNoCredential(t *testing.T) {
	err := providerError(KindTelegram, http.StatusUnauthorized, "not authorized")
	for _, needle := range []string{"bot12345:secret", "sendMessage", "https://"} {
		if strings.Contains(err.Error(), needle) {
			t.Fatalf("the provider error leaked %q: %v", needle, err)
		}
	}
	if !strings.Contains(err.Error(), "not authorized") {
		t.Fatalf("the provider's own diagnosis was dropped: %v", err)
	}
}

// TestTransportFailureIsRetryable asserts the shape every adapter shares: a call that
// never reached the provider is neither a dead address nor a rejected message, so the
// dispatcher must retry it.
func TestTransportFailureIsRetryable(t *testing.T) {
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closedURL := closed.URL
	closed.Close()

	sender, err := NewSender(Spec{Kind: KindBark, Endpoint: closedURL})
	if err != nil {
		t.Fatal(err)
	}
	_, err = sender.Send(context.Background(), "device-key", Message{Title: "x"})
	if err == nil {
		t.Fatal("a closed server must fail the send")
	}
	if IsInvalidTarget(err) || IsUndeliverable(err) {
		t.Fatalf("a transport failure must stay retryable: %v", err)
	}
}
