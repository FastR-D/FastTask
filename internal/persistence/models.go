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
	AuthSource, CasIssuer, CasLinkID                          string
	CasSID                                                    string `gorm:"column:cas_sid"`
	CasLinkVersion                                            int64
}

type FastCASTransaction struct {
	State     string `gorm:"primaryKey"`
	Payload   string
	ExpiresAt time.Time
}

func (FastCASTransaction) TableName() string { return "fastcas_transactions" }

type FastCASLink struct {
	ID         string    `json:"id" gorm:"primaryKey"`
	Issuer     string    `json:"issuer"`
	ClientID   string    `json:"client_id"`
	UserID     string    `json:"local_account_ref"`
	Subject    string    `json:"subject"`
	State      string    `json:"state"`
	Version    int64     `json:"version"`
	VerifiedAt time.Time `json:"verified_at"`
}

func (FastCASLink) TableName() string { return "fastcas_links" }

type FastCASEvent struct {
	Issuer      string `gorm:"primaryKey"`
	ID          string `gorm:"primaryKey"`
	ProcessedAt time.Time
}

func (FastCASEvent) TableName() string { return "fastcas_events" }

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

type TaskCoord struct {
	ID        string    `json:"id"`
	UserID    string    `json:"-"`
	TaskID    string    `json:"task_id"`
	Lens      string    `json:"lens"`
	X         int       `json:"x"`
	Y         int       `json:"y"`
	Source    string    `json:"source"`
	Pinned    bool      `json:"pinned"`
	Rationale string    `json:"rationale"`
	Revision  int       `json:"revision"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
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

// AgentRun is one execution of the agent loop for a thread (doc/agent-impl.md
// §3, §4). ThreadID is the reused conversations.id. JobID links the AgentJob
// that currently carries the run; a run outlives individual jobs because an
// approval receipt starts a new job against the same run (§4.0, §7.2).
// StateJSON is the retained snapshot used to resume a stream (§2.8).
type AgentRun struct {
	ID              string  `json:"id"`
	UserID          string  `json:"-"`
	ThreadID        string  `json:"thread_id"`
	JobID           string  `json:"job_id,omitempty"`
	Status          string  `json:"status"`
	StateJSON       string  `json:"-"`
	CheckpointSeq   int     `json:"checkpoint_seq"`
	ParentMessageID *string `json:"parent_message_id,omitempty"`
	// HarnessMode records who drives the run: "" is the in-process loop,
	// "wasm" the browser host, "sidecar" the Node host (doc/harness.md §1.2).
	HarnessMode string `json:"harness_mode,omitempty"`
	// CancelRequested is the durable half of a cancellation; the other two
	// channels are the heartbeat response and the open streams (§5.2).
	CancelRequested bool `json:"cancel_requested,omitempty"`
	// ApprovalWaitMs accumulates time spent awaiting approval, which §7 excludes from the
	// run wall clock.
	ApprovalWaitMs int64 `json:"approval_wait_ms,omitempty"`
	// ModelCalls counts the proxied model calls of a run. The host drives the loop, but the
	// turn budget stays a server-side limit (doc/harness.md §1.1).
	ModelCalls   int        `json:"model_calls,omitempty"`
	ErrorCode    string     `json:"error_code,omitempty"`
	ErrorMessage string     `json:"error_message,omitempty"`
	Revision     int        `json:"revision"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
}

// AgentHarnessToken is one issued run capability (doc/harness.md §10). Only
// TokenHash is stored: the plaintext is handed to the host once, at issuance, and
// can never be read back. A token dies when it expires, when the run reaches a
// terminal state, or when a newer token for the same run replaces it.
type AgentHarnessToken struct {
	ID        string `json:"id"`
	TokenHash string `json:"-"`
	UserID    string `json:"-"`
	ThreadID  string `json:"thread_id"`
	RunID     string `json:"run_id"`
	Mode      string `json:"mode"`
	// The column is tools_etag; GORM would otherwise split the initialism into tools_e_tag.
	ToolsETag       string     `gorm:"column:tools_etag" json:"tools_etag,omitempty"`
	ExpiresAt       time.Time  `json:"expires_at"`
	LastHeartbeatAt *time.Time `json:"last_heartbeat_at,omitempty"`
	RevokedAt       *time.Time `json:"revoked_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
}

// AgentMessage is one message in a thread. Seq is thread-scoped and monotonic,
// guaranteeing ordering (§3.1). ParentID is reserved for future branching and is
// nil in v1, where a thread is strictly linear (§4.1).
type AgentMessage struct {
	ID        string    `json:"id"`
	UserID    string    `json:"-"`
	ThreadID  string    `json:"thread_id"`
	RunID     string    `json:"run_id"`
	ParentID  *string   `json:"parent_id,omitempty"`
	Role      string    `json:"role"`
	Seq       int       `json:"seq"`
	CreatedAt time.Time `json:"created_at"`
}

// AgentMessagePart is one part of a message: text, tool-call, or tool-result
// (§2.7). ToolCallID is the unique locator for add-tool-result receipts (§7).
// ApprovalStatus and ProposalID carry the proposal-approval state (§7).
type AgentMessagePart struct {
	ID             string    `json:"id"`
	UserID         string    `json:"-"`
	MessageID      string    `json:"message_id"`
	Idx            int       `json:"idx"`
	Type           string    `json:"type"`
	Text           string    `json:"text,omitempty"`
	ToolCallID     *string   `json:"tool_call_id,omitempty"`
	ToolName       string    `json:"tool_name,omitempty"`
	ArgsJSON       string    `json:"args_json,omitempty"`
	ResultJSON     string    `json:"result_json,omitempty"`
	IsError        bool      `json:"is_error,omitempty"`
	ArtifactJSON   string    `json:"artifact_json,omitempty"`
	ApprovalStatus string    `json:"approval_status,omitempty"`
	ProposalID     *string   `json:"proposal_id,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// AgentThread is one persisted agent conversation (doc/chat-features.md §2.3).
// It replaced the conversations table as the agent's Thread: a thread owns the
// libfx checkpoint, the regular/archived status and the client-owned `custom`
// blob, none of which belong to the conversation aggregate.
//
// Custom is untrusted display data written by the client; nothing that takes part
// in authorization or an invariant may be read from it. Checkpoint is opaque
// versioned bytes whose format belongs to libfx_version (doc/harness.md §6).
type AgentThread struct {
	ID           string  `json:"id"`
	UserID       string  `json:"-"`
	GoalID       *string `json:"goal_id,omitempty"`
	Title        string  `json:"title"`
	Status       string  `json:"status"`
	Custom       string  `json:"custom,omitempty"`
	Checkpoint   []byte  `json:"-"`
	LibfxVersion string  `json:"libfx_version,omitempty"`
	// SourceConversationID is set only by migration 000006, which backfilled one
	// thread per pre-existing conversation. It is never written afterwards.
	SourceConversationID *string    `json:"-"`
	LastMessageAt        *time.Time `json:"last_message_at,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
	UpdatedAt            time.Time  `json:"updated_at"`
}

// Thread statuses (doc/chat-features.md §2.2): archived keeps every row, delete
// removes them.
const (
	ThreadRegular  = "regular"
	ThreadArchived = "archived"
)

// AgentAttachment is one uploaded image (doc/chat-features.md §4.3). The mime type is the sniffed one:
// a client's Content-Type and a file's extension are both claims, and §4.5 accepts neither.
//
// ThreadID is nil until the attachment is sent with a message; an attachment that is never sent is an
// orphan and is reclaimed by TTL (§4.5).
type AgentAttachment struct {
	ID        string    `json:"id"`
	UserID    string    `json:"-"`
	ThreadID  *string   `json:"thread_id,omitempty"`
	MessageID *string   `json:"message_id,omitempty"`
	Mime      string    `json:"mime"`
	Bytes     int64     `json:"bytes"`
	Width     int       `json:"width"`
	Height    int       `json:"height"`
	Path      string    `json:"-"`
	CreatedAt time.Time `json:"created_at"`
}

// AgentRunChunk is one emitted protocol chunk in a run's persistent log. Chunks
// are replayed on reconnect (§2.8, §8). The primary key is (RunID, Seq).
type AgentRunChunk struct {
	RunID     string    `gorm:"primaryKey" json:"run_id"`
	Seq       int       `gorm:"primaryKey" json:"seq"`
	UserID    string    `json:"-"`
	ChunkJSON string    `json:"chunk_json"`
	CreatedAt time.Time `json:"created_at"`
}

// NotificationChannel is one provider an administrator configured (doc/notification.md §3).
// Provider is one of the four adapters internal/notify builds; SettingsJSON is that
// adapter's non-secret configuration, and SecretCiphertext is its credential — a Telegram
// bot token, an FCM service-account key, an APNs .p8 key — encrypted with the same AES-GCM
// key that protects model-provider API keys. Bark needs no credential, so the ciphertext
// may legitimately be empty for it.
//
// LastCheckStatus is the outcome of the last credential check an administrator ran; it is
// diagnostic and never gates delivery.
type NotificationChannel struct {
	ID               string     `json:"id"`
	Name             string     `json:"name"`
	Provider         string     `json:"provider"`
	Endpoint         string     `json:"endpoint"`
	SettingsJSON     string     `json:"-"`
	SecretCiphertext string     `json:"-"`
	SecretHint       string     `json:"secret_hint"`
	Status           string     `json:"status"`
	LastCheckAt      *time.Time `json:"last_check_at,omitempty"`
	LastCheckStatus  string     `json:"last_check_status"`
	LastError        string     `json:"last_error,omitempty"`
	Revision         int        `json:"revision"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

// NotificationTarget is one address a user can be reached at on one channel: a Telegram
// chat id, a Bark device key, an FCM registration token, an APNs device token.
//
// The address is a credential — an FCM token in hand is the ability to push to somebody's
// phone — so only its ciphertext, its hash and a masked hint are stored. The hash is what
// makes the one-registration-per-address rule exact (migration 000010's unique index) and
// the hint is what a person recognises the target by.
//
// Status "invalid" is written by the dispatcher when a provider permanently rejects the
// address, which is how an uninstalled app stops being retried forever.
type NotificationTarget struct {
	ID                string     `json:"id"`
	UserID            string     `json:"-"`
	ChannelID         string     `json:"channel_id"`
	Label             string     `json:"label"`
	AddressCiphertext string     `json:"-"`
	AddressHash       string     `json:"-"`
	AddressHint       string     `json:"address_hint"`
	Status            string     `json:"status"`
	FailureCount      int        `json:"failure_count"`
	LastError         string     `json:"last_error,omitempty"`
	LastSentAt        *time.Time `json:"last_sent_at,omitempty"`
	Revision          int        `json:"revision"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

// NotificationMessage is one queued or finished delivery (doc/notification.md §5). The row
// is both the queue and the audit trail: `status` and `run_after` decide when a dispatcher
// may claim it, `attempts` and `max_attempts` bound the retries, and `provider_message_id`
// is the provider's own id so a delivery can be traced on their side.
type NotificationMessage struct {
	ID                string     `json:"id"`
	UserID            string     `json:"user_id"`
	ChannelID         string     `json:"channel_id"`
	TargetID          string     `json:"target_id"`
	Topic             string     `json:"topic"`
	Title             string     `json:"title"`
	Body              string     `json:"body"`
	URL               string     `json:"url"`
	PayloadJSON       string     `json:"payload_json"`
	Status            string     `json:"status"`
	Attempts          int        `json:"attempts"`
	MaxAttempts       int        `json:"max_attempts"`
	ProviderMessageID string     `json:"provider_message_id,omitempty"`
	LastError         string     `json:"last_error,omitempty"`
	RunAfter          time.Time  `json:"run_after"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
	SentAt            *time.Time `json:"sent_at,omitempty"`
}

// Notification message statuses. A claimed row stays "sending" with run_after as its lease
// expiry, so a dispatcher that died mid-send leaves a row that becomes due again instead of
// one nobody will ever look at.
const (
	NotificationQueued  = "queued"
	NotificationSending = "sending"
	NotificationSent    = "sent"
	NotificationFailed  = "failed"
)
