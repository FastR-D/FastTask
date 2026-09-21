package persistence

import (
	"context"
	"errors"
	"testing"
)

func agentTestUser(t *testing.T, store *Store, identifier string) User {
	t.Helper()
	now := Now()
	user := User{ID: NewID("user"), Identifier: identifier, PasswordHash: "hash", DisplayName: identifier, Timezone: "Asia/Shanghai", Locale: "zh-CN", Role: "member", Status: "active", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := store.DB.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	return user
}

// TestAgentRepositoryScopesByUser asserts the cross-user 404 invariant: a record
// owned by another user is indistinguishable from a missing one on every read
// path (arch.md §12, agent-impl.md §10 "跨用户 threadId / toolCallId 返回 404").
func TestAgentRepositoryScopesByUser(t *testing.T) {
	store := newTestStore(t)
	defer store.Close()
	ctx := context.Background()
	repo := NewAgentRepository(store)
	owner := agentTestUser(t, store, "owner")
	other := agentTestUser(t, store, "other")

	thread, err := repo.CreateThread(ctx, owner.ID, nil, "owner thread")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.GetThread(ctx, other.ID, thread.ID); !IsNotFound(err) {
		t.Fatalf("cross-user GetThread err=%v, want not-found", err)
	}

	run := &AgentRun{UserID: owner.ID, ThreadID: thread.ID, Status: RunRunning}
	if err := repo.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.GetRun(ctx, other.ID, run.ID); !IsNotFound(err) {
		t.Fatalf("cross-user GetRun err=%v, want not-found", err)
	}
	if _, err := repo.GetActiveRunByThread(ctx, other.ID, thread.ID); !IsNotFound(err) {
		t.Fatalf("cross-user GetActiveRunByThread err=%v, want not-found", err)
	}

	msg := &AgentMessage{UserID: owner.ID, ThreadID: thread.ID, RunID: run.ID, Role: "assistant"}
	if err := repo.CreateMessage(ctx, msg); err != nil {
		t.Fatal(err)
	}
	toolCallID := "call_1"
	part := &AgentMessagePart{UserID: owner.ID, MessageID: msg.ID, Type: "tool-call", ToolCallID: &toolCallID, ToolName: "list_goals"}
	if err := repo.CreatePart(ctx, part); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.GetPartByToolCallID(ctx, other.ID, toolCallID); !IsNotFound(err) {
		t.Fatalf("cross-user GetPartByToolCallID err=%v, want not-found", err)
	}
	if _, err := repo.GetPartByToolCallID(ctx, owner.ID, toolCallID); err != nil {
		t.Fatalf("owner GetPartByToolCallID err=%v", err)
	}

	// UpdateRun scoped to the wrong user must not touch the row.
	if err := repo.UpdateRun(ctx, other.ID, run.ID, map[string]any{"status": RunSucceeded}); !IsNotFound(err) {
		t.Fatalf("cross-user UpdateRun err=%v, want not-found", err)
	}
	fresh, err := repo.GetRun(ctx, owner.ID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Status != RunRunning {
		t.Fatalf("cross-user update mutated run status=%q", fresh.Status)
	}
}

// TestAgentRepositoryOneActiveRunPerThread asserts the database rejects a second
// queued/running/awaiting_approval run on the same thread (agent-impl.md §3.1),
// while allowing a new run once the prior one reaches a terminal state.
func TestAgentRepositoryOneActiveRunPerThread(t *testing.T) {
	store := newTestStore(t)
	defer store.Close()
	ctx := context.Background()
	repo := NewAgentRepository(store)
	user := agentTestUser(t, store, "solo")
	thread, err := repo.CreateThread(ctx, user.ID, nil, "")
	if err != nil {
		t.Fatal(err)
	}

	first := &AgentRun{UserID: user.ID, ThreadID: thread.ID, Status: RunRunning}
	if err := repo.CreateRun(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := &AgentRun{UserID: user.ID, ThreadID: thread.ID, Status: RunQueued}
	if err := repo.CreateRun(ctx, second); !IsUniqueViolation(err) {
		t.Fatalf("second active run err=%v, want unique violation", err)
	}

	// A different thread is independent.
	otherThread, err := repo.CreateThread(ctx, user.ID, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateRun(ctx, &AgentRun{UserID: user.ID, ThreadID: otherThread.ID, Status: RunRunning}); err != nil {
		t.Fatalf("run on a different thread rejected: %v", err)
	}

	// Terminal state frees the thread for a new run.
	if err := repo.SetRunStatus(ctx, user.ID, first.ID, RunSucceeded, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.GetActiveRunByThread(ctx, user.ID, thread.ID); !IsNotFound(err) {
		t.Fatalf("finished run still reported active: %v", err)
	}
	if err := repo.CreateRun(ctx, &AgentRun{UserID: user.ID, ThreadID: thread.ID, Status: RunQueued}); err != nil {
		t.Fatalf("new run after terminal state rejected: %v", err)
	}
}

// TestAgentRepositoryAwaitingApprovalBlocksNewRun asserts awaiting_approval
// counts as in-flight: the run holds no lease but the thread cannot start a
// second concurrent run (agent-impl.md §4.0).
func TestAgentRepositoryAwaitingApprovalBlocksNewRun(t *testing.T) {
	store := newTestStore(t)
	defer store.Close()
	ctx := context.Background()
	repo := NewAgentRepository(store)
	user := agentTestUser(t, store, "approver")
	thread, err := repo.CreateThread(ctx, user.ID, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	run := &AgentRun{UserID: user.ID, ThreadID: thread.ID, Status: RunRunning}
	if err := repo.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetRunStatus(ctx, user.ID, run.ID, RunAwaitingApproval, "", ""); err != nil {
		t.Fatal(err)
	}
	active, err := repo.GetActiveRunByThread(ctx, user.ID, thread.ID)
	if err != nil {
		t.Fatalf("awaiting_approval run not reported active: %v", err)
	}
	if active.ID != run.ID {
		t.Fatalf("active run=%q, want %q", active.ID, run.ID)
	}
	// The conditional write rejects a new run even though awaiting_approval holds
	// no lease and is not covered by the partial unique index (agent-impl.md §3.1).
	if err := repo.CreateRunExclusive(ctx, &AgentRun{UserID: user.ID, ThreadID: thread.ID, Status: RunQueued}); !errors.Is(err, ErrActiveRunExists) {
		t.Fatalf("new run during awaiting_approval err=%v, want ErrActiveRunExists", err)
	}
}

// TestAgentRepositoryChunkLogReplay asserts chunk sequence assignment is
// monotonic per run, the (run_id, seq) primary key rejects duplicates, and
// ListChunksFrom replays a suffix in order for resume (agent-impl.md §2.8, §8).
func TestAgentRepositoryChunkLogReplay(t *testing.T) {
	store := newTestStore(t)
	defer store.Close()
	ctx := context.Background()
	repo := NewAgentRepository(store)
	user := agentTestUser(t, store, "streamer")
	thread, err := repo.CreateThread(ctx, user.ID, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	run := &AgentRun{UserID: user.ID, ThreadID: thread.ID, Status: RunRunning}
	if err := repo.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 5; i++ {
		seq, err := repo.AppendChunk(ctx, user.ID, run.ID, `{"type":"step-start"}`)
		if err != nil {
			t.Fatal(err)
		}
		if seq != i {
			t.Fatalf("chunk seq=%d, want %d", seq, i)
		}
	}
	count, err := repo.CountChunks(ctx, user.ID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if count != 5 {
		t.Fatalf("chunk count=%d, want 5", count)
	}
	// Replay from seq 3 returns the last two chunks in order.
	suffix, err := repo.ListChunksFrom(ctx, user.ID, run.ID, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(suffix) != 2 || suffix[0].Seq != 3 || suffix[1].Seq != 4 {
		t.Fatalf("suffix replay=%+v, want seqs [3 4]", suffix)
	}
	// Cross-user replay is empty, not an error, and never leaks another user's log.
	if leaked, err := repo.ListChunksFrom(ctx, agentTestUser(t, store, "peeker").ID, run.ID, 0); err != nil || len(leaked) != 0 {
		t.Fatalf("cross-user chunk replay=%+v err=%v, want empty", leaked, err)
	}
}

// TestAgentRepositoryMessageSeqAndTransaction asserts message seq is
// thread-scoped and monotonic, and that a run with its message, parts and chunks
// can be written atomically through Transaction (agent-impl.md §2.7.1, §8).
func TestAgentRepositoryMessageSeqAndTransaction(t *testing.T) {
	store := newTestStore(t)
	defer store.Close()
	ctx := context.Background()
	repo := NewAgentRepository(store)
	user := agentTestUser(t, store, "atomic")
	thread, err := repo.CreateThread(ctx, user.ID, nil, "")
	if err != nil {
		t.Fatal(err)
	}

	// Two messages created independently get seq 1 then 2. Both belong to a real
	// run: agent_messages.run_id is a genuine FK (agent-impl.md §8 creates the
	// user message and the run in one transaction).
	bootstrapRun := &AgentRun{UserID: user.ID, ThreadID: thread.ID, Status: RunSucceeded}
	if err := repo.CreateRun(ctx, bootstrapRun); err != nil {
		t.Fatal(err)
	}
	first := &AgentMessage{UserID: user.ID, ThreadID: thread.ID, RunID: bootstrapRun.ID, Role: "user"}
	if err := repo.CreateMessage(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := &AgentMessage{UserID: user.ID, ThreadID: thread.ID, RunID: bootstrapRun.ID, Role: "assistant"}
	if err := repo.CreateMessage(ctx, second); err != nil {
		t.Fatal(err)
	}
	if first.Seq != 1 || second.Seq != 2 {
		t.Fatalf("message seqs=%d,%d want 1,2", first.Seq, second.Seq)
	}

	// A transactional unit of work: run + message + part + chunk all commit or all
	// roll back together.
	toolCallID := "call_atomic"
	err = repo.Transaction(ctx, func(tx *AgentRepository) error {
		run := &AgentRun{UserID: user.ID, ThreadID: thread.ID, Status: RunRunning}
		if err := tx.CreateRun(ctx, run); err != nil {
			return err
		}
		msg := &AgentMessage{UserID: user.ID, ThreadID: thread.ID, RunID: run.ID, Role: "assistant"}
		if err := tx.CreateMessage(ctx, msg); err != nil {
			return err
		}
		if err := tx.CreatePart(ctx, &AgentMessagePart{UserID: user.ID, MessageID: msg.ID, Type: "tool-call", ToolCallID: &toolCallID, ToolName: "get_task_tree"}); err != nil {
			return err
		}
		if _, err := tx.AppendChunk(ctx, user.ID, run.ID, `{"type":"message-finish","finishReason":"stop"}`); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("transactional write: %v", err)
	}
	part, err := repo.GetPartByToolCallID(ctx, user.ID, toolCallID)
	if err != nil {
		t.Fatal(err)
	}
	if part.ToolName != "get_task_tree" {
		t.Fatalf("part tool=%q", part.ToolName)
	}

	// A tool_call_id is globally unique: reusing it fails.
	dup := &AgentMessagePart{UserID: user.ID, MessageID: second.ID, Type: "tool-call", ToolCallID: &toolCallID, ToolName: "list_goals"}
	if err := repo.CreatePart(ctx, dup); !IsUniqueViolation(err) {
		t.Fatalf("duplicate tool_call_id err=%v, want unique violation", err)
	}
}

// TestAgentRepositoryMarkInterruptedRuns asserts startup recovery moves runs left
// in flight by a crashed process to interrupted while preserving their partial
// messages, and leaves terminal runs untouched (agent-impl.md §4.2).
func TestAgentRepositoryMarkInterruptedRuns(t *testing.T) {
	store := newTestStore(t)
	defer store.Close()
	ctx := context.Background()
	repo := NewAgentRepository(store)
	user := agentTestUser(t, store, "crash")

	mkThread := func() string {
		th, err := repo.CreateThread(ctx, user.ID, nil, "")
		if err != nil {
			t.Fatal(err)
		}
		return th.ID
	}
	running := &AgentRun{UserID: user.ID, ThreadID: mkThread(), Status: RunRunning}
	queued := &AgentRun{UserID: user.ID, ThreadID: mkThread(), Status: RunQueued}
	awaiting := &AgentRun{UserID: user.ID, ThreadID: mkThread(), Status: RunAwaitingApproval}
	done := &AgentRun{UserID: user.ID, ThreadID: mkThread(), Status: RunSucceeded}
	for _, run := range []*AgentRun{running, queued, awaiting, done} {
		if err := repo.CreateRun(ctx, run); err != nil {
			t.Fatal(err)
		}
	}
	// A partial assistant message under the running run must survive.
	partial := &AgentMessage{UserID: user.ID, ThreadID: running.ThreadID, RunID: running.ID, Role: "assistant"}
	if err := repo.CreateMessage(ctx, partial); err != nil {
		t.Fatal(err)
	}

	affected, err := repo.MarkInterruptedRuns(ctx, "INTERRUPTED")
	if err != nil {
		t.Fatal(err)
	}
	if affected != 2 {
		t.Fatalf("interrupted=%d, want 2 (running+queued)", affected)
	}
	for id, want := range map[string]string{running.ID: RunInterrupted, queued.ID: RunInterrupted, awaiting.ID: RunAwaitingApproval, done.ID: RunSucceeded} {
		got, err := repo.GetRun(ctx, user.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != want {
			t.Fatalf("run %s status=%q, want %q", id, got.Status, want)
		}
	}
	if _, err := repo.GetMessage(ctx, user.ID, partial.ID); err != nil {
		t.Fatalf("partial message lost on interrupt: %v", err)
	}
}

// TestAgentRepositoryRunStateAndJobLink asserts the resume snapshot and job link
// round-trip, and that GetRunByJobID finds the run carrying a job.
func TestAgentRepositoryRunStateAndJobLink(t *testing.T) {
	store := newTestStore(t)
	defer store.Close()
	ctx := context.Background()
	repo := NewAgentRepository(store)
	user := agentTestUser(t, store, "resumer")
	thread, err := repo.CreateThread(ctx, user.ID, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	jobID := NewID("job")
	run := &AgentRun{UserID: user.ID, ThreadID: thread.ID, JobID: jobID, Status: RunRunning}
	if err := repo.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	state := `{"messages":[],"isRunning":true,"fasttask":{"activeGoalId":"goal_1"}}`
	if err := repo.SaveRunState(ctx, user.ID, run.ID, state, 7); err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetRunByJobID(ctx, user.ID, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if got.StateJSON != state || got.CheckpointSeq != 7 {
		t.Fatalf("state=%q checkpoint=%d", got.StateJSON, got.CheckpointSeq)
	}
	if got.Revision <= run.Revision {
		t.Fatalf("revision did not advance: %d -> %d", run.Revision, got.Revision)
	}
}
