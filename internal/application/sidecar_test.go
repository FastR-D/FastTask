package application

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/FastR-D/FastTask/internal/agent"
	"github.com/FastR-D/FastTask/internal/persistence"
)

// The sidecar host path (doc/harness.md §8, §14.3).
//
// §14.3 is the assertion ADR-0005 §2 rests on: the two hosts are interchangeable because neither of them
// writes the transcript — the server does, from the model proxy. These tests drive the same conversation
// through both and compare what landed in the database.

// fakeSidecar is a stand-in for the Node process. It behaves like the real one: it takes a capability
// token and a prompt, drives the turn through the same server surface a browser uses, and reports what the
// turn ended with.
type fakeSidecar struct {
	svc      *AgentService
	upstream *fakeUpstream
	healthy  bool
	driven   []SidecarRun
	cancels  []string
}

func (f *fakeSidecar) Healthy(context.Context) bool { return f.healthy }

func (f *fakeSidecar) Cancel(_ context.Context, runID string) error {
	f.cancels = append(f.cancels, runID)
	return nil
}

func (f *fakeSidecar) Drive(ctx context.Context, run SidecarRun) (SidecarResult, error) {
	f.driven = append(f.driven, run)
	principal, err := f.svc.AuthenticateHarness(ctx, "Bearer "+run.HarnessToken)
	if err != nil {
		return SidecarResult{}, err
	}
	for turn := 0; turn < 4; turn++ {
		stream, err := f.svc.OpenModelProxy(ctx, principal, hostRequestBody(run.Prompt))
		if err != nil {
			var proxyErr *ProxyError
			if asProxy(err, &proxyErr) && proxyErr.Code == "MAX_TURNS" {
				break
			}
			return SidecarResult{StopReason: "error", ErrorMessage: err.Error()}, err
		}
		calls, err := drainForCalls(ctx, stream)
		if err != nil {
			return SidecarResult{StopReason: "error", ErrorMessage: err.Error()}, err
		}
		if len(calls) == 0 {
			break
		}
		for _, call := range calls {
			var args map[string]any
			_ = json.Unmarshal([]byte(call.Arguments), &args)
			outcome, err := f.svc.ExecuteHarnessTool(ctx, principal, call.Name, HarnessToolCall{ToolCallID: call.ID, Input: args})
			if err != nil {
				return SidecarResult{}, err
			}
			if outcome.Status != "pending" {
				continue
			}
			// A sidecar has no page to render an approval card on, which is exactly why §7 chose a long
			// poll over an in-page event.
			decision, _ := json.Marshal(map[string]any{"decision": "approve"})
			if _, err := f.svc.ResolveApproval(ctx, principal.UserID, Command{
				Type: "add-tool-result", ToolCallID: call.ID, Result: decision,
			}); err != nil {
				return SidecarResult{}, err
			}
			if _, err := f.svc.WaitForApproval(ctx, principal, outcome.ProposalID, 5*time.Second); err != nil {
				return SidecarResult{}, err
			}
		}
	}
	if err := f.svc.CompleteRun(ctx, principal, Completion{
		StopReason: "stop", Checkpoint: []byte("sidecar-checkpoint"), LibfxVersion: LibfxVersion,
	}); err != nil {
		return SidecarResult{}, err
	}
	return SidecarResult{StopReason: "stop", Checkpoint: []byte("sidecar-checkpoint"), LibfxVersion: LibfxVersion}, nil
}

// drainForCalls reads a proxied stream the way a host does, collecting the tool calls it asked for.
func drainForCalls(ctx context.Context, stream *ModelProxyStream) ([]agent.ToolCall, error) {
	collector := newCallCollector()
	parser := agent.NewStreamParser(collector)
	buffer := make([]byte, 4096)
	for {
		n, err := stream.Body.Read(buffer)
		if n > 0 {
			if feedErr := parser.Feed(ctx, buffer[:n]); feedErr != nil {
				_ = stream.Body.Close()
				return nil, feedErr
			}
		}
		if err != nil {
			break
		}
	}
	if err := parser.Close(ctx); err != nil {
		_ = stream.Body.Close()
		return nil, err
	}
	_ = stream.Body.Close()
	return collector.result(), nil
}

// submitSidecarRun creates a run in sidecar mode. Unlike a wasm run, this one DOES create a job: the Worker
// is what calls the sidecar (§1.2).
func submitSidecarRun(t *testing.T, svc *AgentService, store *persistence.Store, userID, text string) persistence.AgentJob {
	t.Helper()
	submitted, err := svc.SubmitCommands(context.Background(), userID, CommandsRequest{
		HarnessMode: HarnessModeSidecar,
		Commands: []Command{{Type: "add-message", Message: &CommandMessage{
			Role: "user", Parts: []CommandPart{{Type: "text", Text: text}},
		}}},
	})
	if err != nil {
		t.Fatalf("SubmitCommands: %v", err)
	}
	if submitted.HarnessMode != HarnessModeSidecar {
		t.Fatalf("harness mode=%q, want sidecar", submitted.HarnessMode)
	}
	var job persistence.AgentJob
	if err := store.DB.Where("subject_id = ?", submitted.RunID).First(&job).Error; err != nil {
		t.Fatalf("a sidecar run must create the job the Worker turns into a sidecar call (§1.2): %v", err)
	}
	return job
}

func TestSidecarRunIsDrivenByTheWorker(t *testing.T) {
	upstream := newFakeUpstream(t,
		scriptedTurn{text: "让我看看目标", toolCalls: []agent.ToolCall{{ID: "call_1", Name: "list_goals", Arguments: `{"status":"active"}`}}},
		scriptedTurn{text: "你有一个活跃目标。"},
	)
	f := newFixture(t)
	// The service is built once, with the driver: the fake sidecar drives through the same instance the
	// Worker calls, exactly as the real one drives through the same HTTP surface.
	sidecar := &fakeSidecar{upstream: upstream, healthy: true}
	svc := NewAgentService(f.app, WithCredentialsResolver(upstream.resolver()), WithSidecarDriver(sidecar))
	sidecar.svc = svc

	job := submitSidecarRun(t, svc, f.store, f.user.ID, "我有哪些目标")
	worker := NewWorker(f.app, time.Millisecond).WithAgentRunner(svc)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if runStatus(t, svc, f.user.ID, job.SubjectID).Status == persistence.RunSucceeded {
			break
		}
		if err := worker.RunOnce(context.Background()); err != nil {
			t.Fatalf("worker: %v", err)
		}
	}

	run := runStatus(t, svc, f.user.ID, job.SubjectID)
	if run.Status != persistence.RunSucceeded {
		t.Fatalf("run status=%q code=%q message=%q, want succeeded", run.Status, run.ErrorCode, run.ErrorMessage)
	}
	if len(sidecar.driven) != 1 {
		t.Fatalf("the sidecar was called %d times, want 1", len(sidecar.driven))
	}
	driven := sidecar.driven[0]
	if driven.Prompt != "我有哪些目标" {
		t.Fatalf("prompt=%q, want the user's message read from the authoritative parts", driven.Prompt)
	}
	if !strings.HasPrefix(driven.HarnessToken, "fth_") {
		t.Fatalf("the sidecar got %q, want a run capability token", driven.HarnessToken)
	}
	// The transcript was written by the proxy, not reported by the sidecar (§8.3).
	parts := assistantParts(t, svc, f.user.ID, job.SubjectID)
	if len(partsOfType(parts, "tool-call")) != 1 {
		t.Fatalf("tool-call parts=%#v, want the one the model asked for", parts)
	}
	if !strings.Contains(concatText(parts), "你有一个活跃目标") {
		t.Fatalf("the answer was not persisted: %q", concatText(parts))
	}
	thread, err := svc.Repository().GetThread(context.Background(), f.user.ID, run.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if string(thread.Checkpoint) != "sidecar-checkpoint" {
		t.Fatalf("checkpoint=%q, want the one the sidecar reported", thread.Checkpoint)
	}
}

// TestBothHostsWriteTheSameTranscript is §14.3: the same conversation driven by a browser host and by a
// sidecar host must produce the same authoritative parts. This is the property that makes the sidecar a
// fallback rather than a second implementation.
//
// Both runs use one user and one goal, and run one after the other because a user has at most one active
// run (§9.3). Sharing the data is what makes the comparison meaningful: a tool result that names a goal has
// to name the SAME goal on both sides, or the test would be comparing fixtures instead of hosts.
func TestBothHostsWriteTheSameTranscript(t *testing.T) {
	// A provider generates a fresh call id per call, so the two scripts differ in exactly that: everything
	// else about the conversation is identical, which is what the comparison is about.
	script := func(callID string) []scriptedTurn {
		return []scriptedTurn{
			{text: "让我看看目标", reasoning: "先查活跃目标", toolCalls: []agent.ToolCall{{ID: callID, Name: "list_goals", Arguments: `{"status":"active"}`}}},
			{text: "你有一个活跃目标：Finish paper。"},
		}
	}
	f := newFixture(t)
	const question = "我有哪些目标"

	// Host A: the browser path, driven through the same surface the real host uses.
	upstreamA := newFakeUpstream(t, script("call_wasm_1")...)
	svcA := NewAgentService(f.app, WithCredentialsResolver(upstreamA.resolver()))
	runA := submitHarnessRun(t, svcA, f.store, f.user.ID, question)
	hostA := newTestHost(t, svcA, upstreamA, f.user.ID, runA)
	hostA.drive(4)
	if got := runStatus(t, svcA, f.user.ID, runA).Status; got != persistence.RunSucceeded {
		t.Fatalf("the wasm run status=%q, want succeeded", got)
	}

	// Host B: the sidecar path, driven by the Worker through a fake Node process against the same data.
	upstreamB := newFakeUpstream(t, script("call_sidecar_1")...)
	sidecar := &fakeSidecar{upstream: upstreamB, healthy: true}
	svcB := NewAgentService(f.app, WithCredentialsResolver(upstreamB.resolver()), WithSidecarDriver(sidecar))
	sidecar.svc = svcB
	jobB := submitSidecarRun(t, svcB, f.store, f.user.ID, question)
	worker := NewWorker(f.app, time.Millisecond).WithAgentRunner(svcB)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if runStatus(t, svcB, f.user.ID, jobB.SubjectID).Status == persistence.RunSucceeded {
			break
		}
		if err := worker.RunOnce(context.Background()); err != nil {
			t.Fatalf("worker: %v", err)
		}
	}
	if got := runStatus(t, svcB, f.user.ID, jobB.SubjectID).Status; got != persistence.RunSucceeded {
		t.Fatalf("the sidecar run status=%q, want succeeded", got)
	}

	// The two transcripts must agree on everything a reader can observe: part order, kinds, tool names,
	// arguments, results and text. Ids and timestamps are per-run and are not part of the claim.
	a := normalizeParts(t, assistantParts(t, svcA, f.user.ID, runA))
	b := normalizeParts(t, assistantParts(t, svcB, f.user.ID, jobB.SubjectID))
	// The call ids differ by construction (a provider never repeats one), so they are normalized away;
	// everything else must match exactly.
	a, b = withoutCallIDs(a), withoutCallIDs(b)
	if len(a) != len(b) {
		t.Fatalf("the hosts produced %d and %d parts:\nA=%s\nB=%s", len(a), len(b), a, b)
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("part %d differs between hosts:\n wasm=%s\n sidecar=%s", i, a[i], b[i])
		}
	}
	if len(a) < 4 {
		t.Fatalf("the transcript is too short to prove anything: %s", a)
	}
	// Each host stored its own checkpoint on its own thread, which is what makes the conversation portable
	// the same way in both modes (§6.1).
	for _, runID := range []string{runA, jobB.SubjectID} {
		run, err := svcA.Repository().GetRun(context.Background(), f.user.ID, runID)
		if err != nil {
			t.Fatal(err)
		}
		thread, err := svcA.Repository().GetThread(context.Background(), f.user.ID, run.ThreadID)
		if err != nil {
			t.Fatal(err)
		}
		if len(thread.Checkpoint) == 0 || thread.LibfxVersion != LibfxVersion {
			t.Fatalf("run %s left no usable checkpoint on its thread: %#v", runID, thread)
		}
	}
}

// withoutCallIDs drops the tool call ids from a normalized transcript: a provider generates a fresh one per
// call, so two hosts running the same conversation cannot share them and the ids say nothing about
// equivalence.
func withoutCallIDs(lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if !strings.HasPrefix(line, "tool-call ") {
			out = append(out, line)
			continue
		}
		out = append(out, "tool-call "+strings.Join(strings.Fields(line)[2:], " "))
	}
	return out
}

// normalizeParts renders a transcript as comparable lines, dropping the per-run identity that cannot be
// equal between two runs.
func normalizeParts(t *testing.T, parts []persistence.AgentMessagePart) []string {
	t.Helper()
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		switch part.Type {
		case "tool-call":
			var args, result any
			_ = json.Unmarshal([]byte(part.ArgsJSON), &args)
			if part.ResultJSON != "" {
				_ = json.Unmarshal([]byte(part.ResultJSON), &result)
			}
			encodedArgs, _ := json.Marshal(args)
			encodedResult, _ := json.Marshal(result)
			out = append(out, "tool-call "+part.ToolName+" "+string(encodedArgs)+" "+string(encodedResult)+" approval="+part.ApprovalStatus)
		default:
			out = append(out, part.Type+" "+part.Text)
		}
	}
	return out
}

func TestSidecarModeRefusedWhenUnhealthy(t *testing.T) {
	upstream := newFakeUpstream(t, scriptedTurn{text: "不会到达"})
	f := newFixture(t)
	sidecar := &fakeSidecar{healthy: false}
	svc := NewAgentService(f.app, WithCredentialsResolver(upstream.resolver()), WithSidecarDriver(sidecar))

	_, err := svc.SubmitCommands(context.Background(), f.user.ID, CommandsRequest{
		HarnessMode: HarnessModeSidecar,
		Commands: []Command{{Type: "add-message", Message: &CommandMessage{
			Role: "user", Parts: []CommandPart{{Type: "text", Text: "你好"}},
		}}},
	})
	if err == nil || !strings.Contains(err.Error(), "harness is unavailable") {
		t.Fatalf("err=%v, want HARNESS_UNAVAILABLE rather than a job nobody will run (§1.2)", err)
	}
	var jobs int64
	f.store.DB.Model(&persistence.AgentJob{}).Where("type = ?", "agent_run").Count(&jobs)
	if jobs != 0 {
		t.Fatalf("%d agent_run jobs were queued for a refused sidecar run", jobs)
	}
	// The same user on the WASM path is unaffected (§14.10).
	runID := submitHarnessRun(t, svc, f.store, f.user.ID, "你好")
	if runID == "" {
		t.Fatal("the wasm path stopped working when the sidecar was unhealthy")
	}
}

// TestToolCallIDCollisionIsRefused pins the guard the equivalence test surfaced: a tool call id belongs to
// one run, and a second run reusing it must fail rather than write into the first run's transcript
// (§4.4.1's dedup key is (run_id, tool_call_id), and the unique index is global).
func TestToolCallIDCollisionIsRefused(t *testing.T) {
	upstream := newFakeUpstream(t,
		scriptedTurn{toolCalls: []agent.ToolCall{{ID: "call_shared", Name: "list_goals", Arguments: `{}`}}},
		scriptedTurn{text: "好"},
		scriptedTurn{toolCalls: []agent.ToolCall{{ID: "call_shared", Name: "list_goals", Arguments: `{}`}}},
		scriptedTurn{text: "好"},
	)
	f, svc := harnessFixture(t, upstream)

	firstRun := submitHarnessRun(t, svc, f.store, f.user.ID, "第一次")
	firstHost := newTestHost(t, svc, upstream, f.user.ID, firstRun)
	firstHost.drive(4)
	before := assistantParts(t, svc, f.user.ID, firstRun)

	secondRun := submitHarnessRun(t, svc, f.store, f.user.ID, "第二次")
	secondHost := newTestHost(t, svc, upstream, f.user.ID, secondRun)
	_, err := secondHost.modelCall("第二次")
	if err == nil || !strings.Contains(err.Error(), "another run") {
		t.Fatalf("err=%v, want a tool call id collision", err)
	}
	// The first run's transcript is untouched, which is the whole point of refusing.
	after := assistantParts(t, svc, f.user.ID, firstRun)
	if len(after) != len(before) {
		t.Fatalf("the first run gained parts: %d -> %d", len(before), len(after))
	}
}
