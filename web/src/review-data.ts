export interface ReviewEntry {
  id: string; source: string; kind: string; title: string; summary: string; occurred_at?: string; source_updated_at?: string;
  task_id?: string; task_title?: string; goal_id?: string; goal_title?: string; status: string; url?: string; version?: string;
  duration_minutes?: number; attribution?: string; time_basis?: string; related_event_ids?: string[];
}
export interface ReviewActivity {
  week: string; timezone: string; start_date: string; end_date: string; items: ReviewEntry[]; total: number;
  has_more: boolean; next_offset: number; complete?: boolean; focus_minutes: number;
  counts: Record<string, number>; upcoming: {date:string;task_id?:string;title:string;goal_id?:string;target_minutes:number;status:string}[]; warnings: string[];
}
export function taskReviewEntry(item: ReviewEntry) {
  return item.source === 'fasttask' && ['result', 'step', 'minimum_action', 'time'].includes(item.kind) && item.time_basis !== 'imported_at'
}
const INVALID_STATUSES = new Set(['retracted', 'reopened', 'failed', 'error', 'rolled_back', 'rolled-back', 'rejected', 'deleted'])
export function validReviewEntry(item: ReviewEntry) { return !INVALID_STATUSES.has(item.status) }
export function entryGroup(item: ReviewEntry): 'result'|'step'|'time'|'material' {
  if (item.kind === 'time' || item.kind === 'activity') return 'time'
  if (['material', 'summary'].includes(item.kind)) return 'material'
  if (['step', 'minimum_action', 'annotation', 'note'].includes(item.kind)) return 'step'
  return 'result'
}
export function entryKind(item: ReviewEntry) {
  return ({result:'任务成果',step:'阶段进展',minimum_action:'小步推进',time:'专注投入'} as Record<string,string>)[item.kind] || '任务记录'
}
export function entryState(item: ReviewEntry) {
  return ({retracted:'已撤销',reopened:'已重新打开',stale:'对应旧版本',partial:'部分完成',partially_accepted:'部分采纳',accepted:'已采纳',generated:'已生成',completed:'已确认完成',checkpoint:'版本记录',saved:'已保存',time_unknown:'时间未记录'} as Record<string,string>)[item.status] || ''
}
export function reviewStamp(value: string|undefined, timezone: string) {
  if (!value || !Number.isFinite(Date.parse(value))) return '时间未记录'
  return new Intl.DateTimeFormat('zh-CN', {timeZone:timezone,year:'numeric',month:'2-digit',day:'2-digit',hour:'2-digit',minute:'2-digit',hour12:false}).format(new Date(value))
}
export function mergeReviewEntries(current: ReviewEntry[], added: ReviewEntry[]) {
  const indexed = new Map(current.map(item => [`${item.source}:${item.id}`, item]))
  added.forEach(item => indexed.set(`${item.source}:${item.id}`, item))
  return [...indexed.values()].sort((a,b) => (Date.parse(b.occurred_at || '') || 0) - (Date.parse(a.occurred_at || '') || 0) || a.id.localeCompare(b.id))
}
export function reportFromRecords(activity: ReviewActivity) {
  const local = mergeReviewEntries([], activity.items).filter(item => taskReviewEntry(item) && validReviewEntry(item))
  const lines = [`# FastTask 任务周报 · ${activity.week}`, `${activity.start_date} — ${activity.end_date}（${activity.timezone}）`, '', '## 本周确认成果']
  const summarize = (items:ReviewEntry[], empty:string) => items.length ? items.map(item => `- ${item.title}${item.summary ? `：${item.summary.replace(/\s+/g,' ').trim()}` : ''}${item.goal_title ? `（${item.goal_title}）` : ''}`) : [`- ${empty}`]
  lines.push(...summarize(local.filter(item => entryGroup(item)==='result'), '暂无已记录的最终成果。'), '', '## 阶段进展')
  lines.push(...summarize(local.filter(item => entryGroup(item)==='step'), '暂无已记录的阶段进展。'))
  lines.push('', '## 实际投入', `- FastTask 已记录有效专注：${activity.focus_minutes} 分钟。`, '', '## 待解决问题', '- 待补充：记录当前实际遇到的阻碍。', '', '## 下周已有安排')
  lines.push(...(activity.upcoming.length ? activity.upcoming.map(item => `- ${item.date} · ${item.title}（计划 ${item.target_minutes} 分钟）`) : ['- 暂无已保存的下周安排。']))
  if (activity.has_more || activity.complete===false || activity.warnings.length) {
    lines.push('', '## 数据提示')
    if (activity.has_more || activity.complete===false) lines.push('- 部分任务记录尚未载入，请载入后重新生成。')
    for (const warning of new Set(activity.warnings)) lines.push(`- ${warning}`)
  }
  return lines.join('\n')
}
