package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/FastR-D/FastTask/internal/application"
	"github.com/FastR-D/FastTask/internal/persistence"
)

// Run lifetime versus connection lifetime (doc/agent-impl.md §4.3, doc/harness.md §5.2).
//
// The two rules these tests guard are unchanged by the harness and easier to state now: a
// disconnected client never cancels a run, and an explicit cancellation always does — through the
// user's own JWT, never through the host's capability token.

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

// postDetached fires a POST with a cancellable context, the way a browser does, and returns the
// cancel func, the recorder and a channel closed when the handler returns. api.do cannot inject a
// context, so the request is built against the same engine.
func postDetached(t *testing.T, api testAPI, path string, body map[string]any) (context.CancelFunc, *httptest.ResponseRecorder, <-chan struct{}) {
	t.Helper()
	encoded, _ := json.Marshal(body)
	requestContext, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(encoded)).WithContext(requestContext)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+api.access)
	recorder := httptest.NewRecorder()
	served := make(chan struct{})
	go func() {
		defer close(served)
		api.server.Engine.ServeHTTP(recorder, request)
	}()
	return cancel, recorder, served
}

// TestAgentClientDisconnectDoesNotCancelRun covers §4.3 and §10 ("客户端断开后运行继续"; "客户端断开
// 不能[取消运行]"): cancelling the HTTP request stops the SSE stream, but the run belongs to the host
// and keeps going to a successful end.
func TestAgentClientDisconnectDoesNotCancelRun(t *testing.T) {
	stub := newModelStub(t, stubTurn{text: "完成"})
	api := newHarnessAPI(t, stub)

	runID, _ := startHarnessRun(t, api, "帮我推进论文")
	host := newHarnessClient(t, api, runID)

	// A second client attaches to the same run's stream (§2.8) and then goes away: the run must not
	// notice either way.
	cancel, _, served := postDetached(t, api, "/api/v1/agent/resume", map[string]any{
		"commands": []any{}, "runId": runID,
	})
	cancel()
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream handler did not return after the client disconnected")
	}
	if got := host.runStatus().Status; got == persistence.RunCancelled {
		t.Fatal("a client disconnect cancelled the run (§4.3: the run is host-owned)")
	}

	// The host keeps driving and the run still succeeds.
	host.drive(2)
	if got := waitRunStatus(t, api, api.user.ID, persistence.RunSucceeded); got.Status != persistence.RunSucceeded {
		t.Fatalf("run status=%q after disconnect, want succeeded", got.Status)
	}
}

// TestAgentExplicitCancelTerminatesRunOverHTTP is the §5.2 counterpart: an explicit cancellation
// reaches the run through the user's JWT, is visible on the heartbeat channel, refuses further model
// calls, and ends the run cancelled.
func TestAgentExplicitCancelTerminatesRunOverHTTP(t *testing.T) {
	stub := newModelStub(t, stubTurn{text: "不会到达"})
	api := newHarnessAPI(t, stub)
	runID, responses := startHarnessRun(t, api, "帮我推进论文")
	host := newHarnessClient(t, api, runID)
	host.beat()

	response := host.cancel()
	if response.Code != http.StatusOK {
		t.Fatalf("cancellation status=%d body=%s", response.Code, response.Body.String())
	}
	var body struct {
		RunID           string `json:"run_id"`
		Status          string `json:"status"`
		CancelRequested bool   `json:"cancel_requested"`
	}
	decode(t, response, &body)
	if !body.CancelRequested || body.RunID != runID {
		t.Fatalf("cancellation body=%#v", body)
	}
	if body.Status != persistence.RunCancelling {
		t.Fatalf("run status=%q, want cancelling", body.Status)
	}

	// Channel 1: the heartbeat carries the cancellation to an idle host (§5.2).
	beat := host.beat()
	if cancelRequested, _ := beat["cancel_requested"].(bool); !cancelRequested {
		t.Fatalf("heartbeat=%#v, want cancel_requested", beat)
	}
	// Channel 3: a model call is refused, so no further tokens are spent.
	if _, response := host.modelCall("不会到达"); response.Code == http.StatusOK {
		t.Fatal("a cancelled run still proxied a model call")
	}
	if calls := stub.callCount(); calls != 0 {
		t.Fatalf("the model was called %d times after cancellation", calls)
	}

	// The host confirms, and the run ends cancelled.
	if response := host.asHost(http.MethodPut, "/api/v1/agent/threads/"+host.threadID()+"/checkpoint", map[string]any{
		"checkpoint": "aG9zdC1jaGVja3BvaW50", "libfx_version": application.LibfxVersion, "stop_reason": "cancelled",
	}); response.Code != http.StatusOK {
		t.Fatalf("completion status=%d body=%s", response.Code, response.Body.String())
	}
	if got := waitRunStatus(t, api, api.user.ID, persistence.RunCancelled); got.Status != persistence.RunCancelled {
		t.Fatalf("run status=%q after an explicit cancel, want cancelled", got.Status)
	}
	select {
	case response := <-responses:
		if !parseSSE(t, response.Body.String()).done {
			t.Fatal("the command stream did not end with [DONE] after the run was cancelled")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the stream did not end after the run was cancelled")
	}
}

// TestAgentCancellationIsRefusedForAFinishedRun asserts cancelling a run that already ended is a
// no-op rather than a resurrection (§4).
func TestAgentCancellationIsRefusedForAFinishedRun(t *testing.T) {
	stub := newModelStub(t, stubTurn{text: "完成"})
	api := newHarnessAPI(t, stub)
	runID, responses := startHarnessRun(t, api, "帮我推进论文")
	host := newHarnessClient(t, api, runID)
	host.drive(2)
	streamResponse(t, responses)

	if response := host.cancel(); response.Code != http.StatusOK {
		t.Fatalf("cancellation status=%d body=%s", response.Code, response.Body.String())
	}
	if got := host.runStatus().Status; got != persistence.RunSucceeded {
		t.Fatalf("run status=%q, want it to stay succeeded", got)
	}
}

// TestAgentJobCancellationStillWorksForServerDrivenRuns keeps the pre-harness path honest: a run the
// Worker executes has no host to tell, so it is still cancelled through its job (agent-impl.md §4.3),
// and the run-level endpoint routes there.
func TestAgentJobCancellationStillWorksForServerDrivenRuns(t *testing.T) {
	// No worker: the job stays queued so the cancellation is deterministic.
	api := newTestAPI(t)
	disconnect, _, served := postDetached(t, api, "/api/v1/agent/commands", commandsBody("帮我推进论文"))
	defer disconnect()

	var run persistence.AgentRun
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := api.store.DB.Where("user_id = ?", api.user.ID).Order("created_at DESC").First(&run).Error; err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if run.ID == "" {
		t.Fatal("no run was created")
	}
	if run.HarnessMode != "" {
		t.Fatalf("harness_mode=%q, want the server-driven path when no model is configured", run.HarnessMode)
	}
	if run.JobID == "" {
		t.Fatal("a server-driven run has no job to cancel")
	}

	var job persistence.AgentJob
	if err := api.store.DB.Where("id = ?", run.JobID).First(&job).Error; err != nil {
		t.Fatalf("load job: %v", err)
	}
	response := api.do(t, http.MethodPut, "/api/v1/agent-jobs/"+job.ID+"/cancellation", nil,
		map[string]string{"If-Match": application.StrongETag("job", job.ID, job.Revision)})
	if response.Code != http.StatusOK {
		t.Fatalf("job cancellation=%d %s", response.Code, response.Body.String())
	}
	if err := api.store.DB.Where("id = ?", job.ID).First(&job).Error; err != nil {
		t.Fatal(err)
	}
	if !job.CancelRequested {
		t.Fatal("the job was not flagged for cancellation")
	}

	// The run-level endpoint reaches the same flag, so a client does not have to know which kind of
	// run it is looking at (§5.2).
	runCancel := api.do(t, http.MethodPost, "/api/v1/agent/runs/"+run.ID+"/cancellation", map[string]any{}, nil)
	if runCancel.Code != http.StatusOK {
		t.Fatalf("run cancellation=%d %s", runCancel.Code, runCancel.Body.String())
	}
	disconnect()
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream handler did not return after the client disconnected")
	}
}
