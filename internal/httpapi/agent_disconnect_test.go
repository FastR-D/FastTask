package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/FastR-D/FastTask/internal/agent"
	"github.com/FastR-D/FastTask/internal/application"
	"github.com/FastR-D/FastTask/internal/persistence"
)

// blockingProvider blocks inside Chat until released, so a test can observe a run
// mid-flight. entered is closed on the first Chat call; Chat returns once release
// is closed. Its context is the WORKER's (via ExecuteRun), never the HTTP
// request's, which is exactly why a client disconnect cannot reach it (§4.3).
type blockingProvider struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *blockingProvider) Name() string { return "blocking" }

func (p *blockingProvider) Chat(ctx context.Context, req agent.ChatRequest, sink agent.ChatSink) (agent.ChatResult, error) {
	p.once.Do(func() { close(p.entered) })
	select {
	case <-p.release:
		if sink != nil {
			_ = sink.TextDelta(ctx, "完成")
		}
		return agent.ChatResult{FinishReason: "stop", Text: "完成"}, nil
	case <-ctx.Done():
		return agent.ChatResult{}, ctx.Err()
	}
}

// latestRun returns the user's most recent agent run.
func latestRun(t *testing.T, api testAPI, userID string) persistence.AgentRun {
	t.Helper()
	var run persistence.AgentRun
	if err := api.store.DB.Where("user_id = ?", userID).Order("created_at DESC").First(&run).Error; err != nil {
		t.Fatalf("load run: %v", err)
	}
	return run
}

// waitRunStatus polls until the user's latest run reaches want or the deadline.
func waitRunStatus(t *testing.T, api testAPI, userID, want string) persistence.AgentRun {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var run persistence.AgentRun
	for time.Now().Before(deadline) {
		run = latestRun(t, api, userID)
		if run.Status == want {
			return run
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("run status=%q, want %q within deadline", run.Status, want)
	return run
}

// TestAgentClientDisconnectDoesNotCancelRun covers §4.3 and §10 ("客户端断开后
// 运行继续"; "客户端断开不能[取消运行]"): cancelling the HTTP request context
// stops the SSE stream, but the worker-owned run keeps executing and still
// succeeds. The run's lifetime is deliberately not tied to the request lifetime —
// this is what makes "关掉浏览器再打开，任务还在跑" hold (agent.md §3).
func TestAgentClientDisconnectDoesNotCancelRun(t *testing.T) {
	provider := &blockingProvider{entered: make(chan struct{}), release: make(chan struct{})}
	api := newTestAPIWithAgent(t, application.WithChatResolver(func(context.Context) (agent.ChatProvider, error) { return provider, nil }))
	workerCancel := startAgentWorker(t, api)
	defer workerCancel()

	// Fire the commands request with a cancellable context so the test can
	// simulate a client disconnect mid-stream. api.do cannot inject a context, so
	// the request is built directly against the same engine.
	encoded, _ := json.Marshal(commandsBody("帮我推进论文"))
	reqCtx, reqCancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "/api/v1/agent/commands", bytes.NewReader(encoded)).WithContext(reqCtx)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+api.access)
	recorder := httptest.NewRecorder()
	served := make(chan struct{})
	go func() {
		defer close(served)
		api.server.Engine.ServeHTTP(recorder, request)
	}()

	// Wait until the worker has claimed the job and is blocked inside Chat.
	select {
	case <-provider.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker never started the run")
	}
	if got := latestRun(t, api, api.user.ID).Status; got != persistence.RunRunning {
		t.Fatalf("run status=%q mid-flight, want running", got)
	}

	// Disconnect the client. The stream handler must return, but must NOT cancel
	// the run (§4.3: AbortSignal 触发时不得取消运行).
	reqCancel()
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("stream handler did not return after client disconnect")
	}
	if got := latestRun(t, api, api.user.ID).Status; got == persistence.RunCancelled {
		t.Fatal("client disconnect cancelled the run; the run is worker-owned and must survive (§4.3)")
	}

	// Let the worker finish: the run still succeeds despite the disconnect, and a
	// later reconnect could have replayed its persisted chunks (§8).
	close(provider.release)
	if got := waitRunStatus(t, api, api.user.ID, persistence.RunSucceeded); got.Status != persistence.RunSucceeded {
		t.Fatalf("run status=%q after disconnect+release, want succeeded", got.Status)
	}
}

// toolThenStopProvider blocks on turn 0, then returns a readonly tool call so the
// loop advances to turn 1 — where the cancel_requested check fires (§6: "每轮开始
// 前检查 cancel_requested"). Later turns return stop so a missed cancel cannot
// spin forever.
type toolThenStopProvider struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	calls   int
}

func (p *toolThenStopProvider) Name() string { return "tool-then-stop" }

func (p *toolThenStopProvider) Chat(ctx context.Context, req agent.ChatRequest, sink agent.ChatSink) (agent.ChatResult, error) {
	i := p.calls
	p.calls++
	if i == 0 {
		p.once.Do(func() { close(p.entered) })
		select {
		case <-p.release:
		case <-ctx.Done():
			return agent.ChatResult{}, ctx.Err()
		}
		return agent.ChatResult{
			FinishReason: "tool_calls",
			ToolCalls:    []agent.ToolCall{{ID: "call_goals", Name: "list_goals", Arguments: "{}"}},
		}, nil
	}
	return agent.ChatResult{FinishReason: "stop", Text: "不应到达"}, nil
}

// TestAgentExplicitCancelTerminatesRunOverHTTP is the §10 counterpart to the
// disconnect test ("显式取消能终止运行；客户端断开不能"): an explicit
// PUT /agent-jobs/{id}/cancellation while a run is in flight sets cancel_requested,
// and the loop terminates the run as cancelled at the next turn boundary.
func TestAgentExplicitCancelTerminatesRunOverHTTP(t *testing.T) {
	provider := &toolThenStopProvider{entered: make(chan struct{}), release: make(chan struct{})}
	api := newTestAPIWithAgent(t, application.WithChatResolver(func(context.Context) (agent.ChatProvider, error) { return provider, nil }))
	workerCancel := startAgentWorker(t, api)
	defer workerCancel()

	encoded, _ := json.Marshal(commandsBody("帮我推进论文"))
	request := httptest.NewRequest(http.MethodPost, "/api/v1/agent/commands", bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+api.access)
	recorder := httptest.NewRecorder()
	served := make(chan struct{})
	go func() {
		defer close(served)
		api.server.Engine.ServeHTTP(recorder, request)
	}()

	// Wait until the worker is blocked inside turn 0's Chat.
	select {
	case <-provider.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker never started the run")
	}
	run := latestRun(t, api, api.user.ID)
	if run.Status != persistence.RunRunning {
		t.Fatalf("run status=%q mid-flight, want running", run.Status)
	}

	// Explicitly cancel the job through the HTTP endpoint, reading the live job
	// revision for If-Match (the worker is blocked in Chat, so it is stable).
	var job persistence.AgentJob
	if err := api.store.DB.Where("id = ?", run.JobID).First(&job).Error; err != nil {
		t.Fatalf("load job: %v", err)
	}
	resp := api.do(t, http.MethodPut, "/api/v1/agent-jobs/"+job.ID+"/cancellation", nil, map[string]string{"If-Match": application.StrongETag("job", job.ID, job.Revision)})
	if resp.Code != http.StatusOK {
		t.Fatalf("cancellation status=%d body=%s", resp.Code, resp.Body.String())
	}

	// Release turn 0; the loop executes the readonly tool, reaches turn 1, and the
	// cancel check terminates the run.
	close(provider.release)
	if got := waitRunStatus(t, api, api.user.ID, persistence.RunCancelled); got.Status != persistence.RunCancelled {
		t.Fatalf("run status=%q after explicit cancel, want cancelled", got.Status)
	}
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not end after the run was cancelled")
	}
}
