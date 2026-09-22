package application

import (
	"context"
	"time"

	"github.com/FastR-D/FastTask/internal/agent/protocol"
	"github.com/FastR-D/FastTask/internal/persistence"
)

// Run setup for a harness host (doc/harness.md §1.2, §5.1).
//
// The in-process path built its session and immediately drove a loop. A harness run has no
// loop here, so the same setup is exposed on its own: create the run, emit the preamble the
// UI renders from, and let the host drive everything after it.

// BeginHarnessRun writes the preamble of a host-driven run: isRunning, the thread and run
// identity, the replayed history and the empty assistant message the run streams into
// (§2.7.1). The runId is part of the emitted state on purpose — assistant-ui owns the fetch
// that creates the run, so the browser cannot read the X-Harness-Run response header and
// learns which run to drive from the stream instead.
func (s *AgentService) BeginHarnessRun(ctx context.Context, userID, runID string) error {
	run, err := s.repo.GetRun(ctx, userID, runID)
	if err != nil {
		return err
	}
	sess, err := s.beginRun(ctx, persistence.AgentJob{}, run)
	if err != nil {
		return err
	}
	sess.state.FastTask.RunID = run.ID
	if err := sess.emitSet(protocol.FastTaskRunIDPath(), run.ID); err != nil {
		return err
	}
	return sess.saveState()
}

// loadRunSession rebuilds a run's streaming session from the authoritative rows without
// emitting anything: the client already holds the earlier chunks, and re-emitting them would
// re-apply append-text and duplicate text on screen (doc/agent-impl.md §2.8). It returns a
// nil session when the run has no assistant message yet, which callers treat as "status
// only, nothing to stream into".
func (s *harnessLifecycle) loadRunSession(ctx context.Context, run *persistence.AgentRun) (*runSession, error) {
	sink := &dbSink{repo: s.repo(), userID: run.UserID, run: run.ID}
	// Continue the chunk log where it ended so the resume cursor stays correct even when this
	// session emits nothing.
	if count, err := s.repo().CountChunks(ctx, run.UserID, run.ID); err == nil {
		sink.lastSeq = int(count)
	}
	sess := &runSession{sessionStream: &sessionStream{
		svc: s.svc, ctx: ctx, userID: run.UserID, run: run, sink: sink, state: protocol.NewState(),
	}}
	sess.state.IsRunning = true
	sess.state.FastTask.ThreadID = run.ThreadID
	sess.state.FastTask.RunID = run.ID

	messages, err := s.repo().ListThreadMessages(ctx, run.UserID, run.ThreadID)
	if err != nil {
		return nil, err
	}
	for i, m := range messages {
		status := protocol.CompleteStatus("")
		if m.RunID == run.ID && m.Role == "assistant" {
			status = protocol.RunningStatus()
			sess.assistantID = m.ID
			sess.assistantIdx = i
		}
		pm, err := s.svc.toProtocolMessage(ctx, run.UserID, m, status)
		if err != nil {
			return nil, err
		}
		sess.state.Messages = append(sess.state.Messages, pm)
	}
	if sess.assistantID == "" {
		return nil, nil
	}
	sess.state.FastTask.PendingProposals = s.svc.pendingProposals(ctx, run.UserID, run.ThreadID)
	sess.userText = lastUserText(sess.state.Messages)
	return sess, nil
}

// CancelRunJob flags the AgentJob carrying a non-harness run. The in-process loop polls it
// between turns (agent-impl.md §4.3); harness runs cancel through the run row instead,
// because there is no job (§5.2).
func (s *AgentService) CancelRunJob(ctx context.Context, userID, runID string) error {
	run, err := s.repo.GetRun(ctx, userID, runID)
	if err != nil {
		return err
	}
	if run.JobID == "" {
		return s.repo.UpdateRun(ctx, userID, runID, map[string]any{"cancel_requested": true})
	}
	return s.store.DB.WithContext(ctx).Model(&persistence.AgentJob{}).
		Where("id = ? AND user_id = ?", run.JobID, userID).
		Updates(map[string]any{"cancel_requested": true, "updated_at": persistence.Now()}).Error
}

// runWallClock is the run's wall-clock budget (§6), defaulted when a service was built
// without limits.
func (s *harnessLifecycle) runWallClock() time.Duration {
	if s.svc.limits.WallClock > 0 {
		return s.svc.limits.WallClock
	}
	return DefaultLoopLimits().WallClock
}
