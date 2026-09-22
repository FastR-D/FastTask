package application

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/FastR-D/FastTask/internal/agent/protocol"
	"github.com/FastR-D/FastTask/internal/persistence"
)

// Run lifecycle under a harness host (doc/harness.md §5, §6, §11). The host drives the
// loop; these methods are what the server does about it — accept a cancellation from
// the user, accept a completion from the host, keep the checkpoint that makes a
// conversation portable, and finish the runs whose host disappeared.

// --- cancellation (§5.2) ---

// RequestCancellation is the user-facing half of a cancellation. It authenticates with
// the user's own JWT, never with a harness token: cancelling is the user's power, not
// the host's (§10.2). The other two channels — the heartbeat response and the open
// streams — are read from the flag this sets.
func (s *harnessLifecycle) RequestCancellation(ctx context.Context, userID, runID string) error {
	run, err := s.repo().GetRun(ctx, userID, runID)
	if err != nil {
		return err
	}
	if IsTerminalRunStatus(run.Status) {
		return nil
	}
	if run.HarnessMode == "" {
		// The in-process path cancels through its job (§4.3 of agent-impl.md).
		return s.svc.CancelRunJob(ctx, userID, runID)
	}
	changed, err := s.repo().RequestRunCancellation(ctx, userID, runID)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	// A run with no host to tell (nothing ever asked for a token) cannot confirm the
	// cancellation, so finish it here instead of waiting for the reaper.
	if _, err := s.repo().ActiveHarnessTokenForRun(ctx, runID); err != nil {
		if persistence.IsNotFound(err) {
			return s.finishRun(ctx, run, persistence.RunCancelled, "CANCELLED", "cancelled by user", protocol.ReasonCancelled)
		}
		return err
	}
	return nil
}

// --- completion (§5.1, §6.1) ---

// Completion is what a host reports when its turn ends. The checkpoint write and the
// run's terminal transition are the same event: one user message is one run is one
// prompt() is one turn (§5.1), so the checkpoint lands exactly once, at the end.
type Completion struct {
	StopReason   string
	ErrorMessage string
	Usage        map[string]any
	Checkpoint   []byte
	LibfxVersion string
}

// CompleteRun stores the thread checkpoint and moves the run to its terminal state.
// The status follows the stop reason, except that a run the user cancelled ends
// cancelled whatever the host says (§5.2).
func (s *harnessLifecycle) CompleteRun(ctx context.Context, p HarnessPrincipal, in Completion) error {
	run, err := s.repo().GetRun(ctx, p.UserID, p.RunID)
	if err != nil {
		return err
	}
	if IsTerminalRunStatus(run.Status) {
		// A late report after the reaper got there first. The checkpoint is still worth
		// keeping: it is thread history, not run state.
		return s.saveCheckpoint(ctx, p, in)
	}
	if err := s.saveCheckpoint(ctx, p, in); err != nil {
		return err
	}
	switch {
	case run.CancelRequested || strings.EqualFold(in.StopReason, "cancelled"):
		return s.finishRun(ctx, run, persistence.RunCancelled, "CANCELLED", "cancelled by user", protocol.ReasonCancelled)
	case strings.EqualFold(in.StopReason, "error"):
		code := "PROVIDER_ERROR"
		message := in.ErrorMessage
		if message == "" {
			message = "the harness reported a failed turn"
		}
		return s.finishRun(ctx, run, persistence.RunFailed, code, message, protocol.ReasonError)
	default:
		return s.finishRun(ctx, run, persistence.RunSucceeded, "", "", protocol.ReasonStop)
	}
}

// saveCheckpoint writes the thread's libfx checkpoint (§6.1). It is stored server-side
// so a conversation survives a device change, which a browser-held copy would not.
func (s *harnessLifecycle) saveCheckpoint(ctx context.Context, p HarnessPrincipal, in Completion) error {
	if len(in.Checkpoint) == 0 {
		return nil
	}
	version := in.LibfxVersion
	if version == "" {
		version = LibfxVersion
	}
	return s.repo().SaveThreadCheckpoint(ctx, p.UserID, p.ThreadID, in.Checkpoint, version)
}

// ThreadCheckpoint is the read half of §6.1: what a host restores a thread from.
type ThreadCheckpoint struct {
	Checkpoint   []byte `json:"checkpoint"`
	LibfxVersion string `json:"libfx_version"`
	// Skew reports that the stored checkpoint was written by another libfx version, so
	// the host must not pass it to createFxAgent and the server has already rebuilt a
	// summary into the instructions (§6.3).
	Skew bool `json:"skew"`
}

// GetThreadCheckpoint returns a thread's checkpoint to its own host.
func (s *harnessLifecycle) GetThreadCheckpoint(ctx context.Context, p HarnessPrincipal) (ThreadCheckpoint, error) {
	thread, err := s.repo().GetThread(ctx, p.UserID, p.ThreadID)
	if err != nil {
		return ThreadCheckpoint{}, err
	}
	if len(thread.Checkpoint) == 0 {
		return ThreadCheckpoint{LibfxVersion: LibfxVersion}, nil
	}
	skew := thread.LibfxVersion != LibfxVersion
	if skew {
		// A version the running host cannot read is not handed out: restoring it would
		// throw inside createFxAgent, and §6.3 wants the degraded path instead.
		return ThreadCheckpoint{LibfxVersion: thread.LibfxVersion, Skew: true}, nil
	}
	return ThreadCheckpoint{Checkpoint: thread.Checkpoint, LibfxVersion: thread.LibfxVersion}, nil
}

// PutThreadCheckpoint stores a checkpoint mid-thread (§6.1). A host that wants its run
// finished reports through CompleteRun instead.
func (s *harnessLifecycle) PutThreadCheckpoint(ctx context.Context, p HarnessPrincipal, checkpoint []byte, libfxVersion string) error {
	version := strings.TrimSpace(libfxVersion)
	if version == "" {
		version = LibfxVersion
	}
	return s.repo().SaveThreadCheckpoint(ctx, p.UserID, p.ThreadID, checkpoint, version)
}

// --- run teardown ---

// finishRun drives a run to a terminal state, emits the protocol chunks that let the
// UI stop showing it as running, and revokes the tokens that could still drive it.
//
// A run without a session — one that never got as far as an assistant message — still
// has to reach its terminal status, so the status write is the fallback rather than an
// error path.
func (s *harnessLifecycle) finishRun(ctx context.Context, run *persistence.AgentRun, status, code, message, _ string) error {
	sess, err := s.loadRunSession(ctx, run)
	switch {
	case err != nil:
		return err
	case sess == nil:
		if err := s.repo().SetRunStatus(ctx, run.UserID, run.ID, status, code, message); err != nil {
			return err
		}
	case status == persistence.RunSucceeded:
		if _, err := sess.succeed(); err != nil {
			return err
		}
	case status == persistence.RunCancelled:
		if _, err := sess.cancelRun(); err != nil {
			return err
		}
	case status == persistence.RunInterrupted:
		// Interrupted is not failed: the transcript so far stays readable and a pending proposal
		// stays pending, so the user can pick it up later (§11).
		if _, err := sess.interruptRun(code, message); err != nil {
			return err
		}
	default:
		_, _ = sess.failRun(code, errors.New(message))
	}
	_, _ = s.repo().RevokeHarnessTokensForRun(ctx, run.ID)
	return nil
}

// ReapHarnessRuns is the periodic half of run recovery (§11). It interrupts the runs
// whose host stopped beating or never arrived, and finishes cancellations the host did
// not confirm. The scheduler calls it; no goroutine is started here.
func (s *harnessLifecycle) ReapHarnessRuns(ctx context.Context) (int, error) {
	now := s.clock()
	stale, err := s.repo().ListStaleHarnessRuns(ctx, now.Add(-HarnessHeartbeatLoss), now.Add(-harnessPickupGrace))
	if err != nil {
		return 0, err
	}
	reaped := 0
	seen := map[string]bool{}
	for _, item := range stale {
		if seen[item.RunID] {
			continue
		}
		seen[item.RunID] = true
		run, err := s.repo().GetRun(ctx, item.UserID, item.RunID)
		if err != nil {
			continue
		}
		if IsTerminalRunStatus(run.Status) {
			continue
		}
		message := "harness host stopped responding"
		if item.Reason == "HARNESS_NEVER_STARTED" {
			message = "no harness host picked up the run"
		}
		// interrupted, not failed: the transcript so far stays readable and any pending
		// proposal stays pending for the user to decide later (§11).
		if err := s.finishRun(ctx, run, persistence.RunInterrupted, "INTERRUPTED", message, protocol.ReasonError); err != nil {
			continue
		}
		reaped++
	}

	overdue, err := s.repo().ListCancellingRunsSince(ctx, now.Add(-HarnessCancelGrace))
	if err != nil {
		return reaped, err
	}
	for i := range overdue {
		run := overdue[i]
		if err := s.finishRun(ctx, &run, persistence.RunCancelled, "CANCELLED", "cancelled by user", protocol.ReasonCancelled); err != nil {
			continue
		}
		reaped++
	}
	return reaped, nil
}

// PruneHarnessTokens removes tokens that can no longer be used. It runs on the same
// periodic sweep so the table does not grow without bound.
func (s *harnessLifecycle) PruneHarnessTokens(ctx context.Context) (int64, error) {
	return s.repo().DeleteExpiredHarnessTokens(ctx, s.clock().Add(-harnessTokenTTL))
}

// --- helpers shared with the proxy and the tool surface ---

// activeRun loads a run for a harness principal and refuses one that is no longer
// drivable. Every harness endpoint goes through it, so a terminal run cannot be
// written to by a host that has not noticed yet.
func (s *harnessLifecycle) activeRun(ctx context.Context, p HarnessPrincipal) (*persistence.AgentRun, error) {
	run, err := s.repo().GetRun(ctx, p.UserID, p.RunID)
	if err != nil {
		return nil, err
	}
	if run.ThreadID != p.ThreadID {
		return nil, ErrHarnessToken
	}
	if IsTerminalRunStatus(run.Status) {
		return nil, ErrRunNotActive
	}
	return run, nil
}

// wallClockExceeded reports whether a run is past its wall clock once the approval
// wait is subtracted (§7: waiting for a human is not the model's budget).
func (s *harnessLifecycle) wallClockExceeded(run *persistence.AgentRun, now time.Time) bool {
	limit := s.runWallClock()
	elapsed := now.Sub(run.CreatedAt) - time.Duration(run.ApprovalWaitMs)*time.Millisecond
	return elapsed > limit
}
