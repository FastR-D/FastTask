package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/FastR-D/FastTask/internal/agent"
	"github.com/FastR-D/FastTask/internal/persistence"
	"gorm.io/gorm"
)

type Worker struct {
	app         *App
	identity    string
	interval    time.Duration
	provider    agent.Provider
	transcriber agent.Transcriber
}

func (w *Worker) WithTranscriber(transcriber agent.Transcriber) *Worker {
	w.transcriber = transcriber
	return w
}

func NewWorker(app *App, interval time.Duration, providers ...agent.Provider) *Worker {
	worker := &Worker{app: app, identity: persistence.NewID("worker"), interval: interval}
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
		}
	}
}

func (w *Worker) RunOnce(ctx context.Context) error {
	job, token, err := w.claim(ctx)
	if err != nil || job == nil {
		return err
	}
	output, runErr := w.execute(ctx, *job)
	return w.finish(ctx, *job, token, output, runErr)
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
		until := now.Add(2 * time.Minute)
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
	switch job.Type {
	case "task_tree_generation", "task_tree_revision":
		var goal persistence.Goal
		if err := w.app.Store.DB.WithContext(ctx).Where("id = ? AND user_id = ?", job.SubjectID, job.UserID).First(&goal).Error; err != nil {
			return nil, err
		}
		var current int
		w.app.Store.DB.WithContext(ctx).Model(&persistence.TaskTreeRevision{}).Where("goal_id = ?", goal.ID).Select("COALESCE(MAX(revision),0)").Scan(&current)
		if job.BaseRevision > 0 && current != job.BaseRevision {
			return nil, ErrRevision
		}
		patch := []map[string]any{
			{"op": "create", "type": "milestone", "title": "明确验收路径", "success_criteria": "形成可验证的阶段成果", "minimum_action": "列出三个可验证成果", "priority": 90, "estimate_minutes": 50},
			{"op": "create", "type": "task", "title": "完成第一个可验证推进", "success_criteria": goal.SuccessCriteria, "minimum_action": "打开工作材料并写下第一步", "priority": 80, "estimate_minutes": 50},
			{"op": "create", "type": "task", "title": "复盘结果并调整路线", "success_criteria": "记录结果、阻碍和下一步", "minimum_action": "写下当前最大阻碍", "priority": 60, "estimate_minutes": 25},
		}
		providerName := "local-deterministic"
		if w.provider != nil {
			var input map[string]any
			_ = json.Unmarshal([]byte(job.InputJSON), &input)
			instruction := fmt.Sprint(input["instruction"])
			if job.Type == "task_tree_revision" {
				var tasks []persistence.Task
				_ = w.app.Store.DB.WithContext(ctx).Where("goal_id = ? AND user_id = ?", goal.ID, job.UserID).Order("position, id").Find(&tasks).Error
				tree, _ := json.Marshal(tasks)
				instruction += "\n现有任务（修订时可使用 update/move/supersede，target_id 必须来自这里）：" + string(tree)
			}
			generated, err := w.provider.TaskProposal(ctx, goal.Title, goal.SuccessCriteria, instruction)
			if err != nil {
				return nil, err
			}
			patch, providerName = generated, w.provider.Name()
		}
		return map[string]any{"provider": providerName, "proposal": patch, "assumptions": []string{"结构变更应用前需用户确认"}}, nil
	case "daily_plan_generation":
		var input struct {
			LocalDate, Timezone string
			AvailableMinutes    int  `json:"available_minutes"`
			ReplaceExisting     bool `json:"replace_existing"`
			BaseRevision        int  `json:"base_revision"`
		}
		_ = json.Unmarshal([]byte(job.InputJSON), &input)
		if input.LocalDate == "" {
			input.LocalDate = time.Now().Format("2006-01-02")
		}
		if input.Timezone == "" {
			input.Timezone = "Asia/Shanghai"
		}
		var plan *persistence.DailyPlan
		var items []persistence.DailyPlanItem
		var err error
		if input.ReplaceExisting {
			plan, items, err = w.app.ReplanDailyPlan(ctx, job.UserID, input.LocalDate, input.Timezone, nil, input.AvailableMinutes, input.BaseRevision)
		} else {
			plan, items, err = w.app.CreateDailyPlan(ctx, job.UserID, input.LocalDate, input.Timezone, nil, input.AvailableMinutes)
		}
		if err != nil {
			return nil, err
		}
		return map[string]any{"plan": plan, "items": items}, nil
	case "conversation":
		var input map[string]any
		_ = json.Unmarshal([]byte(job.InputJSON), &input)
		content := fmt.Sprint(input["content"])
		reply := "已分析你的输入。建议先执行一个 5-15 分钟的最小行动，再根据结果调整任务树。"
		providerName := "local-deterministic"
		if w.provider != nil {
			generated, err := w.provider.ConversationReply(ctx, content)
			if err != nil {
				return nil, err
			}
			reply, providerName = generated, w.provider.Name()
		}
		return map[string]any{"reply": reply, "echo": content, "provider": providerName}, nil
	case "voice_transcription":
		var input map[string]any
		_ = json.Unmarshal([]byte(job.InputJSON), &input)
		path, name := textValue(input["path"]), textValue(input["filename"])
		if w.transcriber != nil {
			transcript, err := w.transcriber.Transcribe(ctx, path)
			if err != nil {
				return nil, err
			}
			return map[string]any{"transcript": transcript, "provider": w.transcriber.Name()}, nil
		}
		return map[string]any{"transcript": "[演示转写，未配置 STT] 音频文件：" + name, "provider": "local-demo-no-stt"}, nil
	case "support_generation":
		return map[string]any{"suggestions": []map[string]any{{"kind": "input", "title": "阅读与当前阻碍直接相关的资料", "commitment": "限定范围阅读并记录三条与当前阻碍直接相关的信息", "minimum_action": "打开一份相关资料并定位摘要", "minutes": 25}}}, nil
	default:
		return nil, fmt.Errorf("unknown job type %q", job.Type)
	}
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
		if runErr != nil {
			if errors.Is(runErr, ErrRevision) {
				updates["status"], updates["error_code"], updates["error_message"] = "failed", "REVISION_MISMATCH", runErr.Error()
			} else if current.AttemptCount < current.MaxAttempts && !strings.Contains(runErr.Error(), "unknown job") {
				updates["status"], updates["run_after"], updates["finished_at"] = "queued", now.Add(time.Duration(current.AttemptCount)*time.Second), nil
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
