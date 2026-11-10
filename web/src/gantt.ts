import type { Task, TaskTreeNode } from './types'

export interface DateRange { start: string; end: string }
export interface GanttRow {
  task: Task
  depth: number
  ancestors: string[]
  hasChildren: boolean
  range?: DateRange
  source: 'manual' | 'derived' | 'unscheduled'
  unscheduledChildren: number
  prerequisites: string[]
  warnings: string[]
}

const DAY_MS = 86400000

export function calendarDay(value: string): number | undefined {
  if (!/^\d{4}-\d{2}-\d{2}$/.test(value) || value < '0001-01-01') return
  const time = Date.parse(`${value}T00:00:00Z`)
  if (Number.isFinite(time) && new Date(time).toISOString().slice(0, 10) === value) return time / DAY_MS
}

export function plannedDateError(start: string, end: string): string {
  if (!start && !end) return ''
  if (!start || !end) return '请同时填写计划开始、结束日期，或同时清空。'
  if (calendarDay(start) === undefined || calendarDay(end) === undefined) return '请输入有效的计划日期。'
  return end < start ? '计划结束日期不能早于开始日期。' : ''
}

export function taskRange(task: Task): DateRange | undefined {
  if (task.planned_start && task.planned_end && !plannedDateError(task.planned_start, task.planned_end)) {
    return { start: task.planned_start, end: task.planned_end }
  }
}

// Compute display ranges without writing derived values back to a task. A
// manually bounded child stays bounded even if its descendants exceed it.
export function buildGanttRows(nodes: TaskTreeNode[], targetDate?: string): GanttRow[] {
  const rows: GanttRow[] = []
  const seen = new Set<string>()
  const visit = (node: TaskTreeNode, ancestors: string[]): GanttRow | undefined => {
    if (node.task.deleted_at || seen.has(node.task.id)) return
    seen.add(node.task.id)
    const own = taskRange(node.task)
    const row: GanttRow = { task: node.task, depth: ancestors.length, ancestors, hasChildren: false, range: own,
      source: own ? 'manual' : 'unscheduled', unscheduledChildren: 0, prerequisites: [], warnings: [] }
    rows.push(row)
    const children = node.children.map(child => visit(child, [...ancestors, node.task.id])).filter((child): child is GanttRow => Boolean(child))
    row.hasChildren = children.length > 0
    row.unscheduledChildren = children.reduce((sum, child) => sum + child.unscheduledChildren + (child.range ? 0 : 1), 0)
    if (!own) {
      const ranges = children.flatMap(child => child.range ? [child.range] : [])
      if (ranges.length) {
        row.range = { start: ranges.reduce((start, range) => range.start < start ? range.start : start, ranges[0].start),
          end: ranges.reduce((end, range) => range.end > end ? range.end : end, ranges[0].end) }
        row.source = 'derived'
      }
    }
    return row
  }
  nodes.forEach(node => visit(node, []))
  const byId = new Map(rows.map(row => [row.task.id, row]))
  for (const row of rows) {
    row.prerequisites = [...new Set([...row.ancestors, row.task.id].flatMap(id => byId.get(id)?.task.depends_on ?? []))]
    if (!row.range) continue
    if (targetDate && calendarDay(targetDate) !== undefined && row.range.end > targetDate) row.warnings.push('超过目标日期')
    for (const id of row.ancestors) {
      const parent = byId.get(id)!
      if (parent.source === 'manual' && parent.range && (row.range.start < parent.range.start || row.range.end > parent.range.end)) {
        row.warnings.push(`超出「${parent.task.title}」的计划范围`)
      }
    }
    for (const id of row.prerequisites) {
      const predecessor = byId.get(id)
      if (predecessor?.range && row.range.start < predecessor.range.end) row.warnings.push(`计划开始早于前置「${predecessor.task.title}」结束`)
    }
  }
  return rows
}

export function visibleGanttRows(rows: GanttRow[], collapsed: ReadonlySet<string>): GanttRow[] {
  return rows.filter(row => !row.ancestors.some(id => collapsed.has(id)))
}

export function monthRange(month: string): DateRange {
  const start = `${month}-01`
  if (calendarDay(start) === undefined) throw new Error('无效月份')
  const lastDay = new Date(`${start}T00:00:00Z`)
  lastDay.setUTCMonth(lastDay.getUTCMonth() + 1)
  lastDay.setUTCDate(0)
  return { start, end: lastDay.toISOString().slice(0, 10) }
}

export function shiftMonth(month: string, delta: number): string {
  const value = new Date(`${month}-01T00:00:00Z`)
  value.setUTCMonth(value.getUTCMonth() + delta)
  const next = value.toISOString().slice(0, 7)
  return /^\d{4}-\d{2}$/.test(next) && next >= '0001-01' ? next : month
}

// Inclusive ends give a same-day task one complete calendar column. Ranges
// outside the month remain in the task list without a misleading clipped bar.
export function clipRange(range: DateRange | undefined, window: DateRange): { offset: number; days: number; before: boolean; after: boolean } | undefined {
  if (!range || range.end < window.start || range.start > window.end) return
  const start = range.start < window.start ? window.start : range.start
  const end = range.end > window.end ? window.end : range.end
  return { offset: calendarDay(start)! - calendarDay(window.start)!, days: calendarDay(end)! - calendarDay(start)! + 1,
    before: range.start < window.start, after: range.end > window.end }
}
