package application

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/FastR-D/FastTask/internal/agent"
	"github.com/FastR-D/FastTask/internal/persistence"
	"gorm.io/gorm"
)

// Job dispatch via value groups (wiring.md §5, §4.1).
//
// The Worker's job-type switch and App.MaterializeJobResult's switch are replaced
// by two registries collected from fx value groups ("job_handlers" and
// "job_materializers"). Adding a job type no longer requires editing a central
// switch: a domain provides a handler/materializer and registers it in the group.
//
// Handlers and materializers are stateless — everything they need arrives via
// JobRuntime (for handlers) or the tx/job/output arguments (for materializers) —
// so the same instances serve every job. Each declares the job types it covers so
// one implementation can serve a family (e.g. task_tree_generation + _revision).

// JobRuntime carries the per-execution dependencies the Worker resolves before
// dispatch: the application aggregate (for store/service access) and the model
// provider, transcriber and agent runner in effect for this job.
type JobRuntime struct {
	App         *App
	Provider    agent.Provider
	Transcriber agent.Transcriber
	AgentRunner *AgentService
}

// JobHandler executes one family of job types. Provided into the fx value group
// "job_handlers" and collected by the Worker (wiring.md §5).
type JobHandler interface {
	// JobTypes lists the job.Type values this handler serves.
	JobTypes() []string
	// HandleJob runs the job and returns its output (materialized on success).
	HandleJob(ctx context.Context, job persistence.AgentJob, rt JobRuntime) (any, error)
}

// JobMaterializer folds a succeeded job's output into business tables inside the
// finishing transaction. Provided into the fx value group "job_materializers"
// (wiring.md §4.1: MaterializeJobResult 拆成值组).
type JobMaterializer interface {
	JobTypes() []string
	MaterializeJob(tx *gorm.DB, job persistence.AgentJob, output any, now time.Time) error
}

// ---- handlers -------------------------------------------------------------

// agentRunHandler dispatches agent_run jobs to the agent runtime (agent-impl §8).
type agentRunHandler struct{}

func NewAgentRunHandler() JobHandler { return agentRunHandler{} }
func (agentRunHandler) JobTypes() []string {
	return []string{"agent_run"}
}
func (agentRunHandler) HandleJob(ctx context.Context, job persistence.AgentJob, rt JobRuntime) (any, error) {
	if rt.AgentRunner == nil {
		return nil, fmt.Errorf("agent runtime is not configured")
	}
	return rt.AgentRunner.ExecuteRun(ctx, job)
}

// taskTreeHandler generates or revises a goal's task tree (a proposal, never a
// direct write — the user confirms before it is applied).
type taskTreeHandler struct{}

func NewTaskTreeHandler() JobHandler { return taskTreeHandler{} }
func (taskTreeHandler) JobTypes() []string {
	return []string{"task_tree_generation", "task_tree_revision"}
}
func (taskTreeHandler) HandleJob(ctx context.Context, job persistence.AgentJob, rt JobRuntime) (any, error) {
	var goal persistence.Goal
	if err := rt.App.Store.DB.WithContext(ctx).Where("id = ? AND user_id = ?", job.SubjectID, job.UserID).First(&goal).Error; err != nil {
		return nil, err
	}
	var current int
	rt.App.Store.DB.WithContext(ctx).Model(&persistence.TaskTreeRevision{}).Where("goal_id = ?", goal.ID).Select("COALESCE(MAX(revision),0)").Scan(&current)
	if job.BaseRevision > 0 && current != job.BaseRevision {
		return nil, ErrRevision
	}
	patch := []map[string]any{
		{"op": "create", "type": "milestone", "title": "明确验收路径", "success_criteria": "形成可验证的阶段成果", "minimum_action": "列出三个可验证成果", "priority": 90, "estimate_minutes": 50, "uncertainty": 40, "contribution": 90},
		{"op": "create", "type": "task", "title": "完成第一个可验证推进", "success_criteria": goal.SuccessCriteria, "minimum_action": "打开工作材料并写下第一步", "priority": 80, "estimate_minutes": 50, "uncertainty": 70, "contribution": 85},
		{"op": "create", "type": "task", "title": "复盘结果并调整路线", "success_criteria": "记录结果、阻碍和下一步", "minimum_action": "写下当前最大阻碍", "priority": 60, "estimate_minutes": 25, "uncertainty": 30, "contribution": 60},
	}
	providerName := "local-deterministic"
	if rt.Provider != nil {
		var input map[string]any
		_ = json.Unmarshal([]byte(job.InputJSON), &input)
		instruction := fmt.Sprint(input["instruction"])
		if job.Type == "task_tree_revision" {
			var tasks []persistence.Task
			_ = rt.App.Store.DB.WithContext(ctx).Where("goal_id = ? AND user_id = ?", goal.ID, job.UserID).Order("position, id").Find(&tasks).Error
			tree, _ := json.Marshal(tasks)
			instruction += "\n现有任务（修订时可使用 update/move/supersede，target_id 必须来自这里）：" + string(tree)
		}
		generated, err := rt.Provider.TaskProposal(ctx, goal.Title, goal.SuccessCriteria, instruction)
		if err != nil {
			return nil, err
		}
		patch, providerName = generated, rt.Provider.Name()
	}
	return map[string]any{"provider": providerName, "proposal": patch, "assumptions": []string{"结构变更应用前需用户确认"}}, nil
}

// dailyPlanHandler generates or replans the day's plan deterministically.
type dailyPlanHandler struct{}

func NewDailyPlanHandler() JobHandler { return dailyPlanHandler{} }
func (dailyPlanHandler) JobTypes() []string {
	return []string{"daily_plan_generation"}
}
func (dailyPlanHandler) HandleJob(ctx context.Context, job persistence.AgentJob, rt JobRuntime) (any, error) {
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
		plan, items, err = rt.App.ReplanDailyPlan(ctx, job.UserID, input.LocalDate, input.Timezone, nil, input.AvailableMinutes, input.BaseRevision)
	} else {
		plan, items, err = rt.App.CreateDailyPlan(ctx, job.UserID, input.LocalDate, input.Timezone, nil, input.AvailableMinutes)
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"plan": plan, "items": items}, nil
}

// conversationHandler is the legacy single-turn reply path (agent-impl §8.1 keeps
// it for jobs queued before the agent runtime; no new jobs are created).
type conversationHandler struct{}

func NewConversationHandler() JobHandler { return conversationHandler{} }
func (conversationHandler) JobTypes() []string {
	return []string{"conversation"}
}
func (conversationHandler) HandleJob(ctx context.Context, job persistence.AgentJob, rt JobRuntime) (any, error) {
	var input map[string]any
	_ = json.Unmarshal([]byte(job.InputJSON), &input)
	content := fmt.Sprint(input["content"])
	reply := "已分析你的输入。建议先执行一个 5-15 分钟的最小行动，再根据结果调整任务树。"
	providerName := "local-deterministic"
	if rt.Provider != nil {
		generated, err := rt.Provider.ConversationReply(ctx, content)
		if err != nil {
			return nil, err
		}
		reply, providerName = generated, rt.Provider.Name()
	}
	return map[string]any{"reply": reply, "echo": content, "provider": providerName}, nil
}

// voiceTranscriptionHandler transcribes an uploaded audio file.
type voiceTranscriptionHandler struct{}

func NewVoiceTranscriptionHandler() JobHandler { return voiceTranscriptionHandler{} }
func (voiceTranscriptionHandler) JobTypes() []string {
	return []string{"voice_transcription"}
}
func (voiceTranscriptionHandler) HandleJob(ctx context.Context, job persistence.AgentJob, rt JobRuntime) (any, error) {
	var input map[string]any
	_ = json.Unmarshal([]byte(job.InputJSON), &input)
	path, name := textValue(input["path"]), textValue(input["filename"])
	if rt.Transcriber != nil {
		transcript, err := rt.Transcriber.Transcribe(ctx, path)
		if err != nil {
			return nil, err
		}
		return map[string]any{"transcript": transcript, "provider": rt.Transcriber.Name()}, nil
	}
	return map[string]any{"transcript": "[演示转写，未配置 STT] 音频文件：" + name, "provider": "local-demo-no-stt"}, nil
}

// supportGenerationHandler suggests one input-type support item for a stalled plan.
type supportGenerationHandler struct{}

func NewSupportGenerationHandler() JobHandler { return supportGenerationHandler{} }
func (supportGenerationHandler) JobTypes() []string {
	return []string{"support_generation"}
}
func (supportGenerationHandler) HandleJob(context.Context, persistence.AgentJob, JobRuntime) (any, error) {
	return map[string]any{"suggestions": []map[string]any{{"kind": "input", "title": "阅读与当前阻碍直接相关的资料", "commitment": "限定范围阅读并记录三条与当前阻碍直接相关的信息", "minimum_action": "打开一份相关资料并定位摘要", "minutes": 25}}}, nil
}

// BuiltinJobHandlers is the default handler set, used when the Worker is built
// without an injected value group (unit tests construct services directly per
// wiring.md §2 rule 4). Bootstrap registers these same constructors into the
// "job_handlers" group so production dispatch is value-group driven.
func BuiltinJobHandlers() []JobHandler {
	return []JobHandler{
		NewAgentRunHandler(), NewTaskTreeHandler(), NewDailyPlanHandler(),
		NewConversationHandler(), NewVoiceTranscriptionHandler(), NewSupportGenerationHandler(),
	}
}

// ---- materializers --------------------------------------------------------

// taskTreeMaterializer stages the generated patch as a pending Proposal.
type taskTreeMaterializer struct{}

func NewTaskTreeMaterializer() JobMaterializer { return taskTreeMaterializer{} }
func (taskTreeMaterializer) JobTypes() []string {
	return []string{"task_tree_generation", "task_tree_revision"}
}
func (taskTreeMaterializer) MaterializeJob(tx *gorm.DB, job persistence.AgentJob, output any, now time.Time) error {
	encoded, _ := json.Marshal(output)
	var result map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &result); err != nil || len(result["proposal"]) == 0 {
		return ErrValidation
	}
	proposal := persistence.Proposal{ID: persistence.NewID("proposal"), UserID: job.UserID, GoalID: job.SubjectID, JobID: job.ID, BaseRevision: job.BaseRevision, Status: "pending", Instruction: "agent proposal", PatchJSON: string(result["proposal"]), Revision: 1, CreatedAt: now, UpdatedAt: now}
	return tx.Create(&proposal).Error
}

// conversationMaterializer appends the assistant reply to the conversation.
type conversationMaterializer struct{}

func NewConversationMaterializer() JobMaterializer { return conversationMaterializer{} }
func (conversationMaterializer) JobTypes() []string {
	return []string{"conversation"}
}
func (conversationMaterializer) MaterializeJob(tx *gorm.DB, job persistence.AgentJob, output any, now time.Time) error {
	encoded, _ := json.Marshal(output)
	var input map[string]any
	_ = json.Unmarshal([]byte(job.InputJSON), &input)
	var result map[string]any
	_ = json.Unmarshal(encoded, &result)
	conversationID, reply := textValue(input["conversation_id"]), textValue(result["reply"])
	if conversationID == "" || reply == "" {
		return ErrValidation
	}
	return tx.Create(&persistence.ConversationMessage{ID: persistence.NewID("msg"), UserID: job.UserID, ConversationID: conversationID, Role: "assistant", Content: reply, JobID: job.ID, CreatedAt: now}).Error
}

// supportMaterializer adds the suggested input item once every core item is done.
type supportMaterializer struct{}

func NewSupportMaterializer() JobMaterializer { return supportMaterializer{} }
func (supportMaterializer) JobTypes() []string {
	return []string{"support_generation"}
}
func (supportMaterializer) MaterializeJob(tx *gorm.DB, job persistence.AgentJob, output any, now time.Time) error {
	var plan persistence.DailyPlan
	if err := tx.Where("id = ? AND user_id = ?", job.SubjectID, job.UserID).First(&plan).Error; err != nil {
		return notFound(err)
	}
	var unsatisfied int64
	if err := tx.Model(&persistence.DailyPlanItem{}).Where("plan_id = ? AND kind = 'core' AND status NOT IN ('satisfied','superseded')", plan.ID).Count(&unsatisfied).Error; err != nil {
		return err
	}
	if unsatisfied > 0 {
		return ErrConflict
	}
	item := persistence.DailyPlanItem{ID: persistence.NewID("dpi"), UserID: job.UserID, PlanID: plan.ID, PlanRevision: plan.CurrentRevision, Kind: "input", Title: "阅读与当前阻碍直接相关的资料", Commitment: "限定范围阅读并记录三条与当前阻碍直接相关的信息", MinimumAction: "打开一份相关资料并定位摘要", AllowedTypes: "result,step,time,minimum_action", TargetMinutes: 25, Status: "planned", Position: 1, Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := tx.Create(&item).Error; err != nil {
		return err
	}
	result := tx.Model(&persistence.DailyPlan{}).Where("id = ? AND revision = ?", plan.ID, plan.Revision).Updates(map[string]any{"revision": plan.Revision + 1, "updated_at": now})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrRevision
	}
	return nil
}

// BuiltinJobMaterializers is the default materializer set (see BuiltinJobHandlers).
func BuiltinJobMaterializers() []JobMaterializer {
	return []JobMaterializer{NewTaskTreeMaterializer(), NewConversationMaterializer(), NewSupportMaterializer()}
}

// ---- registries -----------------------------------------------------------

func handlerMap(handlers []JobHandler) map[string]JobHandler {
	m := make(map[string]JobHandler, len(handlers))
	for _, h := range handlers {
		for _, t := range h.JobTypes() {
			m[t] = h
		}
	}
	return m
}

func materializerMap(materializers []JobMaterializer) map[string]JobMaterializer {
	m := make(map[string]JobMaterializer, len(materializers))
	for _, mat := range materializers {
		for _, t := range mat.JobTypes() {
			m[t] = mat
		}
	}
	return m
}
