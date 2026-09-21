package persistence

import (
	"context"
	"errors"
	"strings"

	"gorm.io/gorm"
)

// ErrActiveRunExists is returned by the conditional-write guard when a thread
// already has an in-flight run (queued, running, or awaiting_approval). It maps
// to HTTP 409 and enforces "一个用户同时最多一个活跃运行" (agent-impl.md §3.1,
// §9.3) and "审批续跑的是同一个 Run，不是新 Run" (§7.2).
var ErrActiveRunExists = errors.New("an active agent run already exists for this thread")

// AgentRepository encapsulates every query against the agent runtime tables
// (agent_runs, agent_messages, agent_message_parts, agent_run_chunks) plus the
// conversations table reused as Thread (doc/agent-impl.md §3).
//
// Two invariants are enforced here rather than left to callers:
//
//  1. Every query is scoped by user_id (arch.md §12). A record owned by another
//     user is indistinguishable from a missing one: reads return
//     gorm.ErrRecordNotFound, which callers map to 404.
//  2. *gorm.DB never escapes this type. Callers compose atomic units of work
//     through Transaction, receiving a transaction-bound repository.
//
// The zero-tx repository reads and writes through Store.DB; WithTx returns a
// copy bound to an ambient transaction so the agent loop can write a run, its
// messages, parts and chunk log atomically.
type AgentRepository struct {
	store *Store
	tx    *gorm.DB
}

// NewAgentRepository builds a repository backed by the given store.
func NewAgentRepository(store *Store) *AgentRepository {
	return &AgentRepository{store: store}
}

// WithTx returns a repository bound to tx. It shares the store but routes every
// query through the transaction.
func (r *AgentRepository) WithTx(tx *gorm.DB) *AgentRepository {
	return &AgentRepository{store: r.store, tx: tx}
}

// Transaction runs fn against a transaction-bound repository, retrying on
// SQLite busy exactly like Store.Transaction.
func (r *AgentRepository) Transaction(ctx context.Context, fn func(repo *AgentRepository) error) error {
	return r.store.Transaction(ctx, func(tx *gorm.DB) error {
		return fn(r.WithTx(tx))
	})
}

// db resolves the active query builder: the ambient transaction when present,
// otherwise the store handle.
func (r *AgentRepository) db(ctx context.Context) *gorm.DB {
	if r.tx != nil {
		return r.tx.WithContext(ctx)
	}
	return r.store.DB.WithContext(ctx)
}

// Run statuses (doc/agent-impl.md §4). AwaitingApproval is the only status where
// the run yields control without terminating: the HTTP stream ends but the run
// is neither finished nor holding a lease.
const (
	RunQueued           = "queued"
	RunRunning          = "running"
	RunAwaitingApproval = "awaiting_approval"
	RunSucceeded        = "succeeded"
	RunFailed           = "failed"
	RunCancelled        = "cancelled"
	RunInterrupted      = "interrupted"
)

// activeRunStatuses are the statuses that count as "a run is in flight" for the
// one-active-run-per-thread rule. awaiting_approval is included: a thread with a
// pending approval must not start a second concurrent run.
var activeRunStatuses = []string{RunQueued, RunRunning, RunAwaitingApproval}

// --- Thread (reused conversations table) ---

// CreateThread inserts a conversations row that backs an agent thread. goalID
// may be nil (agent.md §11: goal binding is optional).
func (r *AgentRepository) CreateThread(ctx context.Context, userID string, goalID *string, title string) (*Conversation, error) {
	now := Now()
	if strings.TrimSpace(title) == "" {
		title = "Agent 会话"
	}
	thread := &Conversation{ID: NewID("conv"), UserID: userID, GoalID: goalID, Title: title, Status: "active", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := r.db(ctx).Create(thread).Error; err != nil {
		return nil, err
	}
	return thread, nil
}

// GetThread returns a thread owned by userID, or gorm.ErrRecordNotFound.
func (r *AgentRepository) GetThread(ctx context.Context, userID, threadID string) (*Conversation, error) {
	var thread Conversation
	if err := r.db(ctx).Where("id = ? AND user_id = ?", threadID, userID).First(&thread).Error; err != nil {
		return nil, err
	}
	return &thread, nil
}

// TouchThread bumps updated_at so thread listings sort by recent activity.
func (r *AgentRepository) TouchThread(ctx context.Context, userID, threadID string) error {
	return r.db(ctx).Model(&Conversation{}).
		Where("id = ? AND user_id = ?", threadID, userID).
		Updates(map[string]any{"updated_at": Now()}).Error
}

// --- Runs ---

// CreateRun inserts a run, defaulting status/timestamps. It relies on the
// partial unique index idx_agent_runs_active_thread to reject a second active
// run on the same thread; callers should translate that into a 409.
func (r *AgentRepository) CreateRun(ctx context.Context, run *AgentRun) error {
	now := Now()
	if run.ID == "" {
		run.ID = NewID("run")
	}
	if run.Status == "" {
		run.Status = RunQueued
	}
	if run.StateJSON == "" {
		run.StateJSON = "{}"
	}
	if run.Revision == 0 {
		run.Revision = 1
	}
	run.CreatedAt, run.UpdatedAt = now, now
	return r.db(ctx).Create(run).Error
}

// CreateRunExclusive is the conditional write that enforces "at most one active
// run per thread" (agent-impl.md §3.1). It checks for any in-flight run
// (queued/running/awaiting_approval) and creates the new run in the same
// transaction, so the awaiting_approval case — which the partial unique index
// does not cover because that run holds no lease — is still rejected. The DB
// unique index remains as defence-in-depth for the queued/running race.
//
// It returns ErrActiveRunExists when the thread is busy. It must be called on a
// non-transactional repository because it opens its own transaction.
func (r *AgentRepository) CreateRunExclusive(ctx context.Context, run *AgentRun) error {
	if r.tx != nil {
		return errors.New("CreateRunExclusive must not be called within an existing transaction")
	}
	return r.store.Transaction(ctx, func(tx *gorm.DB) error {
		txRepo := r.WithTx(tx)
		var count int64
		err := tx.WithContext(ctx).Model(&AgentRun{}).
			Where("thread_id = ? AND user_id = ? AND status IN ?", run.ThreadID, run.UserID, activeRunStatuses).
			Count(&count).Error
		if err != nil {
			return err
		}
		if count > 0 {
			return ErrActiveRunExists
		}
		if err := txRepo.CreateRun(ctx, run); err != nil {
			if IsUniqueViolation(err) {
				return ErrActiveRunExists
			}
			return err
		}
		return nil
	})
}

// GetRun returns a run owned by userID, or gorm.ErrRecordNotFound.
func (r *AgentRepository) GetRun(ctx context.Context, userID, runID string) (*AgentRun, error) {
	var run AgentRun
	if err := r.db(ctx).Where("id = ? AND user_id = ?", runID, userID).First(&run).Error; err != nil {
		return nil, err
	}
	return &run, nil
}

// GetActiveRunByThread returns the single queued/running/awaiting_approval run
// for a thread, or gorm.ErrRecordNotFound when there is none.
func (r *AgentRepository) GetActiveRunByThread(ctx context.Context, userID, threadID string) (*AgentRun, error) {
	var run AgentRun
	err := r.db(ctx).
		Where("thread_id = ? AND user_id = ? AND status IN ?", threadID, userID, activeRunStatuses).
		Order("created_at DESC").First(&run).Error
	if err != nil {
		return nil, err
	}
	return &run, nil
}

// GetRunByJobID finds the run currently carried by an AgentJob.
func (r *AgentRepository) GetRunByJobID(ctx context.Context, userID, jobID string) (*AgentRun, error) {
	var run AgentRun
	if err := r.db(ctx).Where("job_id = ? AND user_id = ?", jobID, userID).First(&run).Error; err != nil {
		return nil, err
	}
	return &run, nil
}

// UpdateRun applies fields to a run with an optimistic revision bump. It returns
// gorm.ErrRecordNotFound when the run does not belong to userID.
func (r *AgentRepository) UpdateRun(ctx context.Context, userID, runID string, fields map[string]any) error {
	now := Now()
	updates := make(map[string]any, len(fields)+2)
	for k, v := range fields {
		updates[k] = v
	}
	updates["updated_at"] = now
	updates["revision"] = gorm.Expr("revision + 1")
	res := r.db(ctx).Model(&AgentRun{}).Where("id = ? AND user_id = ?", runID, userID).Updates(updates)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// SetRunStatus transitions a run's status. A terminal status stamps finished_at.
func (r *AgentRepository) SetRunStatus(ctx context.Context, userID, runID, status, errorCode, errorMessage string) error {
	fields := map[string]any{"status": status, "error_code": errorCode, "error_message": errorMessage}
	switch status {
	case RunSucceeded, RunFailed, RunCancelled, RunInterrupted:
		now := Now()
		fields["finished_at"] = &now
	}
	return r.UpdateRun(ctx, userID, runID, fields)
}

// SaveRunState persists the retained resume snapshot and checkpoint sequence
// (doc/agent-impl.md §2.8, §3.1 state_json).
func (r *AgentRepository) SaveRunState(ctx context.Context, userID, runID, stateJSON string, checkpointSeq int) error {
	return r.UpdateRun(ctx, userID, runID, map[string]any{"state_json": stateJSON, "checkpoint_seq": checkpointSeq})
}

// --- Messages ---

// CreateMessage inserts a message. When Seq is zero it is auto-assigned as the
// next thread-scoped sequence number (the server is the sole index owner,
// doc/agent-impl.md §2.7.1). Call within a Transaction to keep assignment and
// insert atomic.
func (r *AgentRepository) CreateMessage(ctx context.Context, msg *AgentMessage) error {
	if msg.ID == "" {
		msg.ID = NewID("amsg")
	}
	if msg.Seq == 0 {
		next, err := r.nextMessageSeq(ctx, msg.UserID, msg.ThreadID)
		if err != nil {
			return err
		}
		msg.Seq = next
	}
	msg.CreatedAt = Now()
	return r.db(ctx).Create(msg).Error
}

func (r *AgentRepository) nextMessageSeq(ctx context.Context, userID, threadID string) (int, error) {
	var maxSeq *int
	err := r.db(ctx).Model(&AgentMessage{}).
		Where("thread_id = ? AND user_id = ?", threadID, userID).
		Select("MAX(seq)").Scan(&maxSeq).Error
	if err != nil {
		return 0, err
	}
	if maxSeq == nil {
		return 1, nil
	}
	return *maxSeq + 1, nil
}

// GetMessage returns a message owned by userID, or gorm.ErrRecordNotFound.
func (r *AgentRepository) GetMessage(ctx context.Context, userID, messageID string) (*AgentMessage, error) {
	var msg AgentMessage
	if err := r.db(ctx).Where("id = ? AND user_id = ?", messageID, userID).First(&msg).Error; err != nil {
		return nil, err
	}
	return &msg, nil
}

// ListThreadMessages returns a thread's messages in seq order.
func (r *AgentRepository) ListThreadMessages(ctx context.Context, userID, threadID string) ([]AgentMessage, error) {
	var messages []AgentMessage
	err := r.db(ctx).Where("thread_id = ? AND user_id = ?", threadID, userID).
		Order("seq").Find(&messages).Error
	return messages, err
}

// ListRunMessages returns the messages produced under a single run, in seq order.
func (r *AgentRepository) ListRunMessages(ctx context.Context, userID, runID string) ([]AgentMessage, error) {
	var messages []AgentMessage
	err := r.db(ctx).Where("run_id = ? AND user_id = ?", runID, userID).
		Order("seq").Find(&messages).Error
	return messages, err
}

// --- Message parts ---

// CreatePart inserts a message part. Idx is the part's position within its
// message; ToolCallID must be globally unique when non-nil (the partial unique
// index enforces this and is the add-tool-result locator).
func (r *AgentRepository) CreatePart(ctx context.Context, part *AgentMessagePart) error {
	now := Now()
	if part.ID == "" {
		part.ID = NewID("apart")
	}
	if part.ArgsJSON == "" {
		part.ArgsJSON = "{}"
	}
	part.CreatedAt, part.UpdatedAt = now, now
	return r.db(ctx).Create(part).Error
}

// ListMessageParts returns a message's parts in idx order.
func (r *AgentRepository) ListMessageParts(ctx context.Context, userID, messageID string) ([]AgentMessagePart, error) {
	var parts []AgentMessagePart
	err := r.db(ctx).Where("message_id = ? AND user_id = ?", messageID, userID).
		Order("idx").Find(&parts).Error
	return parts, err
}

// GetPartByToolCallID locates a tool-call part by its unique tool_call_id,
// scoped to userID. Used to route an add-tool-result receipt to the right run
// (doc/agent-impl.md §7.2, §7.4).
func (r *AgentRepository) GetPartByToolCallID(ctx context.Context, userID, toolCallID string) (*AgentMessagePart, error) {
	var part AgentMessagePart
	err := r.db(ctx).Where("tool_call_id = ? AND user_id = ?", toolCallID, userID).First(&part).Error
	if err != nil {
		return nil, err
	}
	return &part, nil
}

// UpdatePartResult records a tool result (or approval receipt) on a part.
func (r *AgentRepository) UpdatePartResult(ctx context.Context, userID, partID, resultJSON string, isError bool) error {
	res := r.db(ctx).Model(&AgentMessagePart{}).
		Where("id = ? AND user_id = ?", partID, userID).
		Updates(map[string]any{"result_json": resultJSON, "is_error": isError, "updated_at": Now()})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// UpdatePartApproval sets the approval status and, for proposals, links the
// Proposal record created by a proposal tool (doc/agent-impl.md §7).
func (r *AgentRepository) UpdatePartApproval(ctx context.Context, userID, partID, status string, proposalID *string) error {
	fields := map[string]any{"approval_status": status, "updated_at": Now()}
	if proposalID != nil {
		fields["proposal_id"] = *proposalID
	}
	res := r.db(ctx).Model(&AgentMessagePart{}).Where("id = ? AND user_id = ?", partID, userID).Updates(fields)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// --- Chunk log ---

// AppendChunk writes a protocol chunk to a run's persistent log, assigning the
// next sequence number atomically, and returns that sequence. The log is the
// replay source for resume (doc/agent-impl.md §2.8, §8).
func (r *AgentRepository) AppendChunk(ctx context.Context, userID, runID, chunkJSON string) (int, error) {
	seq, err := r.NextChunkSeq(ctx, userID, runID)
	if err != nil {
		return 0, err
	}
	chunk := AgentRunChunk{RunID: runID, Seq: seq, UserID: userID, ChunkJSON: chunkJSON, CreatedAt: Now()}
	if err := r.db(ctx).Create(&chunk).Error; err != nil {
		return 0, err
	}
	return seq, nil
}

// NextChunkSeq returns the next chunk sequence for a run (0-based log).
func (r *AgentRepository) NextChunkSeq(ctx context.Context, userID, runID string) (int, error) {
	var maxSeq *int
	err := r.db(ctx).Model(&AgentRunChunk{}).
		Where("run_id = ? AND user_id = ?", runID, userID).
		Select("MAX(seq)").Scan(&maxSeq).Error
	if err != nil {
		return 0, err
	}
	if maxSeq == nil {
		return 0, nil
	}
	return *maxSeq + 1, nil
}

// ListChunksFrom returns a run's chunks with seq >= afterSeq, ordered by seq,
// for replay on reconnect.
func (r *AgentRepository) ListChunksFrom(ctx context.Context, userID, runID string, afterSeq int) ([]AgentRunChunk, error) {
	var chunks []AgentRunChunk
	err := r.db(ctx).Where("run_id = ? AND user_id = ? AND seq >= ?", runID, userID, afterSeq).
		Order("seq").Find(&chunks).Error
	return chunks, err
}

// CountChunks returns how many chunks a run has emitted.
func (r *AgentRepository) CountChunks(ctx context.Context, userID, runID string) (int64, error) {
	var count int64
	err := r.db(ctx).Model(&AgentRunChunk{}).Where("run_id = ? AND user_id = ?", runID, userID).Count(&count).Error
	return count, err
}

// MarkInterruptedRuns transitions any queued/running runs to interrupted. It is
// called on startup: v1 does not resume across a process restart, so runs left
// in flight by a crashed process are terminal-failed while their partial
// messages are preserved (doc/agent-impl.md §4.2).
func (r *AgentRepository) MarkInterruptedRuns(ctx context.Context, errorCode string) (int64, error) {
	now := Now()
	res := r.db(ctx).Model(&AgentRun{}).
		Where("status IN ?", []string{RunQueued, RunRunning}).
		Updates(map[string]any{
			"status":        RunInterrupted,
			"error_code":    errorCode,
			"error_message": "run interrupted by process restart",
			"finished_at":   &now,
			"updated_at":    now,
			"revision":      gorm.Expr("revision + 1"),
		})
	return res.RowsAffected, res.Error
}

// IsUniqueViolation reports whether err is a SQLite UNIQUE constraint failure,
// letting callers turn the one-active-run and unique-tool-call rules into 409s.
func IsUniqueViolation(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "unique")
}
