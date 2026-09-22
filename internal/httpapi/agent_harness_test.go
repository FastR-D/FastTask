package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/FastR-D/FastTask/internal/application"
	"github.com/FastR-D/FastTask/internal/persistence"
)

// The harness contract over HTTP (doc/harness.md §14, doc/interface.md §20).
//
// These are the tests that would catch the mistakes the spec calls out by name: a tool list
// declared twice, a capability that works on the wrong endpoint, a credential that leaks, a host
// that disappears and takes a run with it.

// harnessTestClock is an injectable clock shared with the agent service, so the heartbeat-loss and
// approval-timeout paths can be tested without sleeping.
type harnessTestClock struct {
	now time.Time
}

func newHarnessTestClock() *harnessTestClock { return &harnessTestClock{now: persistence.Now()} }

func (c *harnessTestClock) At() time.Time { return c.now }

func (c *harnessTestClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

// newHarnessTestAPI builds an API whose agent runs on a controllable clock.
func newHarnessTestAPI(t *testing.T, stub *modelStub, clock *harnessTestClock, opts ...application.AgentOption) testAPI {
	t.Helper()
	base := []application.AgentOption{application.WithClock(clock.At)}
	return newHarnessAPI(t, stub, append(base, opts...)...)
}

// TestAgentToolsEndpointProjectsTheRegistry covers §3.4 and §14.2: the manifest a host builds its
// tools from is a projection of the server registry — same names, same order, and no identity field
// anywhere in a schema.
func TestAgentToolsEndpointProjectsTheRegistry(t *testing.T) {
	stub := newModelStub(t, stubTurn{text: "好。"})
	api := newHarnessAPI(t, stub)

	response := api.do(t, http.MethodGet, "/api/v1/agent/tools", nil, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("GET /agent/tools=%d %s", response.Code, response.Body.String())
	}
	if etag := response.Header().Get("ETag"); etag == "" {
		t.Fatal("the manifest carries no ETag, so a mid-run change would be undetectable (§10.3)")
	}
	var body struct {
		Tools []application.ToolDescriptor `json:"tools"`
	}
	decode(t, response, &body)

	registry := api.agent.Tools()
	want := registry.Names()
	if len(body.Tools) != len(want) {
		t.Fatalf("manifest has %d tools, the registry has %d", len(body.Tools), len(want))
	}
	for i, name := range want {
		if body.Tools[i].Name != name {
			t.Fatalf("manifest[%d]=%q, want the registry order (%q)", i, body.Tools[i].Name, name)
		}
		if body.Tools[i].Description == "" {
			t.Fatalf("tool %q has no description", name)
		}
		if body.Tools[i].InputSchema == nil {
			t.Fatalf("tool %q has no input schema", name)
		}
		encoded, _ := json.Marshal(body.Tools[i].InputSchema)
		for _, forbidden := range []string{"user_id", "userid", "workspace_id", "tenant_id", "owner_id", "actor", "principal", "identity", "on_behalf_of"} {
			if strings.Contains(string(encoded), `"`+forbidden+`"`) {
				t.Fatalf("tool %q advertises the identity field %q (§5.2): %s", name, forbidden, encoded)
			}
		}
	}
	// Both levels are advertised: a host that could not see the proposal tools could never propose.
	names := map[string]bool{}
	for _, tool := range body.Tools {
		names[tool.Name] = true
	}
	for _, required := range []string{"propose_task_tree_patch", "propose_daily_plan", "list_goals"} {
		if !names[required] {
			t.Fatalf("manifest is missing %q", required)
		}
	}
}

// TestHarnessTokenBoundaries covers §10.2, §14.5 and §14.16: the two subjects are not
// interchangeable, a token is bound to one run, and it dies with that run.
func TestHarnessTokenBoundaries(t *testing.T) {
	stub := newModelStub(t, stubTurn{text: "好。"}, stubTurn{text: "好。"})
	api := newHarnessAPI(t, stub)
	runID, responses := startHarnessRun(t, api, "第一个问题")
	host := newHarnessClient(t, api, runID)

	userJWT := map[string]string{"Authorization": "Bearer " + api.access}
	harnessAuth := map[string]string{"Authorization": "Bearer " + host.token}

	// A user JWT is not a run capability.
	for _, call := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/api/v1/agent/runs/" + runID + "/openai/chat/completions", hostModelBody("x")},
		{http.MethodPost, "/api/v1/agent/runs/" + runID + "/tools/list_goals", map[string]any{"tool_call_id": "c", "input": map[string]any{}}},
		{http.MethodPost, "/api/v1/agent/runs/" + runID + "/heartbeat", map[string]any{}},
		{http.MethodGet, "/api/v1/agent/runs/" + runID + "/approvals/prop_x", nil},
		{http.MethodGet, "/api/v1/agent/threads/" + host.threadID() + "/checkpoint", nil},
	} {
		response := api.do(t, call.method, call.path, call.body, userJWT)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s with a user JWT=%d, want 401 (%s)", call.method, call.path, response.Code, response.Body.String())
		}
	}

	// A run capability is not a user credential: it cannot mint tokens, list tools, submit commands
	// or cancel — cancelling is the user's power, not the host's (§10.2).
	for _, call := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/api/v1/agent/runs", map[string]any{"run_id": runID}},
		{http.MethodGet, "/api/v1/agent/tools", nil},
		{http.MethodPost, "/api/v1/agent/commands", harnessCommandsBody("第二个问题")},
		{http.MethodPost, "/api/v1/agent/runs/" + runID + "/cancellation", map[string]any{}},
	} {
		response := api.do(t, call.method, call.path, call.body, harnessAuth)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s with a harness token=%d, want 401 (%s)", call.method, call.path, response.Code, response.Body.String())
		}
	}

	// A token is bound to its run: the same token on another run's path is refused.
	other := api.do(t, http.MethodPost, "/api/v1/agent/runs/other_run/openai/chat/completions", hostModelBody("x"), harnessAuth)
	if other.Code != http.StatusUnauthorized {
		t.Fatalf("a token worked on another run's proxy=%d, want 401", other.Code)
	}
	// A fabricated token is refused.
	forged := api.do(t, http.MethodPost, "/api/v1/agent/runs/"+runID+"/heartbeat", map[string]any{},
		map[string]string{"Authorization": "Bearer fth_not-a-real-token"})
	if forged.Code != http.StatusUnauthorized {
		t.Fatalf("a forged token=%d, want 401", forged.Code)
	}

	// The token dies with the run (§10.2).
	host.drive(2)
	streamResponse(t, responses)
	if got := host.runStatus().Status; got != persistence.RunSucceeded {
		t.Fatalf("run status=%q, want succeeded", got)
	}
	dead := host.asHost(http.MethodPost, "/api/v1/agent/runs/"+runID+"/heartbeat", map[string]any{})
	if dead.Code != http.StatusUnauthorized {
		t.Fatalf("a token for a finished run=%d, want 401", dead.Code)
	}
	deadProxy := host.asHost(http.MethodPost, "/api/v1/agent/runs/"+runID+"/openai/chat/completions", hostModelBody("x"))
	if deadProxy.Code != http.StatusUnauthorized {
		t.Fatalf("a finished run still proxied a model call=%d, want 401", deadProxy.Code)
	}
}

// TestRunGrantRejectsStaleToolsETag covers §10.3 and §14.15: a host holding a manifest that no longer
// describes the server's tools is told to refetch, rather than being issued a capability it will
// misuse.
func TestRunGrantRejectsStaleToolsETag(t *testing.T) {
	stub := newModelStub(t, stubTurn{text: "好。"})
	api := newHarnessAPI(t, stub)
	runID, responses := startHarnessRun(t, api, "问题")

	response := api.do(t, http.MethodPost, "/api/v1/agent/runs", map[string]any{"run_id": runID, "tools_etag": `"stale-etag"`}, nil)
	if response.Code != http.StatusConflict {
		t.Fatalf("stale etag=%d, want 409 body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "TOOLS_ETAG_STALE") {
		t.Fatalf("the 409 does not carry TOOLS_ETAG_STALE: %s", response.Body.String())
	}
	// Without an etag the server does not compare, it only reports the current one (§10.3).
	first := api.do(t, http.MethodPost, "/api/v1/agent/runs", map[string]any{"run_id": runID}, nil)
	if first.Code != http.StatusOK {
		t.Fatalf("grant without an etag=%d %s", first.Code, first.Body.String())
	}
	var grant map[string]any
	decode(t, first, &grant)
	tools := api.do(t, http.MethodGet, "/api/v1/agent/tools", nil, nil)
	if grant["tools_etag"] != tools.Header().Get("ETag") {
		t.Fatalf("grant etag=%v, manifest etag=%q", grant["tools_etag"], tools.Header().Get("ETag"))
	}
	host := &harnessClient{t: t, api: api, runID: runID, token: grant["harness_token"].(string)}
	host.decide = func(string, string) map[string]any { return map[string]any{"decision": "approve"} }
	host.complete("stop")
	streamResponse(t, responses)
}

// TestRunGrantRefusesUnknownOrFinishedRuns covers §10.2: a capability is issued for one active run of
// one user, and nothing else.
func TestRunGrantRefusesUnknownOrFinishedRuns(t *testing.T) {
	stub := newModelStub(t, stubTurn{text: "好。"})
	api := newHarnessAPI(t, stub)

	unknown := api.do(t, http.MethodPost, "/api/v1/agent/runs", map[string]any{"run_id": "run_does-not-exist"}, nil)
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("grant for an unknown run=%d, want 404 body=%s", unknown.Code, unknown.Body.String())
	}

	runID, responses := startHarnessRun(t, api, "问题")
	host := newHarnessClient(t, api, runID)
	host.drive(2)
	streamResponse(t, responses)

	finished := api.do(t, http.MethodPost, "/api/v1/agent/runs", map[string]any{"run_id": runID}, nil)
	if finished.Code != http.StatusConflict {
		t.Fatalf("grant for a finished run=%d, want 409 body=%s", finished.Code, finished.Body.String())
	}
	if !strings.Contains(finished.Body.String(), "RUN_NOT_ACTIVE") {
		t.Fatalf("the 409 does not carry RUN_NOT_ACTIVE: %s", finished.Body.String())
	}

	// Another user cannot mint a capability for this run, and cannot tell it exists (§12).
	otherToken := agentSecondToken(t, api)
	crossUser := api.do(t, http.MethodPost, "/api/v1/agent/runs", map[string]any{"run_id": runID},
		map[string]string{"Authorization": "Bearer " + otherToken})
	if crossUser.Code != http.StatusNotFound {
		t.Fatalf("cross-user grant=%d, want 404", crossUser.Code)
	}
}

// TestHarnessHeartbeatKeepsARunAlive covers §10.4: a host that beats is not reaped, and a heartbeat
// response carries the run's cancellation flag.
func TestHarnessHeartbeatKeepsARunAlive(t *testing.T) {
	stub := newModelStub(t, stubTurn{text: "好。"})
	clock := newHarnessTestClock()
	api := newHarnessTestAPI(t, stub, clock)
	runID, responses := startHarnessRun(t, api, "问题")
	host := newHarnessClient(t, api, runID)

	for i := 0; i < 6; i++ {
		clock.Advance(10 * time.Second)
		beat := host.beat()
		if requested, _ := beat["cancel_requested"].(bool); requested {
			t.Fatal("a heartbeat reported a cancellation nobody asked for")
		}
		if _, ok := beat["expires_at"]; !ok {
			t.Fatalf("heartbeat response has no expires_at: %#v", beat)
		}
	}
	// Sixty seconds of silence would have reaped the run; beating kept it alive.
	if _, err := api.agent.ReapHarnessRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := host.runStatus().Status; got != persistence.RunRunning {
		t.Fatalf("run status=%q, want running (a beating host must not be reaped)", got)
	}
	host.drive(2)
	streamResponse(t, responses)
}

// TestHarnessHeartbeatLossInterruptsRun covers §10.4, §11 and §14.14: a host that stops beating loses
// its run, its capability, and leaves any pending proposal for the user to decide later.
func TestHarnessHeartbeatLossInterruptsRun(t *testing.T) {
	stub := newModelStub(t, stubTurn{
		text:      "我建议这样拆：",
		toolCalls: []stubCall{{id: "call_reap", name: "propose_task_tree_patch", arguments: `{"goal_id":"","instruction":"x","patch":[]}`}},
	})
	clock := newHarnessTestClock()
	api := newHarnessTestAPI(t, stub, clock)
	// A proposal needs a real goal, so the script is rebuilt after it exists.
	goalResp := api.do(t, http.MethodPost, "/api/v1/goals", map[string]any{"title": "完成论文", "success_criteria": "通过评审"}, map[string]string{"Idempotency-Key": "goal-reap"})
	var goal persistence.Goal
	decode(t, goalResp, &goal)
	patch := []map[string]any{{"op": "create", "type": "task", "client_ref": "n1", "title": "补充实验", "success_criteria": "可复现", "minimum_action": "跑一次"}}
	encoded, _ := json.Marshal(map[string]any{"goal_id": goal.ID, "instruction": "拆解", "patch": patch})
	stub.setTurns(stubTurn{text: "我建议这样拆：", toolCalls: []stubCall{{id: "call_reap", name: "propose_task_tree_patch", arguments: string(encoded)}}})

	runID, responses := startHarnessRun(t, api, "帮我拆解")
	host := newHarnessClient(t, api, runID)
	outcomes := host.modelTurn()
	if len(outcomes) != 1 || outcomes[0]["status"] != "pending" {
		t.Fatalf("outcomes=%#v, want a parked proposal", outcomes)
	}
	host.beat()

	// The host disappears: a closed tab, a refresh, a dead network (§10.4).
	clock.Advance(application.HarnessHeartbeatLoss + time.Second)
	reaped, err := api.agent.ReapHarnessRuns(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if reaped == 0 {
		t.Fatal("the reaper found nothing after the heartbeat was lost")
	}
	run := host.runStatus()
	if run.Status != persistence.RunInterrupted {
		t.Fatalf("run status=%q, want interrupted", run.Status)
	}
	// The capability died with the run.
	if response := host.asHost(http.MethodPost, "/api/v1/agent/runs/"+runID+"/heartbeat", map[string]any{}); response.Code != http.StatusUnauthorized {
		t.Fatalf("heartbeat after loss=%d, want 401", response.Code)
	}
	// The proposal is still pending: the user can decide later, but the run does not resume (§11).
	var pending int64
	api.store.DB.Model(&persistence.Proposal{}).Where("user_id = ? AND status = 'pending'", api.user.ID).Count(&pending)
	if pending != 1 {
		t.Fatalf("pending proposals=%d, want 1 (interrupting a run must not discard the proposal)", pending)
	}
	// The waiting long poll is released rather than holding a dead host's connection.
	waitResponse := host.asHost(http.MethodGet, "/api/v1/agent/runs/"+runID+"/approvals/"+outcomes[0]["proposal_id"].(string), nil)
	if waitResponse.Code == http.StatusOK {
		var wait map[string]any
		decode(t, waitResponse, &wait)
		if got, _ := wait["status"].(string); got == "pending" {
			t.Fatalf("the long poll is still waiting on an interrupted run: %#v", wait)
		}
	}
	select {
	case response := <-responses:
		if !parseSSE(t, response.Body.String()).done {
			t.Fatal("the command stream did not end after the run was interrupted")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the command stream never ended after the run was interrupted")
	}
}

// TestHarnessRunNeverPickedUpIsReaped covers the other half of §11: a run whose host never asked for a
// capability must not hold the user's single active run forever.
func TestHarnessRunNeverPickedUpIsReaped(t *testing.T) {
	stub := newModelStub(t, stubTurn{text: "好。"})
	clock := newHarnessTestClock()
	api := newHarnessTestAPI(t, stub, clock)
	runID, responses := startHarnessRun(t, api, "问题")

	if _, err := api.agent.ReapHarnessRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := hostRunStatus(t, api, runID).Status; got != persistence.RunQueued {
		t.Fatalf("run status=%q inside the pickup grace, want queued", got)
	}
	clock.Advance(2 * time.Minute)
	if _, err := api.agent.ReapHarnessRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := hostRunStatus(t, api, runID).Status; got != persistence.RunInterrupted {
		t.Fatalf("run status=%q after the pickup grace, want interrupted", got)
	}
	streamResponse(t, responses)
}

func hostRunStatus(t *testing.T, api testAPI, runID string) persistence.AgentRun {
	t.Helper()
	var run persistence.AgentRun
	if err := api.store.DB.Where("id = ?", runID).First(&run).Error; err != nil {
		t.Fatal(err)
	}
	return run
}

// TestApprovalWaitTimesOut covers §7: the total wait is enforced server-side, the tool result says
// APPROVAL_TIMEOUT, and the run fails rather than staying parked forever.
func TestApprovalWaitTimesOut(t *testing.T) {
	stub := newModelStub(t)
	api := newHarnessAPI(t, stub, application.WithApprovalWait(20*time.Millisecond, 60*time.Millisecond))
	goalResp := api.do(t, http.MethodPost, "/api/v1/goals", map[string]any{"title": "完成论文", "success_criteria": "通过评审"}, map[string]string{"Idempotency-Key": "goal-timeout"})
	var goal persistence.Goal
	decode(t, goalResp, &goal)
	stub.setTurns(proposalTurns(goal.ID, "不会到达")...)

	runID, responses := startHarnessRun(t, api, "帮我拆解")
	host := newHarnessClient(t, api, runID)
	outcomes := host.modelTurn()
	proposalID, _ := outcomes[0]["proposal_id"].(string)

	var last map[string]any
	for i := 0; i < 10; i++ {
		last = host.poll(proposalID)
		if status, _ := last["status"].(string); status != "pending" {
			break
		}
	}
	if got, _ := last["status"].(string); got != "timeout" {
		t.Fatalf("the wait ended as %q, want timeout (%#v)", got, last)
	}
	run := host.runStatus()
	if run.Status != persistence.RunFailed || run.ErrorCode != "APPROVAL_TIMEOUT" {
		t.Fatalf("run=%q code=%q, want failed/APPROVAL_TIMEOUT", run.Status, run.ErrorCode)
	}
	// The proposal stays pending: the user can still see what was asked (§7).
	var pending int64
	api.store.DB.Model(&persistence.Proposal{}).Where("user_id = ? AND status = 'pending'", api.user.ID).Count(&pending)
	if pending != 1 {
		t.Fatalf("pending proposals=%d, want 1", pending)
	}
	streamResponse(t, responses)
}

// TestCheckpointVersionSkewDegrades covers §6.3 and §14.7: a checkpoint written by another libfx
// version is not handed back, the instructions carry a rebuilt summary instead, and the grant says so
// so the UI can tell the user their history was compressed.
func TestCheckpointVersionSkewDegrades(t *testing.T) {
	stub := newModelStub(t, stubTurn{text: "第一轮回答。"}, stubTurn{text: "第二轮回答。"})
	api := newHarnessAPI(t, stub)

	// First run: the host reports a checkpoint at the end of its turn.
	firstRun, firstResponses := startHarnessRun(t, api, "第一个问题")
	firstHost := newHarnessClient(t, api, firstRun)
	firstHost.drive(2)
	streamResponse(t, firstResponses)
	threadID := firstHost.threadID()
	if got := firstHost.runStatus().Status; got != persistence.RunSucceeded {
		t.Fatalf("first run status=%q, want succeeded", got)
	}

	// A deployment rolls forward and the stored checkpoint is from an older libfx.
	if err := api.store.DB.Model(&persistence.AgentThread{}).Where("id = ?", threadID).
		Update("libfx_version", "0.0.1").Error; err != nil {
		t.Fatal(err)
	}

	secondRun, secondResponses := startHarnessRunInThread(t, api, "第二个问题", threadID)
	grant := api.do(t, http.MethodPost, "/api/v1/agent/runs", map[string]any{"run_id": secondRun}, nil)
	if grant.Code != http.StatusOK {
		t.Fatalf("grant=%d %s", grant.Code, grant.Body.String())
	}
	var body map[string]any
	decode(t, grant, &body)
	if degraded, _ := body["context_degraded"].(bool); !degraded {
		t.Fatalf("grant=%#v, want context_degraded on a checkpoint the running version cannot read", body)
	}
	instructions, _ := body["instructions"].(string)
	if !strings.Contains(instructions, "历史上下文摘要") || !strings.Contains(instructions, "第一轮回答") {
		t.Fatalf("the degraded instructions carry no rebuilt summary: %q", instructions)
	}
	if len(instructions) > application.InstructionsLimit {
		t.Fatalf("instructions are %d bytes, over the 64 KiB libfx cap (§3.5)", len(instructions))
	}

	// The unreadable checkpoint is not handed back to a host (§6.3).
	token, _ := body["harness_token"].(string)
	read := api.do(t, http.MethodGet, "/api/v1/agent/threads/"+threadID+"/checkpoint", nil,
		map[string]string{"Authorization": "Bearer " + token})
	if read.Code != http.StatusOK {
		t.Fatalf("checkpoint read=%d %s", read.Code, read.Body.String())
	}
	var checkpoint struct {
		Checkpoint   string `json:"checkpoint"`
		LibfxVersion string `json:"libfx_version"`
		Skew         bool   `json:"skew"`
	}
	decode(t, read, &checkpoint)
	if !checkpoint.Skew {
		t.Fatalf("checkpoint=%#v, want skew reported", checkpoint)
	}
	if checkpoint.Checkpoint != "" {
		t.Fatal("an unreadable checkpoint was handed back to the host")
	}
	if checkpoint.LibfxVersion != "0.0.1" {
		t.Fatalf("libfx_version=%q, want the stored one so the host can log the skew", checkpoint.LibfxVersion)
	}

	host := &harnessClient{t: t, api: api, runID: secondRun, token: token}
	host.decide = func(string, string) map[string]any { return map[string]any{"decision": "approve"} }
	host.drive(2)
	streamResponse(t, secondResponses)
	// The turn wrote a checkpoint the running version can read, so the next grant is not degraded.
	third := api.do(t, http.MethodPost, "/api/v1/agent/runs", map[string]any{"run_id": secondRun}, nil)
	if third.Code != http.StatusConflict {
		t.Fatalf("a grant for a finished run=%d, want 409", third.Code)
	}
	var thread persistence.AgentThread
	if err := api.store.DB.Where("id = ?", threadID).First(&thread).Error; err != nil {
		t.Fatal(err)
	}
	if thread.LibfxVersion != application.LibfxVersion || len(thread.Checkpoint) == 0 {
		t.Fatalf("thread checkpoint=%d bytes version=%q, want a current one", len(thread.Checkpoint), thread.LibfxVersion)
	}
}

// TestProxyReplacesHostSystemAndTools covers §4.3 and §14.4 over HTTP: a request carrying a hostile
// system prompt and an extra tool reaches the provider with neither.
func TestProxyReplacesHostSystemAndTools(t *testing.T) {
	stub := newModelStub(t, stubTurn{text: "好。"})
	api := newHarnessAPI(t, stub)
	runID, responses := startHarnessRun(t, api, "问题")
	host := newHarnessClient(t, api, runID)

	if _, response := host.modelCall("问题"); response.Code != http.StatusOK {
		t.Fatalf("proxy=%d %s", response.Code, response.Body.String())
	}
	request := stub.request(t, 0)
	messages, _ := request["messages"].([]any)
	if len(messages) == 0 {
		t.Fatal("the provider saw no messages")
	}
	system, _ := messages[0].(map[string]any)
	content, _ := system["content"].(string)
	if system["role"] != "system" || strings.Contains(content, "HOST INJECTED") || !strings.Contains(content, "最多三个") {
		t.Fatalf("the provider saw %#v, want the server's own system prompt", system)
	}
	tools, _ := request["tools"].([]any)
	for _, item := range tools {
		entry, _ := item.(map[string]any)
		function, _ := entry["function"].(map[string]any)
		if function["name"] == "delete_everything" {
			t.Fatal("a host-declared tool reached the provider")
		}
	}
	if len(tools) != len(api.agent.Tools().Names()) {
		t.Fatalf("the provider saw %d tools, want the registry's %d", len(tools), len(api.agent.Tools().Names()))
	}
	if request["model"] != "qwen3.8-max" {
		t.Fatalf("model=%v, want the server's", request["model"])
	}
	if got := stub.authorization(0); got != "Bearer "+stubAPIKey {
		t.Fatalf("upstream authorization=%q, want the injected provider key", got)
	}
	host.complete("stop")
	streamResponse(t, responses)
}

// TestHarnessResponsesNeverCarryCredentials covers §4.5 and §14.17: no response a host can read
// contains the provider key or the upstream address.
func TestHarnessResponsesNeverCarryCredentials(t *testing.T) {
	stub := newModelStub(t, stubTurn{text: "好。", reasoning: "想一想"})
	api := newHarnessAPI(t, stub)
	runID, responses := startHarnessRun(t, api, "问题")
	host := newHarnessClient(t, api, runID)

	proxied := host.asHost(http.MethodPost, "/api/v1/agent/runs/"+runID+"/openai/chat/completions", hostModelBody("问题"))
	grant := host.asUser(http.MethodGet, "/api/v1/agent/tools", nil)
	beat := host.asHost(http.MethodPost, "/api/v1/agent/runs/"+runID+"/heartbeat", map[string]any{})
	checkpoint := host.asHost(http.MethodGet, "/api/v1/agent/threads/"+host.threadID()+"/checkpoint", nil)
	host.complete("stop")
	stream := streamResponse(t, responses)

	for name, body := range map[string]string{
		"proxy":      proxied.Body.String(),
		"tools":      grant.Body.String(),
		"heartbeat":  beat.Body.String(),
		"checkpoint": checkpoint.Body.String(),
		"stream":     stream.Body.String(),
	} {
		for _, secret := range []string{stubAPIKey, stub.server.URL} {
			if strings.Contains(body, secret) {
				t.Fatalf("the %s response leaked %q", name, secret)
			}
		}
	}
}

// TestReasoningIsPersistedAndCanBeDisabled covers doc/chat-features.md §3.3 and §3.4: reasoning deltas
// become their own part so a message can hold several segments, and the operator's switch drops them
// entirely rather than storing empty ones.
func TestReasoningIsPersistedAndCanBeDisabled(t *testing.T) {
	stub := newModelStub(t, stubTurn{text: "回答。", reasoning: "先看看目标"})
	api := newHarnessAPI(t, stub)
	runID, responses := startHarnessRun(t, api, "问题")
	host := newHarnessClient(t, api, runID)
	host.drive(2)
	stream := parseSSE(t, streamResponse(t, responses).Body.String())
	if !strings.Contains(stream.body, `"reasoning"`) {
		t.Fatalf("the stream carried no reasoning part: %s", stream.body)
	}
	parts := harnessParts(t, api, runID)
	var reasoning int
	for _, part := range parts {
		if part.Type == "reasoning" {
			reasoning++
			if part.Text != "先看看目标" {
				t.Fatalf("reasoning text=%q", part.Text)
			}
		}
	}
	if reasoning != 1 {
		t.Fatalf("reasoning parts=%d, want 1", reasoning)
	}

	// With persistence off, nothing is stored and nothing is streamed (§3.4: the UI must not render an
	// empty folded block).
	offStub := newModelStub(t, stubTurn{text: "回答。", reasoning: "先看看目标"})
	offAPI := newHarnessAPI(t, offStub, application.WithReasoningPersistence(false))
	offRun, offResponses := startHarnessRun(t, offAPI, "问题")
	offHost := newHarnessClient(t, offAPI, offRun)
	offHost.drive(2)
	offStream := parseSSE(t, streamResponse(t, offResponses).Body.String())
	if strings.Contains(offStream.body, `"reasoning"`) {
		t.Fatalf("reasoning was streamed although persistence is off: %s", offStream.body)
	}
	for _, part := range harnessParts(t, offAPI, offRun) {
		if part.Type == "reasoning" {
			t.Fatalf("a reasoning part was stored although persistence is off: %#v", part)
		}
	}
}

// harnessParts returns every persisted part of a run's assistant message.
func harnessParts(t *testing.T, api testAPI, runID string) []persistence.AgentMessagePart {
	t.Helper()
	var parts []persistence.AgentMessagePart
	err := api.store.DB.
		Joins("JOIN agent_messages ON agent_messages.id = agent_message_parts.message_id").
		Where("agent_messages.run_id = ?", runID).
		Order("agent_message_parts.idx").
		Find(&parts).Error
	if err != nil {
		t.Fatal(err)
	}
	return parts
}

// TestSidecarModeIsRefusedWithoutASidecar covers §1.2 and §14.10: with no sidecar deployed, asking for
// one is refused — while the WASM path keeps working, which is the spike's reversal of the old
// "no sidecar, no agent" rule.
func TestSidecarModeIsRefusedWithoutASidecar(t *testing.T) {
	stub := newModelStub(t, stubTurn{text: "好。"})
	api := newHarnessAPI(t, stub)

	response := api.do(t, http.MethodPost, "/api/v1/agent/commands", map[string]any{
		"commands":     harnessCommandsBody("问题")["commands"],
		"threadId":     nil,
		"harness_mode": application.HarnessModeSidecar,
	}, nil)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("sidecar mode without a sidecar=%d, want 503 body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "HARNESS_UNAVAILABLE") {
		t.Fatalf("the 503 does not carry HARNESS_UNAVAILABLE: %s", response.Body.String())
	}
	// The same browser, same server, WASM mode: unaffected.
	runID, responses := startHarnessRun(t, api, "问题")
	host := newHarnessClient(t, api, runID)
	host.drive(2)
	if got := host.runStatus().Status; got != persistence.RunSucceeded {
		t.Fatalf("run status=%q, want succeeded without a sidecar", got)
	}
	streamResponse(t, responses)
}

// TestHarnessRunReportsItsModeAndRunID covers §1.2: the response header names the run a host must
// drive, and the streamed state carries the same id for a browser that cannot read headers.
func TestHarnessRunReportsItsModeAndRunID(t *testing.T) {
	stub := newModelStub(t, stubTurn{text: "好。"})
	api := newHarnessAPI(t, stub)

	encoded, _ := json.Marshal(harnessCommandsBody("问题"))
	request := httptest.NewRequest(http.MethodPost, "/api/v1/agent/commands", strings.NewReader(string(encoded)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+api.access)
	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		api.server.Engine.ServeHTTP(recorder, request)
	}()

	var runID string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && runID == "" {
		var run persistence.AgentRun
		if err := api.store.DB.
			Where("user_id = ? AND harness_mode = ? AND status = ?", api.user.ID, application.HarnessModeWASM, persistence.RunQueued).
			Order("created_at DESC").First(&run).Error; err == nil && hasAssistantMessage(api, run.ID) {
			runID = run.ID
		}
		time.Sleep(5 * time.Millisecond)
	}
	if runID == "" {
		t.Fatal("no harness run was created")
	}
	host := newHarnessClient(t, api, runID)
	host.drive(2)
	<-done

	if got := recorder.Header().Get("X-Harness-Run"); got != runID {
		t.Fatalf("X-Harness-Run=%q, want %q", got, runID)
	}
	if got := recorder.Header().Get("X-Harness-Mode"); got != application.HarnessModeWASM {
		t.Fatalf("X-Harness-Mode=%q, want wasm", got)
	}
	stream := parseSSE(t, recorder.Body.String())
	state := applyOps(t, stream.ops)
	streamedRunID, ok := getAt(state, []string{"fasttask", "runId"})
	if !ok || streamedRunID != runID {
		t.Fatalf("streamed fasttask.runId=%v, want %q (a browser cannot read the header)", streamedRunID, runID)
	}
}
