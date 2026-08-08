export interface User { id: string; display_name: string; timezone: string; locale: string; revision: number }
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
