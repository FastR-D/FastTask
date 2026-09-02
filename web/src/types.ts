export interface User { id: string; identifier: string; display_name: string; timezone: string; locale: string; role: string; status: string; revision: number; created_at: string }
export interface Goal { id: string; title: string; description: string; success_criteria: string; status: string; target_date?: string; revision: number }
export interface Task { id: string; goal_id: string; parent_id?: string; type: string; title: string; description: string; status: string; priority: number; estimate_minutes: number; success_criteria: string; minimum_action: string; revision: number }
export interface Plan { id: string; local_date: string; timezone: string; status: string; revision: number; current_revision: number }
export interface PlanItem { id: string; daily_plan_id: string; task_id?: string; kind: string; title: string; commitment: string; minimum_action: string; status: string; target_minutes: number; revision: number }
export interface Job { id: string; type: string; status: string; output_json?: string; error_code?: string; error_message?: string; revision: number }
export interface Device { id: string; name: string; kind: string; status: string; timezone: string; revision: number }
export interface WorkSession { id: string; task_id: string; daily_plan_item_id?: string; status: string; target_minutes: number; duration_seconds: number; started_at: string; revision: number }
export interface Conversation { id: string; title: string; status: string; revision: number }
export interface Message { id: string; role: string; content: string; created_at: string }
export interface Proposal { id: string; goal_id: string; job_id: string; status: string; instruction: string; patch_json: string; base_revision: number; revision: number }
export interface TaskTree { goal_id: string; revision: number; nodes: unknown[]; proposals: Proposal[] }
export interface SessionView { id: string; family_id: string; user_id: string; status: string; expires_at: string; created_at: string; updated_at: string }
export interface ModelProvider { id: string; name: string; provider_type: string; base_url: string; model_name: string; transcription_model: string; api_key_hint: string; status: string; is_default: boolean; revision: number; created_at: string; updated_at: string }
export interface AuditView { id: string; actor_user_id: string; target_user_id?: string; action: string; detail_json: string; created_at: string; actor_identifier: string; actor_display_name: string; target_identifier?: string; target_display_name?: string }
