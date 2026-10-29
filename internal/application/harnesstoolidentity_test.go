package application

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/FastR-D/FastTask/internal/agent"
	"github.com/FastR-D/FastTask/internal/persistence"
)

func toolIdentityFixture(t *testing.T) (*AgentService, HarnessPrincipal, *atomic.Int32) {
	t.Helper()
	svc, p := providerHarness(t, func(w http.ResponseWriter, r *http.Request) {
		frame := map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "verified-call", "type": "function", "function": map[string]any{"name": "fixture_read", "arguments": `{"value":"one"}`}}}}, "finish_reason": "tool_calls"}}}
		b, _ := json.Marshal(frame)
		io.WriteString(w, "data: "+string(b)+"\n\ndata: [DONE]\n\n")
	}, LoopLimits{})
	count := &atomic.Int32{}
	registry, err := NewToolRegistry(funcTool{def: agent.ToolDefinition{Name: "fixture_read", Parameters: objectSchema(map[string]any{"value": stringProp("fixture")}, "value")}, fn: func(context.Context, ToolContext, map[string]any) (ToolResult, error) {
		count.Add(1)
		return ToolResult{Result: map[string]any{"value": "result"}}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	svc.tools = registry
	stream, err := svc.OpenModelProxy(context.Background(), p, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.Copy(io.Discard, stream.Body); err != nil {
		t.Fatal(err)
	}
	stream.Body.Close()
	return svc, p, count
}
func TestHarnessVerifiedToolIdentityAndReplay(t *testing.T) {
	svc, p, count := toolIdentityFixture(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name, id string
		args     map[string]any
	}{
		{"fixture_read", "", map[string]any{"value": "one"}},
		{"fixture_read", "forged-call", map[string]any{"value": "one"}},
		{"another_tool", "verified-call", map[string]any{"value": "one"}},
		{"fixture_read", "verified-call", map[string]any{"value": "tampered"}},
	} {
		out, err := svc.ExecuteHarnessTool(ctx, p, tc.name, HarnessToolCall{ToolCallID: tc.id, Input: tc.args})
		if err == nil && !out.IsError {
			t.Fatalf("identity accepted: %+v", tc)
		}
	}
	if count.Load() != 0 {
		t.Fatal("forged identity executed")
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := svc.ExecuteHarnessTool(ctx, p, "fixture_read", HarnessToolCall{ToolCallID: "verified-call", Input: map[string]any{"value": "one"}})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if count.Load() != 1 {
		t.Fatalf("execution count: %d", count.Load())
	}
	out, err := svc.ExecuteHarnessTool(ctx, p, "fixture_read", HarnessToolCall{ToolCallID: "verified-call", Input: map[string]any{"value": "one"}})
	if err != nil || out.Status != "ok" {
		t.Fatalf("retry: %+v %v", out, err)
	}
	part, _ := svc.repo.GetPartByToolCallID(ctx, p.UserID, "verified-call")
	if part.ResultJSON == "" || !part.ToolExecutionClaimed {
		t.Fatal("result not durable")
	}
}
func TestHarnessUnknownClaimAndCancelledClaim(t *testing.T) {
	for _, mode := range []string{"unknown", "cancel", "old-turn", "stale-generation"} {
		t.Run(mode, func(t *testing.T) {
			svc, p, count := toolIdentityFixture(t)
			ctx := context.Background()
			part, _ := svc.repo.GetPartByToolCallID(ctx, p.UserID, "verified-call")
			switch mode {
			case "unknown":
				svc.store.DB.Model(part).Update("tool_execution_claimed", true)
			case "cancel":
				svc.RequestCancellation(ctx, p.UserID, p.RunID)
			case "old-turn":
				svc.repo.UpdateRun(ctx, p.UserID, p.RunID, map[string]any{"model_calls": 2})
			case "stale-generation":
				svc.IssueBrowserRunGrant(ctx, p.UserID, p.RunID, "")
			}
			out, err := svc.ExecuteHarnessTool(ctx, p, "fixture_read", HarnessToolCall{ToolCallID: "verified-call", Input: map[string]any{"value": "one"}})
			if err == nil && !out.IsError {
				t.Fatalf("unsafe execution: %+v", out)
			}
			if count.Load() != 0 {
				t.Fatal("unconfirmed call executed")
			}
		})
	}
}
func TestHarnessCompletionRequiresVerifiedFinalStop(t *testing.T) {
	for _, reason := range []string{"stop", "end_turn", "unknown", ""} {
		t.Run(reason, func(t *testing.T) {
			svc, p, _ := toolIdentityFixture(t)
			if err := svc.CompleteRun(context.Background(), p, Completion{StopReason: reason}); err != nil {
				t.Fatal(err)
			}
			run, _ := svc.repo.GetRun(context.Background(), p.UserID, p.RunID)
			if run.Status == persistence.RunSucceeded {
				t.Fatal("host skipped tools and forged success")
			}
		})
	}
	svc, p := providerHarness(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"verified\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}, LoopLimits{})
	stream, err := svc.OpenModelProxy(context.Background(), p, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, stream.Body)
	stream.Body.Close()
	if err = svc.CompleteRun(context.Background(), p, Completion{StopReason: "end_turn"}); err != nil {
		t.Fatal(err)
	}
	run, _ := svc.repo.GetRun(context.Background(), p.UserID, p.RunID)
	if run.Status != persistence.RunSucceeded {
		t.Fatalf("verified completion: %+v", run)
	}
	count, _ := svc.repo.CountChunks(context.Background(), p.UserID, p.RunID)
	svc.CompleteRun(context.Background(), p, Completion{StopReason: "error", ErrorMessage: "late"})
	after, _ := svc.repo.CountChunks(context.Background(), p.UserID, p.RunID)
	if after != count {
		t.Fatal("duplicate terminal emitted")
	}
	if strings.Contains(run.UsageJSON, "fixture-private-key") {
		t.Fatal("secret in usage")
	}
}
