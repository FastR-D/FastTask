export interface User { id: string; identifier: string; display_name: string; timezone: string; locale: string; role: string; status: string; revision: number; created_at: string }
export interface Goal { id: string; title: string; description: string; success_criteria: string; status: string; target_date?: string; revision: number }
export interface Task { id: string; goal_id: string; parent_id?: string; type: string; title: string; description: string; status: string; priority: number; estimate_minutes: number; success_criteria: string; minimum_action: string; revision: number }
export interface Plan { id: string; local_date: string; timezone: string; status: string; revision: number; current_revision: number }
export interface PlanItem { id: string; daily_plan_id: string; task_id?: string; kind: string; title: string; commitment: string; minimum_action: string; status: string; target_minutes: number; revision: number }
export interface Job { id: string; type: string; status: string; retry_of_job_id?: string; subject_type?: string; subject_id?: string; output_json?: string; error_code?: string; error_message?: string; base_revision: number; attempt_count: number; max_attempts: number; cancel_requested: boolean; run_after: string; created_at: string; updated_at: string; started_at?: string; finished_at?: string; revision: number }
export interface Device { id: string; name: string; kind: string; status: string; timezone: string; revision: number }
export interface WorkSession { id: string; task_id: string; daily_plan_item_id?: string; status: string; target_minutes: number; duration_seconds: number; started_at: string; revision: number }
export interface Conversation { id: string; title: string; status: string; revision: number }
export interface Message { id: string; role: string; content: string; created_at: string }
export interface Proposal { id: string; goal_id: string; job_id: string; status: string; instruction: string; patch_json: string; base_revision: number; revision: number }
export interface TaskTree { goal_id: string; revision: number; nodes: unknown[]; proposals: Proposal[] }
export interface SessionView { id: string; family_id: string; user_id: string; status: string; expires_at: string; created_at: string; updated_at: string }
export interface ModelProvider { id: string; name: string; provider_type: string; base_url: string; model_name: string; transcription_model: string; api_key_hint: string; status: string; is_default: boolean; revision: number; created_at: string; updated_at: string }
export interface AuditView { id: string; actor_user_id: string; target_user_id?: string; action: string; detail_json: string; created_at: string; actor_identifier: string; actor_display_name: string; target_identifier?: string; target_display_name?: string }
export interface LensAxis { key: string; label: string; min: number; max: number; low_label: string; high_label: string }
export interface LensQuadrant { key: string; label: string; advice: string }
export interface Lens { id: string; title: string; threshold: number; x: LensAxis; y: LensAxis; quadrants: LensQuadrant[] }
export interface TaskCoord { id: string; task_id: string; lens: string; x: number; y: number; source: string; pinned: boolean; rationale: string; revision: number }
export interface ReviewQuadrant { key: string; label: string; minutes: number; share: number; previous_minutes: number; delta_minutes: number }
export interface ReviewFocus { total_minutes: number; unplotted_minutes: number; quadrants: ReviewQuadrant[] }
export interface ReviewEvidence { result: number; step: number; time: number; minimum_action: number; total: number; minimum_action_share: number }
export interface ReviewSummary { source: string; rule: string; text: string }
export interface StalledTask { task_id: string; title: string; goal_id: string; quadrant: string; days_since_progress: number }
export interface WeeklyReview { week: string; timezone: string; start_date: string; end_date: string; focus: ReviewFocus; stalled: StalledTask[]; evidence: ReviewEvidence; summary: ReviewSummary; llm_note: string }
export interface GoalMapNode { task_id: string; title: string; type: string; status: string; revision: number; x: number; y: number; quadrant: string; source: string; pinned: boolean; rationale: string; coord_id: string; coord_revision: number; focus_minutes: number; days_since_progress: number }
export interface GoalMap { goal_id: string; lens: string; threshold: number; target_date: string; unplotted: number; nodes: GoalMapNode[] }
