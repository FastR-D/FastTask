package application

import (
	"context"

	"github.com/FastR-D/FastTask/internal/persistence"
)

// The places where the product decides a user should be told something
// (doc/notification.md §10).
//
// They are free functions rather than methods on purpose. AgentService is already at the
// method budget doc/wiring.md §9 sets, and a notification is not part of what the agent
// runtime does: it is one line at the moment a run parks or fails, and keeping it out of
// the struct keeps that obvious.
//
// Every hook is fire-and-forget. The event it announces has already been committed, and
// a notification is a courtesy — a provider that is down must not fail a run, roll back a
// proposal, or turn a completed action into an error the user has to retry.

// notifyProposalPending tells a user that a decision is waiting. This is the one
// notification the product cannot do without: a parked run holds its thread's single
// active slot (doc/agent.md §4), so a user who closed the tab has no way to find out the
// agent is waiting for them, and no way to start another run in that thread.
func notifyProposalPending(ctx context.Context, app *App, run *persistence.AgentRun, proposal *PendingProposalRef) {
	if app == nil || app.NotificationService == nil || run == nil || proposal == nil {
		return
	}
	body := proposal.Summary
	if body == "" {
		body = "有一个任务树变更提案等待确认"
	}
	_, _ = app.NotificationService.Publish(ctx, run.UserID, Event{
		Topic: TopicProposalPending,
		Title: "有待确认的任务树变更",
		Body:  body,
		Data: map[string]string{
			"proposal_id": proposal.ProposalID, "goal_id": proposal.GoalID,
			"run_id": run.ID, "thread_id": run.ThreadID,
		},
		// One banner per proposal, however many channels it reaches.
		CollapseID: "proposal-" + proposal.ProposalID,
	})
}

// notifyRunFailed tells a user a run did not finish. Without it a failed run is visible
// only in the thread the user is no longer looking at.
func notifyRunFailed(ctx context.Context, app *App, run *persistence.AgentRun, code, detail string) {
	if app == nil || app.NotificationService == nil || run == nil {
		return
	}
	_, _ = app.NotificationService.Publish(ctx, run.UserID, Event{
		Topic: TopicRunFailed,
		Title: "对话任务未完成",
		Body:  runFailureText(code, detail),
		Data:  map[string]string{"run_id": run.ID, "thread_id": run.ThreadID, "code": code},
		// A run that fails repeatedly should not fill the lock screen: the newest failure
		// replaces the older one.
		CollapseID: "run-" + run.ID,
	})
}

// runFailureText is the one line a banner can carry. The code is always present and is
// what an operator searches the logs for; the provider's own detail follows when there is
// room, clamped by normalizeEvent.
func runFailureText(code, detail string) string {
	if detail == "" {
		return "错误码 " + code + "，回到对话可以重试。"
	}
	return "错误码 " + code + "：" + detail
}
