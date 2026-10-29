package application

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/FastR-D/FastTask/internal/persistence"
)

func approvalFixture(t *testing.T) (*AgentService, HarnessPrincipal, persistence.Goal, persistence.Proposal, persistence.AgentMessagePart) {
	t.Helper()
	svc, p := providerHarness(t, func(http.ResponseWriter, *http.Request) {}, LoopLimits{})
	ctx := context.Background()
	goal := persistence.Goal{Title: "Original", SuccessCriteria: "Verified report"}
	if err := svc.app.CreateGoal(ctx, p.UserID, &goal); err != nil {
		t.Fatal(err)
	}
	proposal := persistence.Proposal{ID: persistence.NewID("proposal"), UserID: p.UserID, GoalID: goal.ID, Kind: "goal_update", Status: "pending", BaseRevision: goal.Revision, Revision: 1, PatchJSON: `{"changes":{"title":"Approved title"}}`}
	if err := svc.store.DB.Create(&proposal).Error; err != nil {
		t.Fatal(err)
	}
	run, _ := svc.repo.GetRun(ctx, p.UserID, p.RunID)
	sess, err := svc.harnessLifecycle.loadRunSession(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	id := "approval-call"
	part := persistence.AgentMessagePart{UserID: p.UserID, MessageID: sess.assistantID, Type: "tool-call", ToolCallID: &id, ToolName: "propose_goal_update", ArgsJSON: `{}`, ApprovalStatus: "pending", ProposalID: &proposal.ID}
	if err = svc.repo.CreatePart(ctx, &part); err != nil {
		t.Fatal(err)
	}
	if err = svc.repo.UpdateRun(ctx, p.UserID, p.RunID, map[string]any{"status": persistence.RunAwaitingApproval}); err != nil {
		t.Fatal(err)
	}
	return svc, p, goal, proposal, part
}
func TestHarnessApprovalReceipts(t *testing.T) {
	for _, decision := range []string{"approve", "reject", "conflict", "cancel", "cross-user"} {
		t.Run(decision, func(t *testing.T) {
			svc, p, goal, proposal, part := approvalFixture(t)
			ctx := context.Background()
			switch decision {
			case "conflict":
				svc.store.DB.Model(&goal).Update("revision", goal.Revision+1)
			case "cancel":
				if err := svc.RequestCancellation(ctx, p.UserID, p.RunID); err != nil {
					t.Fatal(err)
				}
			}
			user := p.UserID
			if decision == "cross-user" {
				user = "panel-user-b"
			}
			action := decision
			if action != "reject" {
				action = "approve"
			}
			payload, _ := json.Marshal(map[string]any{"decision": action, "reason": "Changed scope"})
			cmd := Command{Type: "add-tool-result", ToolCallID: *part.ToolCallID, Result: payload}
			_, err := svc.ResolveApproval(ctx, user, cmd)
			if decision == "approve" || decision == "reject" {
				if err != nil {
					t.Fatal(err)
				}
				if _, err = svc.ResolveApproval(ctx, user, cmd); !errors.Is(err, ErrApprovalDuplicate) {
					t.Fatalf("duplicate: %v", err)
				}
			} else if err == nil {
				t.Fatal("invalid receipt succeeded")
			}
			if decision == "conflict" {
				messages, _ := svc.repo.ListThreadMessages(ctx, p.UserID, p.ThreadID)
				visible := false
				for _, message := range messages {
					parts, _ := svc.repo.ListMessageParts(ctx, p.UserID, message.ID)
					for _, part := range parts {
						if part.Type == "text" && strings.Contains(part.Text, "提案未生效") {
							visible = true
						}
					}
				}
				if !visible {
					t.Fatal("conflict has no visible explanation")
				}
			}
			var actual persistence.Goal
			svc.store.DB.First(&actual, "id = ?", goal.ID)
			current, _ := svc.repo.GetPart(ctx, p.UserID, part.ID)
			if decision == "approve" {
				if actual.Title != "Approved title" || current.ApprovalStatus != "approved" {
					t.Fatalf("missing approved truth: %+v %+v", actual, current)
				}
			} else if actual.Title != "Original" {
				t.Fatal("unapproved domain write")
			}
			var count int64
			svc.store.DB.Model(&persistence.Proposal{}).Where("id = ? AND status = 'applied'", proposal.ID).Count(&count)
			if (decision == "approve") != (count == 1) {
				t.Fatal("proposal mismatch")
			}
		})
	}
}
func TestHarnessApprovalTransactionRollbackAndTwoServices(t *testing.T) {
	svc, p, goal, proposal, part := approvalFixture(t)
	ctx := context.WithValue(context.WithValue(context.Background(), approvalRunKey{}, p.RunID), approvalPartKey{}, "missing-part")
	if _, err := svc.app.ApplyGoalUpdateProposal(ctx, p.UserID, &proposal); err == nil {
		t.Fatal("receipt failure committed domain state")
	}
	var unchanged persistence.Goal
	svc.store.DB.First(&unchanged, "id = ?", goal.ID)
	if unchanged.Title != goal.Title {
		t.Fatal("domain did not roll back")
	}
	other := NewAgentService(svc.app)
	var wg sync.WaitGroup
	out := make(chan error, 2)
	for _, service := range []*AgentService{svc, other} {
		wg.Add(1)
		go func(s *AgentService) {
			defer wg.Done()
			_, err := s.ResolveApproval(context.Background(), p.UserID, Command{Type: "add-tool-result", ToolCallID: *part.ToolCallID, Result: json.RawMessage(`{"decision":"approve"}`)})
			out <- err
		}(service)
	}
	wg.Wait()
	close(out)
	passed := 0
	for err := range out {
		if err == nil {
			passed++
		}
	}
	if passed != 1 {
		t.Fatalf("receipts committed: %d", passed)
	}
	current, _ := svc.repo.GetPart(context.Background(), p.UserID, part.ID)
	if current.ApprovalStatus != "approved" {
		t.Fatalf("duplicate overwrote receipt: %+v", current)
	}
}
