package persistence

import "time"

type User struct {
	ID           string    `json:"id"`
	Identifier   string    `json:"identifier"`
	PasswordHash string    `json:"-"`
	DisplayName  string    `json:"display_name"`
	Timezone     string    `json:"timezone"`
	Locale       string    `json:"locale"`
	Role         string    `json:"role"`
	Status       string    `json:"status"`
	Revision     int       `json:"revision"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type Session struct {
	ID, FamilyID, UserID, RefreshHash, ReplacedByHash, Status string
	ExpiresAt, CreatedAt, UpdatedAt                           time.Time
}

type AdminAuditEvent struct {
	ID           string    `json:"id"`
	ActorUserID  string    `json:"actor_user_id"`
	TargetUserID *string   `json:"target_user_id,omitempty"`
	Action       string    `json:"action"`
	DetailJSON   string    `json:"detail_json"`
	CreatedAt    time.Time `json:"created_at"`
}

type ModelProvider struct {
	ID                 string    `json:"id"`
	Name               string    `json:"name"`
	ProviderType       string    `json:"provider_type"`
	BaseURL            string    `json:"base_url"`
	ModelName          string    `json:"model_name"`
	TranscriptionModel string    `json:"transcription_model"`
	APIKeyCiphertext   string    `json:"-"`
	APIKeyHint         string    `json:"api_key_hint"`
	Status             string    `json:"status"`
	IsDefault          bool      `json:"is_default"`
	Revision           int       `json:"revision"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

type Goal struct {
	ID              string    `json:"id"`
	UserID          string    `json:"-"`
	Title           string    `json:"title"`
	Description     string    `json:"description"`
	SuccessCriteria string    `json:"success_criteria"`
	Status          string    `json:"status"`
	TargetDate      *string   `json:"target_date,omitempty"`
	Revision        int       `json:"revision"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type Task struct {
	ID              string    `json:"id"`
	UserID          string    `json:"-"`
	GoalID          string    `json:"goal_id"`
	ParentID        *string   `json:"parent_id,omitempty"`
	Type            string    `json:"type"`
	Title           string    `json:"title"`
	Description     string    `json:"description"`
	Status          string    `json:"status"`
	SuccessCriteria string    `json:"success_criteria"`
	MinimumAction   string    `json:"minimum_action"`
	BlockedReason   string    `json:"blocked_reason,omitempty"`
	Priority        int       `json:"priority"`
	Position        int       `json:"position"`
	EstimateMinutes int       `json:"estimate_minutes"`
	Revision        int       `json:"revision"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type TaskTreeRevision struct {
	ID           string    `json:"id"`
	UserID       string    `json:"-"`
	GoalID       string    `json:"goal_id"`
	Reason       string    `json:"reason"`
	Source       string    `json:"source"`
	SnapshotJSON string    `json:"snapshot_json"`
	Revision     int       `json:"revision"`
	CreatedAt    time.Time `json:"created_at"`
}

type Proposal struct {
	ID           string    `json:"id"`
	UserID       string    `json:"-"`
	GoalID       string    `json:"goal_id"`
	JobID        string    `json:"job_id,omitempty"`
	Status       string    `json:"status"`
	Instruction  string    `json:"instruction"`
	PatchJSON    string    `json:"patch_json"`
	BaseRevision int       `json:"base_revision"`
	Revision     int       `json:"revision"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type DailyPlan struct {
	ID               string    `json:"id"`
	UserID           string    `json:"-"`
	LocalDate        string    `json:"local_date"`
	Timezone         string    `json:"timezone"`
	Status           string    `json:"status"`
	AlgorithmVersion string    `json:"algorithm_version"`
	CurrentRevision  int       `json:"current_revision"`
	Revision         int       `json:"revision"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type DailyPlanRevision struct {
	ID            string    `json:"id"`
	PlanID        string    `json:"daily_plan_id"`
	InputSnapshot string    `json:"input_snapshot"`
	InputHash     string    `json:"input_hash"`
	PlanSnapshot  string    `json:"plan_snapshot"`
	Reason        string    `json:"reason"`
	Revision      int       `json:"revision"`
	CreatedAt     time.Time `json:"created_at"`
}

type DailyPlanItem struct {
	ID                string    `json:"id"`
	UserID            string    `json:"-"`
	PlanID            string    `json:"daily_plan_id"`
	TaskID            *string   `json:"task_id,omitempty"`
	Kind              string    `json:"kind"`
	Title             string    `json:"title"`
	Commitment        string    `json:"commitment"`
	MinimumAction     string    `json:"minimum_action"`
	AllowedTypes      string    `json:"allowed_types"`
	Status            string    `json:"status"`
	CompletionType    string    `json:"completion_type,omitempty"`
	CompletionSummary string    `json:"completion_summary,omitempty"`
	PlanRevision      int       `json:"plan_revision"`
	TargetMinutes     int       `json:"target_minutes"`
	Position          int       `json:"position"`
	Revision          int       `json:"revision"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

type WorkSession struct {
	ID                 string     `json:"id"`
	UserID             string     `json:"-"`
	TaskID             string     `json:"task_id"`
	DailyPlanItemID    *string    `json:"daily_plan_item_id,omitempty"`
	SessionType        string     `json:"session_type"`
	Status             string     `json:"status"`
	Outcome            string     `json:"outcome,omitempty"`
	Note               string     `json:"note,omitempty"`
	TargetMinutes      int        `json:"target_minutes"`
	AccumulatedSeconds int        `json:"-"`
	DurationSeconds    int        `json:"duration_seconds"`
	Revision           int        `json:"revision"`
	StartedAt          time.Time  `json:"started_at"`
	PausedAt           *time.Time `json:"paused_at,omitempty"`
	EndedAt            *time.Time `json:"ended_at,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

type ProgressEvent struct {
	ID           string    `json:"id"`
	UserID       string    `json:"-"`
	Type         string    `json:"type"`
	Summary      string    `json:"summary"`
	EvidenceJSON string    `json:"evidence_json"`
	GoalID       *string   `json:"goal_id,omitempty"`
	TaskID       *string   `json:"task_id,omitempty"`
	PlanItemID   *string   `json:"plan_item_id,omitempty"`
	OccurredAt   time.Time `json:"occurred_at"`
	CreatedAt    time.Time `json:"created_at"`
}

type ExternalImport struct {
	ID               string     `json:"id"`
	UserID           string     `json:"-"`
	SchemaVersion    string     `json:"schema_version"`
	TraceID          string     `json:"trace_id,omitempty"`
	SourceSystem     string     `json:"-"`
	SourceExternalID string     `json:"-"`
	SourceURL        string     `json:"-"`
	ContentHash      string     `json:"-"`
	Kind             string     `json:"kind"`
	Title            string     `json:"title"`
	Description      string     `json:"description"`
	SuggestedGoalID  *string    `json:"suggested_goal_id,omitempty"`
	ArtifactsJSON    string     `json:"-"`
	MetadataJSON     string     `json:"-"`
	Status           string     `json:"status"`
	TaskID           *string    `json:"task_id,omitempty"`
	DecisionNote     string     `json:"decision_note,omitempty"`
	Revision         int        `json:"revision"`
	DecidedAt        *time.Time `json:"decided_at,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

type Conversation struct {
	ID        string    `json:"id"`
	UserID    string    `json:"-"`
	Title     string    `json:"title"`
	Status    string    `json:"status"`
	GoalID    *string   `json:"goal_id,omitempty"`
	Revision  int       `json:"revision"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type ConversationMessage struct {
	ID                 string    `json:"id"`
	UserID             string    `json:"-"`
	ConversationID     string    `json:"conversation_id"`
	Role               string    `json:"role"`
	Content            string    `json:"content"`
	JobID              string    `json:"job_id,omitempty"`
	TranscriptionJobID string    `json:"transcription_job_id,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
}

type AgentJob struct {
	ID              string     `json:"id"`
	UserID          string     `json:"-"`
	Type            string     `json:"type"`
	Status          string     `json:"status"`
	RetryOfJobID    *string    `json:"retry_of_job_id,omitempty"`
	SubjectType     string     `json:"subject_type,omitempty"`
	SubjectID       string     `json:"subject_id,omitempty"`
	InputJSON       string     `json:"-"`
	OutputJSON      string     `json:"output_json,omitempty"`
	ErrorCode       string     `json:"error_code,omitempty"`
	ErrorMessage    string     `json:"error_message,omitempty"`
	LockedBy        string     `json:"-"`
	RunTokenHash    string     `json:"-"`
	BaseRevision    int        `json:"base_revision"`
	AttemptCount    int        `json:"attempt_count"`
	MaxAttempts     int        `json:"max_attempts"`
	LeaseVersion    int        `json:"-"`
	Revision        int        `json:"revision"`
	CancelRequested bool       `json:"cancel_requested"`
	RunAfter        time.Time  `json:"run_after"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	LockedUntil     *time.Time `json:"-"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
	FinishedAt      *time.Time `json:"finished_at,omitempty"`
}

type Device struct {
	ID               string     `json:"id"`
	UserID           string     `json:"-"`
	Name             string     `json:"name"`
	Kind             string     `json:"kind"`
	Timezone         string     `json:"timezone"`
	CapabilitiesJSON string     `json:"capabilities_json"`
	TokenHash        string     `json:"-"`
	Status           string     `json:"status"`
	LastSeenAt       *time.Time `json:"last_seen_at,omitempty"`
	Revision         int        `json:"revision"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

type IdempotencyRecord struct {
	ID, Principal, Method, Path, IdemKey, RequestHash, ResponseJSON string
	StatusCode                                                      int
	ExpiresAt, CreatedAt                                            time.Time
}

type OutboxEvent struct {
	ID, UserID, EventType, PayloadJSON, Status string
	Attempts                                   int
	CreatedAt                                  time.Time
	ProcessedAt                                *time.Time
}
