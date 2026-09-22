package application

import (
	"context"
	"encoding/json"
	"time"

	"github.com/FastR-D/FastTask/internal/persistence"
	"gorm.io/gorm"
)

// JobService owns the agent-job lifecycle: enqueue, retry, cancel, the external
// Worker callback, and folding a succeeded job's output into business tables
// (wiring.md §4, arch.md §7.6).
//
// Materialization is dispatched through the "job_materializers" value group
// (wiring.md §4.1, §5) rather than a central switch: each job type's materializer
// is registered by its domain. When built without an injected group (unit tests
// construct services directly per §2 rule 4), it falls back to BuiltinJobMaterializers.
type JobService struct {
	Store         *persistence.Store
	materializers map[string]JobMaterializer
}

// NewJobService builds the job service. A nil/empty materializer set selects the
// builtins; bootstrap passes the fx "job_materializers" value group.
func NewJobService(store *persistence.Store, materializers []JobMaterializer) *JobService {
	if len(materializers) == 0 {
		materializers = BuiltinJobMaterializers()
	}
	return &JobService{Store: store, materializers: materializerMap(materializers)}
}

func (a *JobService) CreateJob(ctx context.Context, userID, jobType, subjectType, subjectID string, baseRevision int, input any) (*persistence.AgentJob, error) {
	encoded, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	now := persistence.Now()
	job := persistence.AgentJob{ID: persistence.NewID("job"), UserID: userID, Type: jobType, Status: "queued", SubjectType: subjectType, SubjectID: subjectID, BaseRevision: baseRevision, InputJSON: string(encoded), MaxAttempts: 3, RunAfter: now, Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := a.Store.DB.WithContext(ctx).Create(&job).Error; err != nil {
		return nil, err
	}
	return &job, nil
}

func (a *JobService) RetryJob(ctx context.Context, userID, id string, expected int) (*persistence.AgentJob, error) {
	var old persistence.AgentJob
	if err := a.Store.DB.WithContext(ctx).Where("id = ? AND user_id = ?", id, userID).First(&old).Error; err != nil {
		return nil, notFound(err)
	}
	if old.Revision != expected {
		return nil, ErrRevision
	}
	if old.Status != "failed" {
		return nil, ErrConflict
	}
	now := persistence.Now()
	job := old
	retryOf := old.ID
	job.ID, job.Status, job.RetryOfJobID, job.AttemptCount, job.LeaseVersion, job.Revision = persistence.NewID("job"), "queued", &retryOf, 0, 0, 1
	job.ErrorCode, job.ErrorMessage, job.OutputJSON, job.LockedBy, job.RunTokenHash = "", "", "", "", ""
	job.LockedUntil, job.StartedAt, job.FinishedAt = nil, nil, nil
	job.RunAfter, job.CreatedAt, job.UpdatedAt = now, now, now
	if err := a.Store.DB.WithContext(ctx).Create(&job).Error; err != nil {
		return nil, err
	}
	return &job, nil
}

func (a *JobService) CancelJob(ctx context.Context, userID, id string, expected int) (*persistence.AgentJob, error) {
	var job persistence.AgentJob
	if err := a.Store.DB.WithContext(ctx).Where("id = ? AND user_id = ?", id, userID).First(&job).Error; err != nil {
		return nil, notFound(err)
	}
	if job.Revision != expected {
		return nil, ErrRevision
	}
	if job.Status != "queued" && job.Status != "running" {
		return nil, ErrConflict
	}
	status := job.Status
	updates := map[string]any{"cancel_requested": true, "revision": expected + 1, "updated_at": persistence.Now()}
	if job.Status == "queued" {
		status = "cancelled"
		updates["status"] = status
		updates["finished_at"] = persistence.Now()
	}
	if err := a.Store.DB.WithContext(ctx).Model(&job).Updates(updates).Error; err != nil {
		return nil, err
	}
	if err := a.Store.DB.WithContext(ctx).First(&job, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &job, nil
}

func (a *JobService) AgentCallback(ctx context.Context, userID, id string, attempt, lease int, runToken, status string, output any, errorCode, errorMessage string) (*persistence.AgentJob, error) {
	var job persistence.AgentJob
	err := a.Store.Transaction(ctx, func(tx *gorm.DB) error {
		if err := tx.Where("id = ? AND user_id = ?", id, userID).First(&job).Error; err != nil {
			return notFound(err)
		}
		if job.Status != "running" || job.AttemptCount != attempt || job.LeaseVersion != lease || job.RunTokenHash != persistence.Hash(runToken) {
			return ErrStaleAgentAttempt
		}
		if job.CancelRequested {
			status = "cancelled"
		}
		if status != "succeeded" && status != "failed" && status != "cancelled" {
			return ErrValidation
		}
		encoded, _ := json.Marshal(output)
		now := persistence.Now()
		if status == "succeeded" {
			if err := a.MaterializeJobResult(tx, job, output, now); err != nil {
				return err
			}
		}
		updates := map[string]any{"status": status, "output_json": string(encoded), "error_code": errorCode, "error_message": errorMessage, "finished_at": now, "locked_by": "", "locked_until": nil, "run_token_hash": "", "revision": job.Revision + 1, "updated_at": now}
		result := tx.Model(&persistence.AgentJob{}).Where("id = ? AND user_id = ? AND status = 'running' AND attempt_count = ? AND lease_version = ? AND run_token_hash = ?", id, userID, attempt, lease, persistence.Hash(runToken)).Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrStaleAgentAttempt
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := a.Store.DB.WithContext(ctx).Where("id = ? AND user_id = ?", id, userID).First(&job).Error; err != nil {
		return nil, err
	}
	return &job, nil
}

// MaterializeJobResult folds a succeeded job's output into business tables inside
// the caller's transaction, dispatching to the materializer registered for the job
// type. Types with no materializer (agent_run, daily_plan_generation,
// voice_transcription) are no-ops, matching the historical switch's default.
func (a *JobService) MaterializeJobResult(tx *gorm.DB, job persistence.AgentJob, output any, now time.Time) error {
	if materializer, ok := a.materializers[job.Type]; ok {
		return materializer.MaterializeJob(tx, job, output, now)
	}
	return nil
}
