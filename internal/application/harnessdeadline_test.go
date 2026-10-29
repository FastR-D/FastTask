package application

import (
	"context"
	"errors"
	"github.com/FastR-D/FastTask/internal/persistence"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func providerHarness(t *testing.T, handler http.HandlerFunc, limits LoopLimits) (*AgentService, HarnessPrincipal) {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.Copy(io.Discard, r.Body); handler(w, r) }))
	t.Cleanup(up.Close)
	store, owner, _ := panelTestStore(t)
	svc := NewAgentService(New(store), WithLoopLimits(limits), WithCredentialsResolver(func(context.Context) (*ModelCredentials, error) {
		return &ModelCredentials{BaseURL: up.URL, Model: "fixture", APIKey: "fixture-private-key"}, nil
	}))
	result, err := svc.SubmitCommands(context.Background(), owner, CommandsRequest{HarnessMode: HarnessModeWASM, Commands: []Command{{Type: "add-message", Message: &CommandMessage{Role: "user", Parts: []CommandPart{{Type: "text", Text: "hello"}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	grant, err := svc.IssueBrowserRunGrant(context.Background(), owner, result.RunID, "")
	if err != nil {
		t.Fatal(err)
	}
	p, err := svc.AuthenticateHarness(context.Background(), "Bearer "+grant.HarnessToken)
	if err != nil {
		t.Fatal(err)
	}
	return svc, p
}
func TestHarnessDeadlines(t *testing.T) {
	for _, mode := range []string{"headers", "idle", "heartbeat", "active"} {
		t.Run(mode, func(t *testing.T) {
			svc, p := providerHarness(t, func(w http.ResponseWriter, r *http.Request) {
				if mode == "headers" {
					<-r.Context().Done()
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(200)
				w.(http.Flusher).Flush()
				if mode == "idle" {
					<-r.Context().Done()
					return
				}
				ticker := time.NewTicker(10 * time.Millisecond)
				defer ticker.Stop()
				for {
					select {
					case <-r.Context().Done():
						return
					case <-ticker.C:
						if mode == "heartbeat" {
							io.WriteString(w, ": ping\n\n")
						} else {
							io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"字\"}}]}\n\n")
						}
						w.(http.Flusher).Flush()
					}
				}
			}, LoopLimits{WallClock: 200 * time.Millisecond, HeaderTimeout: 60 * time.Millisecond, IdleTimeout: 80 * time.Millisecond})
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			stream, err := svc.OpenModelProxy(ctx, p, []byte(`{}`))
			if mode == "headers" {
				var e *ProxyError
				if !errors.As(err, &e) || e.Code != "MODEL_HEADER_TIMEOUT" {
					t.Fatalf("wrong header error: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				_, err = io.ReadAll(stream.Body)
				stream.Body.Close()
				if err == nil {
					t.Fatal("deadline did not interrupt stream")
				}
			}
			run, err := svc.repo.GetRun(context.Background(), p.UserID, p.RunID)
			if err != nil || run.Status != persistence.RunFailed {
				t.Fatalf("run not failed: %+v %v", run, err)
			}
			if run.ModelInflight != "" {
				t.Fatal("attempt lease leaked")
			}
		})
	}
}
func TestHarnessCancellationWithoutProviderBytes(t *testing.T) {
	entered := make(chan struct{})
	var once sync.Once
	svc, p := providerHarness(t, func(w http.ResponseWriter, r *http.Request) { once.Do(func() { close(entered) }); <-r.Context().Done() }, LoopLimits{WallClock: time.Second, HeaderTimeout: time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := svc.OpenModelProxy(ctx, p, []byte(`{}`)); done <- err }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("provider never entered")
	}
	if err := svc.RequestCancellation(ctx, p.UserID, p.RunID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancel returned success")
		}
	case <-ctx.Done():
		t.Fatal("cancel blocked waiting for provider")
	}
	run, err := svc.repo.GetRun(context.Background(), p.UserID, p.RunID)
	if err != nil || run.Status != persistence.RunCancelled {
		t.Fatalf("wrong terminal status: %+v %v", run, err)
	}
}
func TestHarnessAttemptSingleFlightAndLimit(t *testing.T) {
	svc, p := providerHarness(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }, LoopLimits{MaxAttempts: 2})
	lease, err := svc.repo.AcquireModelAttempt(context.Background(), p.UserID, p.RunID, 2, 8)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.OpenModelProxy(context.Background(), p, []byte(`{}`)); err == nil {
		t.Fatal("concurrent attempt allowed")
	}
	if err = svc.repo.ReleaseModelAttempt(context.Background(), p.UserID, p.RunID, lease); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.OpenModelProxy(context.Background(), p, []byte(`{}`)); err == nil {
		t.Fatal("503 accepted")
	}
	if _, err = svc.OpenModelProxy(context.Background(), p, []byte(`{}`)); err == nil {
		t.Fatal("attempt limit not enforced")
	}
	run, _ := svc.repo.GetRun(context.Background(), p.UserID, p.RunID)
	if run.ModelAttempts != 2 || run.Status != persistence.RunFailed {
		t.Fatalf("wrong budget: %+v", run)
	}
}
func TestHarnessIncompleteStreamNeverFinalizesTool(t *testing.T) {
	for _, frame := range []string{"data: [DONE]\n\n", "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"} {
		svc, p := providerHarness(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, frame) }, LoopLimits{})
		stream, err := svc.OpenModelProxy(context.Background(), p, []byte(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.ReadAll(stream.Body)
		stream.Body.Close()
		if err == nil {
			t.Fatal("incomplete stream accepted")
		}
	}
}

func TestHarnessBatchesTextAndPersistsProviderUsage(t *testing.T) {
	svc, p := providerHarness(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"字\"}}]}\n\n")
		for i := 1; i < 1000; i++ {
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"字\"}}]}\n\n")
		}
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":1000,\"total_tokens\":1020,\"private\":\"must-not-store\"}}\n\ndata: [DONE]\n\n")
	}, LoopLimits{})
	stream, err := svc.OpenModelProxy(context.Background(), p, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.Copy(io.Discard, stream.Body); err != nil {
		t.Fatal(err)
	}
	stream.Body.Close()
	run, _ := svc.repo.GetRun(context.Background(), p.UserID, p.RunID)
	if !strings.Contains(run.UsageJSON, "prompt_tokens") || strings.Contains(run.UsageJSON, "must-not-store") {
		t.Fatalf("invalid usage: %s", run.UsageJSON)
	}
	count, err := svc.repo.CountChunks(context.Background(), p.UserID, p.RunID)
	if err != nil || count > 100 {
		t.Fatalf("deltas not batched: %d %v", count, err)
	}
	messages, _ := svc.repo.ListThreadMessages(context.Background(), p.UserID, p.ThreadID)
	parts, _ := svc.repo.ListMessageParts(context.Background(), p.UserID, messages[len(messages)-1].ID)
	if len(parts) != 1 || len([]rune(parts[0].Text)) != 1000 {
		t.Fatal("batching lost text")
	}
}

func TestHarnessProviderErrorDoesNotLeakBody(t *testing.T) {
	svc, p := providerHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		io.WriteString(w, "fixture-private-key Bearer fixture-cap data:image/png;base64,c2VjcmV0 private-model")
	}, LoopLimits{})
	_, err := svc.OpenModelProxy(context.Background(), p, []byte(`{}`))
	if err == nil {
		t.Fatal("provider rejection accepted")
	}
	for _, secret := range []string{"fixture-private-key", "fixture-cap", "c2VjcmV0", "private-model"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("provider body leaked %q", secret)
		}
	}
}
