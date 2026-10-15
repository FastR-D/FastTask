package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/FastR-D/FastTask/internal/agent"
	"github.com/FastR-D/FastTask/internal/persistence"
	"gorm.io/gorm"
)

type Worker struct {
	app              *App
	identity         string
	interval         time.Duration
	provider         agent.Provider
	transcriber      agent.Transcriber
	providerResolver func(ctx context.Context) (agent.Provider, agent.Transcriber, error)
	agentRunner      *AgentService
	handlers         map[string]JobHandler
	// dispatchNotifications drains the notification queue. It is optional: delivery is the
	// scheduler's job, and this is only the fallback for a role that runs without one
	// (doc/notification.md §5).
	dispatchNotifications func(ctx context.Context) error
	lastNotificationDrain time.Time
	// leaseRenewEvery overrides jobLeaseRenewEvery in tests.
	leaseRenewEvery time.Duration
}

// WithAgentRunner attaches the agent runtime so the worker can execute
// type="agent_run" jobs (doc/agent-impl.md §8). The run loop persists its own
// state, messages and chunk log, so MaterializeJobResult is a no-op for it.
func (w *Worker) WithAgentRunner(runner *AgentService) *Worker {
	w.agentRunner = runner
	return w
}

// WithJobHandlers replaces the job-type dispatch table with the fx
// "job_handlers" value group (wiring.md §5). Tests that construct a Worker
// directly keep the builtins installed by NewWorker (§2 rule 4).
func (w *Worker) WithJobHandlers(handlers []JobHandler) *Worker {
	if len(handlers) > 0 {
		w.handlers = handlerMap(handlers)
	}
	return w
}

// WithNotificationDispatcher attaches the fallback notification drain. The worker polls
// every 300 ms and a notification is not that urgent, so the drain runs at most once per
// notificationDrainInterval; the queue itself decides what is due.
func (w *Worker) WithNotificationDispatcher(dispatch func(ctx context.Context) error) *Worker {
	w.dispatchNotifications = dispatch
	return w
}

func (w *Worker) WithTranscriber(transcriber agent.Transcriber) *Worker {
	w.transcriber = transcriber
	return w
}

func (w *Worker) WithProviderResolver(resolver func(ctx context.Context) (agent.Provider, agent.Transcriber, error)) *Worker {
	w.providerResolver = resolver
	return w
}

func (w *Worker) ActiveProviderRuntime(ctx context.Context) (*ProviderRuntimeConfig, error) {
	return w.app.ActiveProviderRuntime(ctx)
}

func NewWorker(app *App, interval time.Duration, providers ...agent.Provider) *Worker {
	worker := &Worker{app: app, identity: persistence.NewID("worker"), interval: interval, handlers: handlerMap(BuiltinJobHandlers())}
	if len(providers) > 0 {
		worker.provider = providers[0]
	}
	return worker
}

func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = w.RunOnce(ctx)
			w.drainNotifications(ctx)
		}
	}
}

// notificationDrainInterval is how often the worker's fallback pass looks at the
// notification queue.
const notificationDrainInterval = 5 * time.Second

// drainNotifications is the rate-limited fallback pass. It is deliberately silent: a
// provider that is down must not stop the worker from executing agent jobs, and the
// messages stay queued for the next pass.
func (w *Worker) drainNotifications(ctx context.Context) {
	if w.dispatchNotifications == nil {
		return
	}
	now := time.Now()
	if !w.lastNotificationDrain.IsZero() && now.Sub(w.lastNotificationDrain) < notificationDrainInterval {
		return
	}
	w.lastNotificationDrain = now
	_ = w.dispatchNotifications(ctx)
}

func (w *Worker) RunOnce(ctx context.Context) error {
	job, token, err := w.claim(ctx)
	if err != nil || job == nil {
		return err
	}
	runCtx, release := w.holdLease(ctx, *job, token)
	output, runErr := w.execute(runCtx, *job)
	release()
	if err := w.finish(ctx, *job, token, output, runErr); err != nil {
		if errors.Is(err, ErrStaleAgentAttempt) {
			slog.Warn("agent job result discarded: lease was lost during execution", "job_id", job.ID, "type", job.Type, "attempt", job.AttemptCount)
		} else {
			slog.Error("agent job could not be finalised", "job_id", job.ID, "type", job.Type, "err", err)
		}
		return err
	}
	if runErr != nil {
		slog.Warn("agent job attempt failed", "job_id", job.ID, "type", job.Type, "attempt", job.AttemptCount, "max_attempts", job.MaxAttempts, "err", runErr)
	}
	return nil
}

// jobRetryDelay backs off exponentially from 2s, capped at a minute. A provider answering 429 or 5xx is
// the common failure, and retrying it one second later mostly earns the same answer.
func jobRetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	return min(2*time.Second<<min(attempt-1, 5), time.Minute)
}

// jobLease is how long a claim stays valid without renewal; jobLeaseRenewEvery is how often a running
// attempt renews it. The scheduler requeues a job whose lease has lapsed, so an attempt that runs longer
// than one lease — an agent loop's wall clock alone is three minutes — must keep extending it, or a
// second worker would start the same job while the first is still producing a result.
const (
	jobLease           = 2 * time.Minute
	jobLeaseRenewEvery = 30 * time.Second
)

// holdLease renews the claim while the attempt runs. If a renewal finds the claim gone (cancelled,
// reclaimed after a stall, or fenced by a newer attempt) the returned context is cancelled, so the
// attempt stops spending provider calls on a result that can no longer be recorded.
func (w *Worker) holdLease(ctx context.Context, job persistence.AgentJob, rawToken string) (context.Context, func()) {
	runCtx, cancel := context.WithCancel(ctx)
	every := w.leaseRenewEvery
	if every <= 0 {
		every = jobLeaseRenewEvery
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				result := w.app.Store.DB.WithContext(runCtx).Model(&persistence.AgentJob{}).
					Where("id = ? AND status = 'running' AND lease_version = ? AND run_token_hash = ?", job.ID, job.LeaseVersion, persistence.Hash(rawToken)).
					UpdateColumn("locked_until", persistence.Now().Add(jobLease))
				if result.Error != nil {
					if runCtx.Err() == nil {
						// A busy database is not a lost lease; the next tick tries again well inside it.
						slog.Warn("agent job lease renewal failed", "job_id", job.ID, "err", result.Error)
					}
					continue
				}
				if result.RowsAffected == 0 {
					slog.Warn("agent job lease lost; stopping the attempt", "job_id", job.ID, "type", job.Type)
					cancel()
					return
				}
			}
		}
	}()
	return runCtx, func() {
		cancel()
		<-done
	}
}

func (w *Worker) claim(ctx context.Context) (*persistence.AgentJob, string, error) {
	var claimed *persistence.AgentJob
	var rawToken string
	err := w.app.Store.Transaction(ctx, func(tx *gorm.DB) error {
		var job persistence.AgentJob
		now := persistence.Now()
		if err := tx.Where("status = 'queued' AND run_after <= ?", now).Order("created_at").First(&job).Error; err != nil {
			if persistence.IsNotFound(err) {
				return nil
			}
			return err
		}
		rawToken = persistence.NewID("run")
		until := now.Add(jobLease)
		result := tx.Model(&persistence.AgentJob{}).Where("id = ? AND status = 'queued'", job.ID).Updates(map[string]any{
			"status": "running", "locked_by": w.identity, "locked_until": until, "lease_version": job.LeaseVersion + 1,
			"run_token_hash": persistence.Hash(rawToken), "attempt_count": job.AttemptCount + 1, "started_at": now,
			"revision": job.Revision + 1, "updated_at": now,
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return nil
		}
		if err := tx.First(&job, "id = ?", job.ID).Error; err != nil {
			return err
		}
		claimed = &job
		return nil
	})
	return claimed, rawToken, err
}

func (w *Worker) execute(ctx context.Context, job persistence.AgentJob) (any, error) {
	if job.CancelRequested {
		return nil, errors.New("cancelled")
	}
	provider, transcriber := w.provider, w.transcriber
	if w.providerResolver != nil {
		resolvedProvider, resolvedTranscriber, err := w.providerResolver(ctx)
		if err != nil {
			return nil, err
		}
		if resolvedProvider != nil {
			provider = resolvedProvider
		}
		if resolvedTranscriber != nil {
			transcriber = resolvedTranscriber
		}
	}
	// Dispatch through the job_handlers registry (wiring.md §5) instead of a
	// central switch: each domain registers its handler, so a new job type never
	// edits this file. The resolved provider/transcriber and the agent runner are
	// handed to the handler via JobRuntime.
	handler, ok := w.handlers[job.Type]
	if !ok {
		return nil, fmt.Errorf("unknown job type %q", job.Type)
	}
	return handler.HandleJob(ctx, job, JobRuntime{App: w.app, Provider: provider, Transcriber: transcriber, AgentRunner: w.agentRunner})
}

func (w *Worker) finish(ctx context.Context, claimed persistence.AgentJob, rawToken string, output any, runErr error) error {
	return w.app.Store.Transaction(ctx, func(tx *gorm.DB) error {
		var current persistence.AgentJob
		if err := tx.First(&current, "id = ?", claimed.ID).Error; err != nil {
			return err
		}
		if current.Status != "running" || current.LeaseVersion != claimed.LeaseVersion || current.AttemptCount != claimed.AttemptCount || current.RunTokenHash != persistence.Hash(rawToken) {
			return ErrStaleAgentAttempt
		}
		now := persistence.Now()
		updates := map[string]any{"finished_at": now, "locked_until": nil, "locked_by": "", "run_token_hash": "", "revision": current.Revision + 1, "updated_at": now}
		if current.CancelRequested {
			updates["status"], updates["error_code"], updates["error_message"] = "cancelled", "CANCELLED", "cancelled by user"
			return tx.Model(&persistence.AgentJob{}).Where("id = ? AND status = 'running' AND lease_version = ? AND run_token_hash = ?", current.ID, current.LeaseVersion, persistence.Hash(rawToken)).Updates(updates).Error
		}
		if current.Type == "support_generation" {
			if _, err := validateSupportJobTx(tx, current, now); err != nil {
				code, terminal := supportJobFailureCode(err)
				if !terminal {
					return err
				}
				updates["status"], updates["error_code"], updates["error_message"] = "failed", code, err.Error()
				return tx.Model(&persistence.AgentJob{}).Where("id = ? AND status = 'running' AND lease_version = ? AND run_token_hash = ?", current.ID, current.LeaseVersion, persistence.Hash(rawToken)).Updates(updates).Error
			}
		}
		if runErr != nil {
			if errors.Is(runErr, ErrRevision) {
				updates["status"], updates["error_code"], updates["error_message"] = "failed", "REVISION_MISMATCH", runErr.Error()
			} else if current.AttemptCount < current.MaxAttempts && !strings.Contains(runErr.Error(), "unknown job") {
				updates["status"], updates["run_after"], updates["finished_at"] = "queued", now.Add(jobRetryDelay(current.AttemptCount)), nil
				updates["error_code"], updates["error_message"] = "TEMPORARY", runErr.Error()
			} else {
				updates["status"], updates["error_code"], updates["error_message"] = "failed", "PROVIDER_ERROR", runErr.Error()
			}
		} else {
			encoded, _ := json.Marshal(output)
			updates["status"], updates["output_json"] = "succeeded", string(encoded)
			if err := w.app.MaterializeJobResult(tx, current, output, now); err != nil {
				return err
			}
			if current.Type == "voice_transcription" {
				var input map[string]any
				_ = json.Unmarshal([]byte(current.InputJSON), &input)
				if path := textValue(input["path"]); path != "" {
					_ = os.Remove(path)
				}
			}
		}
		return tx.Model(&persistence.AgentJob{}).Where("id = ? AND status = 'running' AND lease_version = ? AND run_token_hash = ?", current.ID, current.LeaseVersion, persistence.Hash(rawToken)).Updates(updates).Error
	})
}
