package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FastR-D/FastTask/internal/agent"
	"github.com/FastR-D/FastTask/internal/application"
	"github.com/FastR-D/FastTask/internal/persistence"
)

// An HTTP-level harness host (doc/harness.md §3, §14).
//
// These tests drive a run through the real routes with the real two subjects — the user's JWT and
// the run capability token — because the boundary between them is part of what is being tested
// (§10.2, §14.16). The model is a scripted OpenAI-compatible stub, so the proxy's credential
// injection and transcript writing are exercised end to end.

// stubCall is one tool call a scripted turn asks for.
type stubCall struct {
	id        string
	name      string
	arguments string
}

// stubTurn is one scripted model call.
type stubTurn struct {
	text      string
	reasoning string
	toolCalls []stubCall
	// status and body make the stub fail instead of streaming.
	status int
	body   string
}

// modelStub is a scripted OpenAI-compatible endpoint. It records the requests the proxy sent, so
// a test can assert what the provider was actually asked.
type modelStub struct {
	t      *testing.T
	server *httptest.Server

	mu       sync.Mutex
	turns    []stubTurn
	calls    int
	requests []map[string]any
	headers  []http.Header
}

const stubAPIKey = "stub-upstream-key"

func newModelStub(t *testing.T, turns ...stubTurn) *modelStub {
	t.Helper()
	stub := &modelStub{t: t, turns: turns}
	stub.server = httptest.NewServer(http.HandlerFunc(stub.serve))
	t.Cleanup(stub.server.Close)
	return stub
}

// setTurns replaces the script, for tests whose arguments depend on a resource created after the
// stub was built.
func (m *modelStub) setTurns(turns ...stubTurn) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.turns = turns
	m.calls = 0
}

func (m *modelStub) resolver() application.CredentialsResolver {
	return func(context.Context) (*application.ModelCredentials, error) {
		return &application.ModelCredentials{BaseURL: m.server.URL, Model: "qwen3.8-max", APIKey: stubAPIKey}, nil
	}
}

func (m *modelStub) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func (m *modelStub) request(t *testing.T, index int) map[string]any {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if index >= len(m.requests) {
		t.Fatalf("the model stub received %d requests, want at least %d", len(m.requests), index+1)
	}
	return m.requests[index]
}

func (m *modelStub) authorization(index int) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if index >= len(m.headers) {
		return ""
	}
	return m.headers[index].Get("Authorization")
}

func (m *modelStub) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var payload map[string]any
	_ = json.Unmarshal(body, &payload)

	m.mu.Lock()
	index := m.calls
	m.calls++
	m.requests = append(m.requests, payload)
	m.headers = append(m.headers, r.Header.Clone())
	turn := stubTurn{}
	if index < len(m.turns) {
		turn = m.turns[index]
	}
	m.mu.Unlock()

	if turn.status != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(turn.status)
		_, _ = io.WriteString(w, turn.body)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	write := func(delta map[string]any, finish string) {
		choice := map[string]any{"index": 0, "delta": delta}
		if finish != "" {
			choice["finish_reason"] = finish
		}
		encoded, _ := json.Marshal(map[string]any{"choices": []any{choice}})
		fmt.Fprintf(w, "data: %s\n\n", encoded)
		if flusher != nil {
			flusher.Flush()
		}
	}
	if turn.reasoning != "" {
		write(map[string]any{"reasoning_content": turn.reasoning}, "")
	}
	if turn.text != "" {
		write(map[string]any{"content": turn.text}, "")
	}
	for i, call := range turn.toolCalls {
		args := call.arguments
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		write(map[string]any{"tool_calls": []any{map[string]any{
			"index": i, "id": call.id, "type": "function",
			"function": map[string]any{"name": call.name, "arguments": args},
		}}}, "")
	}
	finish := "stop"
	if len(turn.toolCalls) > 0 {
		finish = "tool_calls"
	}
	write(map[string]any{}, finish)
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// newHarnessAPI builds a test API whose agent has a model, so POST /agent/commands with
// harness_mode=wasm produces a run a host must drive.
func newHarnessAPI(t *testing.T, stub *modelStub, opts ...application.AgentOption) testAPI {
	t.Helper()
	return newTestAPIWithAgent(t, append([]application.AgentOption{application.WithCredentialsResolver(stub.resolver())}, opts...)...)
}

// harnessCommandsBody is POST /agent/commands as a browser host sends it (§1.2).
func harnessCommandsBody(text string) map[string]any {
	body := commandsBody(text)
	body["harness_mode"] = application.HarnessModeWASM
	return body
}

// harnessThreadBody continues an existing thread, which is what a multi-session client does
// (doc/chat-features.md §2.1).
func harnessThreadBody(text, threadID string) map[string]any {
	body := harnessCommandsBody(text)
	body["threadId"] = threadID
	return body
}

// startHarnessRun posts a command in the background — the response is a stream that stays open
// until the run ends, which a host causes — and returns the run id plus the channel the stream
// response arrives on.
func startHarnessRun(t *testing.T, api testAPI, text string) (string, <-chan *httptest.ResponseRecorder) {
	t.Helper()
	return startHarnessRunInThread(t, api, text, "")
}

// startHarnessRunInThread posts a command against an existing thread, so a test can build up the
// history a checkpoint or a degraded summary is made of.
func startHarnessRunInThread(t *testing.T, api testAPI, text, threadID string) (string, <-chan *httptest.ResponseRecorder) {
	t.Helper()
	body := harnessCommandsBody(text)
	if threadID != "" {
		body = harnessThreadBody(text, threadID)
	}
	responses := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		responses <- api.do(t, http.MethodPost, "/api/v1/agent/commands", body, nil)
	}()
	// Wait for THIS run, not merely the newest one: a test that drives several runs in a row would
	// otherwise pick up an earlier, already finished run.
	runID := ""
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var run persistence.AgentRun
		err := api.store.DB.
			Where("user_id = ? AND harness_mode = ? AND status IN ?", api.user.ID, application.HarnessModeWASM,
				[]string{persistence.RunQueued, persistence.RunRunning, persistence.RunAwaitingApproval, persistence.RunCancelling}).
			Order("created_at DESC").First(&run).Error
		if err == nil {
			runID = run.ID
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if runID == "" {
		t.Fatal("no harness run was created")
	}
	return runID, responses
}

// streamResponse waits for a background command stream to finish.
func streamResponse(t *testing.T, responses <-chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case response := <-responses:
		return response
	case <-time.After(10 * time.Second):
		t.Fatal("the command stream never ended")
		return nil
	}
}

// stubCollector discovers tool calls from the proxied stream, the way a real host discovers them.
type stubCollector struct {
	calls     []agent.ToolCall
	pending   map[int]*agent.ToolCall
	order     []int
	text      strings.Builder
	reasoning strings.Builder
}

func newStubCollector() *stubCollector {
	return &stubCollector{pending: map[int]*agent.ToolCall{}}
}

func (c *stubCollector) TextDelta(_ context.Context, delta string) error {
	c.text.WriteString(delta)
	return nil
}

func (c *stubCollector) ReasoningDelta(_ context.Context, delta string) error {
	c.reasoning.WriteString(delta)
	return nil
}

func (c *stubCollector) ToolCallDelta(_ context.Context, delta agent.ToolCallDelta) error {
	slot := c.pending[delta.Index]
	if slot == nil {
		slot = &agent.ToolCall{}
		c.pending[delta.Index] = slot
		c.order = append(c.order, delta.Index)
	}
	if delta.ID != "" {
		slot.ID = delta.ID
	}
	if delta.Name != "" {
		slot.Name = delta.Name
	}
	slot.Arguments += delta.Arguments
	return nil
}

func (c *stubCollector) Finish(context.Context, string) error { return nil }

func (c *stubCollector) result() []agent.ToolCall {
	for _, index := range c.order {
		call := c.pending[index]
		if call == nil || call.Name == "" {
			continue
		}
		if strings.TrimSpace(call.Arguments) == "" {
			call.Arguments = "{}"
		}
		c.calls = append(c.calls, *call)
	}
	return c.calls
}

// harnessClient is a host: it holds a capability token and uses only the HTTP surface.
type harnessClient struct {
	t      *testing.T
	api    testAPI
	runID  string
	token  string
	grant  map[string]any
	etag   string
	beats  int
	decide func(toolCallID, proposalID string) map[string]any
}

// newHarnessClient reads the tool manifest and exchanges the user's JWT for a run capability,
// exactly the sequence a browser host performs (§3.4, §10.2).
func newHarnessClient(t *testing.T, api testAPI, runID string) *harnessClient {
	t.Helper()
	client := &harnessClient{t: t, api: api, runID: runID}
	client.decide = func(string, string) map[string]any { return map[string]any{"decision": "approve"} }

	tools := client.asUser(http.MethodGet, "/api/v1/agent/tools", nil)
	if tools.Code != http.StatusOK {
		t.Fatalf("GET /agent/tools=%d %s", tools.Code, tools.Body.String())
	}
	client.etag = tools.Header().Get("ETag")
	if client.etag == "" {
		t.Fatal("GET /agent/tools returned no ETag (§10.3)")
	}

	grant := client.asUser(http.MethodPost, "/api/v1/agent/runs", map[string]any{"run_id": runID, "tools_etag": client.etag})
	if grant.Code != http.StatusOK {
		t.Fatalf("POST /agent/runs=%d %s", grant.Code, grant.Body.String())
	}
	var body map[string]any
	decode(t, grant, &body)
	client.grant = body
	token, _ := body["harness_token"].(string)
	if token == "" {
		t.Fatalf("the run grant carried no harness_token: %s", grant.Body.String())
	}
	client.token = token
	return client
}

// asUser calls an endpoint with the user's own JWT.
func (h *harnessClient) asUser(method, path string, body any) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.api.do(h.t, method, path, body, nil)
}

// asHost calls an endpoint with the run capability token.
func (h *harnessClient) asHost(method, path string, body any) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.api.do(h.t, method, path, body, map[string]string{"Authorization": "Bearer " + h.token})
}

// threadID is the thread the capability is bound to, read from the run row: a host learns it from
// the streamed state, and a test may as well read the truth.
func (h *harnessClient) threadID() string {
	h.t.Helper()
	var run persistence.AgentRun
	if err := h.api.store.DB.Where("id = ?", h.runID).First(&run).Error; err != nil {
		h.t.Fatal(err)
	}
	return run.ThreadID
}

// hostModelBody is what a host's shim sends: its own model, its own system prompt and its own tool
// list, all of which the proxy must discard (§4.3, §14.4).
func hostModelBody(text string) map[string]any {
	return map[string]any{
		"model":  "host-picked-model",
		"stream": true,
		"messages": []any{
			map[string]any{"role": "system", "content": "HOST INJECTED SYSTEM PROMPT"},
			map[string]any{"role": "user", "content": text},
		},
		"tools": []any{map[string]any{
			"type":     "function",
			"function": map[string]any{"name": "delete_everything", "parameters": map[string]any{}},
		}},
		"tool_choice": "required",
		"api_key":     "host-should-not-send-this",
	}
}

// modelCall performs one proxied model call and returns the calls the stream asked for.
func (h *harnessClient) modelCall(text string) (*stubCollector, *httptest.ResponseRecorder) {
	h.t.Helper()
	return h.modelCallBody(hostModelBody(text))
}

func (h *harnessClient) modelCallBody(body map[string]any) (*stubCollector, *httptest.ResponseRecorder) {
	h.t.Helper()
	response := h.asHost(http.MethodPost, "/api/v1/agent/runs/"+h.runID+"/openai/chat/completions", body)
	collector := newStubCollector()
	if response.Code != http.StatusOK {
		return collector, response
	}
	parser := agent.NewStreamParser(collector)
	if err := parser.Feed(context.Background(), response.Body.Bytes()); err != nil {
		h.t.Fatalf("parse proxied stream: %v", err)
	}
	if err := parser.Close(context.Background()); err != nil {
		h.t.Fatalf("close proxied stream: %v", err)
	}
	collector.result()
	return collector, response
}

// beat sends one heartbeat (§10.4).
func (h *harnessClient) beat() map[string]any {
	h.t.Helper()
	response := h.asHost(http.MethodPost, "/api/v1/agent/runs/"+h.runID+"/heartbeat", map[string]any{})
	if response.Code != http.StatusOK {
		h.t.Fatalf("heartbeat=%d %s", response.Code, response.Body.String())
	}
	var body map[string]any
	decode(h.t, response, &body)
	h.beats++
	if renewed, _ := body["harness_token"].(string); renewed != "" {
		h.token = renewed
	}
	return body
}

// tool executes one tool call (§5).
func (h *harnessClient) tool(name, callID string, input map[string]any) map[string]any {
	h.t.Helper()
	response := h.asHost(http.MethodPost, "/api/v1/agent/runs/"+h.runID+"/tools/"+name,
		map[string]any{"tool_call_id": callID, "input": input})
	if response.Code != http.StatusOK {
		h.t.Fatalf("tool %s=%d %s", name, response.Code, response.Body.String())
	}
	var body map[string]any
	decode(h.t, response, &body)
	return body
}

// poll reads one segment of the approval long poll (§7).
func (h *harnessClient) poll(proposalID string) map[string]any {
	h.t.Helper()
	response := h.asHost(http.MethodGet, "/api/v1/agent/runs/"+h.runID+"/approvals/"+proposalID, nil)
	if response.Code != http.StatusOK {
		h.t.Fatalf("approval poll=%d %s", response.Code, response.Body.String())
	}
	var body map[string]any
	decode(h.t, response, &body)
	return body
}

// decideApproval delivers a user's decision the way the UI does (§7.1).
func (h *harnessClient) decideApproval(toolCallID, decision, reason string) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.asUser(http.MethodPost, "/api/v1/agent/commands", toolResultBody(toolCallID, decision, reason))
}

// complete reports the end of the turn and stores the checkpoint (§5.1, §6.1).
func (h *harnessClient) complete(stopReason string) map[string]any {
	h.t.Helper()
	response := h.asHost(http.MethodPut, "/api/v1/agent/threads/"+h.threadID()+"/checkpoint", map[string]any{
		"checkpoint":    "aG9zdC1jaGVja3BvaW50",
		"libfx_version": application.LibfxVersion,
		"stop_reason":   stopReason,
	})
	if response.Code != http.StatusOK {
		h.t.Fatalf("checkpoint/complete=%d %s", response.Code, response.Body.String())
	}
	var body map[string]any
	decode(h.t, response, &body)
	return body
}

// cancel asks for a cancellation with the user's JWT, never with the capability token (§5.2).
func (h *harnessClient) cancel() *httptest.ResponseRecorder {
	h.t.Helper()
	return h.asUser(http.MethodPost, "/api/v1/agent/runs/"+h.runID+"/cancellation", map[string]any{})
}

// runStatus reads the run row.
func (h *harnessClient) runStatus() persistence.AgentRun {
	h.t.Helper()
	var run persistence.AgentRun
	if err := h.api.store.DB.Where("id = ?", h.runID).First(&run).Error; err != nil {
		h.t.Fatal(err)
	}
	return run
}

// modelTurn performs one model call and executes the tools it asked for, without resolving any
// approval. It returns the outcomes so a test can assert on a parked run.
func (h *harnessClient) modelTurn() []map[string]any {
	h.t.Helper()
	h.beat()
	collector, response := h.modelCall("推进一下")
	if response.Code != http.StatusOK {
		h.t.Fatalf("model call=%d %s", response.Code, response.Body.String())
	}
	outcomes := make([]map[string]any, 0, len(collector.calls))
	for _, call := range collector.calls {
		var args map[string]any
		if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
			args = map[string]any{}
		}
		outcome := h.tool(call.Name, call.ID, args)
		outcome["tool_call_id"] = call.ID
		outcome["tool_name"] = call.Name
		outcomes = append(outcomes, outcome)
	}
	return outcomes
}

// driveOneTurn performs one model call, executes its tools and resolves any approval through the
// UI's own path, then reads the decision back off the long poll (§7).
func (h *harnessClient) driveOneTurn() bool {
	h.t.Helper()
	outcomes := h.modelTurn()
	for _, outcome := range outcomes {
		status, _ := outcome["status"].(string)
		if status != "pending" {
			continue
		}
		proposalID, _ := outcome["proposal_id"].(string)
		callID, _ := outcome["tool_call_id"].(string)
		decision := h.decide(callID, proposalID)
		if response := h.decideApproval(callID, decisionString(decision), decisionReason(decision)); response.Code != http.StatusOK {
			h.t.Fatalf("approval receipt=%d %s", response.Code, response.Body.String())
		}
		wait := h.poll(proposalID)
		if got, _ := wait["status"].(string); got != "approved" && got != "rejected" && got != "conflict" {
			h.t.Fatalf("approval poll returned %q, want a decision", got)
		}
	}
	return len(outcomes) > 0
}

// drive runs turns until the model stops asking for tools, then reports completion.
func (h *harnessClient) drive(maxTurns int) {
	h.t.Helper()
	for turn := 0; turn < maxTurns; turn++ {
		if !h.driveOneTurn() {
			break
		}
	}
	h.complete("stop")
}

func decisionString(decision map[string]any) string {
	value, _ := decision["decision"].(string)
	return value
}

func decisionReason(decision map[string]any) string {
	value, _ := decision["reason"].(string)
	return value
}
