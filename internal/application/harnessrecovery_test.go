package application

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FastR-D/FastTask/internal/agent/protocol"
	"github.com/FastR-D/FastTask/internal/persistence"
)

func TestHarnessMultipleApprovalsAndDetachedReceipt(t *testing.T) {
	for _, detached := range []bool{false, true} {
		t.Run(map[bool]string{false: "two-pending", true: "detached"}[detached], func(t *testing.T) {
			svc, p, goal, proposal, part := approvalFixture(t)
			ctx := context.Background()
			var second persistence.AgentMessagePart
			if detached {
				svc.repo.UpdateRun(ctx, p.UserID, p.RunID, map[string]any{"status": persistence.RunInterrupted})
				svc.repo.RevokeHarnessTokensForRun(ctx, p.RunID)
			} else {
				duplicate := proposal
				duplicate.ID = persistence.NewID("proposal")
				if err := svc.store.DB.Create(&duplicate).Error; err != nil {
					t.Fatal(err)
				}
				second = part
				second.ID = ""
				second.ProposalID = &duplicate.ID
				id := "second-call"
				second.ToolCallID = &id
				if err := svc.repo.CreatePart(ctx, &second); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := svc.ResolveApproval(ctx, p.UserID, Command{Type: "add-tool-result", ToolCallID: *part.ToolCallID, Result: json.RawMessage(`{"decision":"approve"}`)}); err != nil {
				t.Fatal(err)
			}
			run, _ := svc.repo.GetRun(ctx, p.UserID, p.RunID)
			if detached {
				if run.Status != persistence.RunInterrupted {
					t.Fatal("detached host revived")
				}
				history, err := svc.harnessProxy.modelContext(ctx, run, "rules")
				if err != nil {
					t.Fatal(err)
				}
				encoded, _ := json.Marshal(history)
				if !strings.Contains(string(encoded), "applied") {
					t.Fatal("late approval lost")
				}
			} else {
				if run.Status != persistence.RunAwaitingApproval {
					t.Fatalf("remaining approval skipped: %s", run.Status)
				}
				pending, _ := svc.repo.GetPart(ctx, p.UserID, second.ID)
				if pending.ApprovalStatus != "pending" {
					t.Fatal("other proposal changed")
				}
				if _, err := svc.ResolveApproval(ctx, p.UserID, Command{Type: "add-tool-result", ToolCallID: *second.ToolCallID, Result: json.RawMessage(`{"decision":"reject"}`)}); err != nil {
					t.Fatal(err)
				}
				run, _ = svc.repo.GetRun(ctx, p.UserID, p.RunID)
				if run.Status != persistence.RunRunning {
					t.Fatal("did not resume after last receipt")
				}
				if err := svc.RequestCancellation(ctx, p.UserID, p.RunID); err != nil {
					t.Fatal(err)
				}
			}
			var actual persistence.Goal
			svc.store.DB.First(&actual, "id = ?", goal.ID)
			if actual.Title != "Approved title" {
				t.Fatal("committed result lost")
			}
		})
	}
}
func TestHarnessApprovalDeadlineReaper(t *testing.T) {
	svc, p, _, _, _ := approvalFixture(t)
	ctx := context.Background()
	now := persistence.Now()
	started := now.Add(-20 * time.Minute)
	svc.nowFn = func() time.Time { return now }
	if err := svc.repo.UpdateRun(ctx, p.UserID, p.RunID, map[string]any{"approval_started_at": started}); err != nil {
		t.Fatal(err)
	}
	count, err := svc.ReapHarnessRuns(ctx)
	if err != nil || count != 1 {
		t.Fatalf("reaper %d %v", count, err)
	}
	run, _ := svc.repo.GetRun(ctx, p.UserID, p.RunID)
	if run.Status != persistence.RunFailed || run.ErrorCode != "APPROVAL_TIMEOUT" {
		t.Fatalf("approval timeout: %+v", run)
	}
}
func TestHarnessTerminalWriteFailureRollsBack(t *testing.T) {
	svc, p, _ := toolIdentityFixture(t)
	ctx := context.Background()
	run, _ := svc.repo.GetRun(ctx, p.UserID, p.RunID)
	before, _ := svc.repo.CountChunks(ctx, p.UserID, p.RunID)
	if err := svc.store.DB.Exec(`CREATE TRIGGER reject_terminal_event BEFORE INSERT ON agent_run_chunks BEGIN SELECT RAISE(ABORT,'injected event failure'); END;`).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.harnessLifecycle.finishRun(ctx, run, persistence.RunFailed, "FIXTURE", "failure", protocol.ReasonError); err == nil {
		t.Fatal("terminal write failure hidden")
	}
	current, _ := svc.repo.GetRun(ctx, p.UserID, p.RunID)
	after, _ := svc.repo.CountChunks(ctx, p.UserID, p.RunID)
	if IsTerminalRunStatus(current.Status) || before != after {
		t.Fatal("partial terminal committed")
	}
	token, _ := svc.repo.GetHarnessToken(ctx, p.TokenID)
	if token.RevokedAt != nil {
		t.Fatal("token revoked despite rollback")
	}
	if err := svc.store.DB.Exec(`DROP TRIGGER reject_terminal_event`).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.harnessLifecycle.finishRun(ctx, run, persistence.RunFailed, "FIXTURE", "failure", protocol.ReasonError); err != nil {
		t.Fatal(err)
	}
}
func TestHarnessToolResultFailureIsUnconfirmed(t *testing.T) {
	svc, p, count := toolIdentityFixture(t)
	ctx := context.Background()
	if err := svc.store.DB.Exec(`CREATE TRIGGER reject_result_event BEFORE INSERT ON agent_run_chunks BEGIN SELECT RAISE(ABORT,'injected result event failure'); END;`).Error; err != nil {
		t.Fatal(err)
	}
	call := HarnessToolCall{ToolCallID: "verified-call", Input: map[string]any{"value": "one"}}
	out, err := svc.ExecuteHarnessTool(ctx, p, "fixture_read", call)
	if err != nil || !out.IsError {
		t.Fatalf("failed persistence reported success: %+v %v", out, err)
	}
	part, _ := svc.repo.GetPartByToolCallID(ctx, p.UserID, call.ToolCallID)
	if part.ResultJSON != "" || !part.ToolExecutionClaimed {
		t.Fatal("result event transaction inconsistent")
	}
	svc.store.DB.Exec(`DROP TRIGGER reject_result_event`)
	replay, err := svc.ExecuteHarnessTool(ctx, p, "fixture_read", call)
	if err != nil || !replay.IsError || count.Load() != 1 {
		t.Fatalf("unknown effect re-executed: %+v %v %d", replay, err, count.Load())
	}
}
func TestHarnessConcurrentTerminalAndCancellation(t *testing.T) {
	svc, p, _ := toolIdentityFixture(t)
	ctx := context.Background()
	run, _ := svc.repo.GetRun(ctx, p.UserID, p.RunID)
	if err := svc.RequestCancellation(ctx, p.UserID, p.RunID); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := svc.harnessLifecycle.finishRun(ctx, run, persistence.RunSucceeded, "", "", protocol.ReasonStop); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	current, _ := svc.repo.GetRun(ctx, p.UserID, p.RunID)
	if current.Status != persistence.RunCancelled {
		t.Fatalf("cancel lost terminal race: %s", current.Status)
	}
	chunks, _ := svc.repo.ListChunksFrom(ctx, p.UserID, p.RunID, 0)
	finished := 0
	for _, chunk := range chunks {
		if strings.Contains(chunk.ChunkJSON, `"isRunning"`) && strings.Contains(chunk.ChunkJSON, `false`) {
			finished++
		}
	}
	if finished != 1 {
		t.Fatalf("duplicate terminal: %d", finished)
	}
}
