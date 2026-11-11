import { useEffect, useId, useMemo, useState } from 'react'
import type { CSSProperties } from 'react'
import { localDateInTimezone } from './date'
import { buildGanttRows, calendarDay, clipRange, monthRange, shiftMonth, visibleGanttRows } from './gantt'
import type { Goal, Task, TaskTreeNode } from './types'
import './gantt.css'

const STATUS: Record<string, string> = { ready: '待开始', in_progress: '进行中', blocked: '受阻', paused: '已暂停', completed: '已完成', cancelled: '已取消', superseded: '已替换' }
const DAY_WIDTH = 36
const ROW_HEIGHT = 88
type Props = { goal: Goal; nodes: TaskTreeNode[]; timezone: string; busy: boolean; onEdit: (task: Task) => void; onAdd: (parent?: Task) => void }

export function GanttView({ goal, nodes, timezone, busy, onEdit, onAdd }: Props) {
  const [today, setToday] = useState(() => localDateInTimezone(new Date(), timezone))
  const [month, setMonth] = useState(() => today.slice(0, 7))
  const [collapsed, setCollapsed] = useState<Set<string>>(new Set())
  const [selectedId, setSelectedId] = useState('')
  const markerId = `gantt-arrow-${useId().replace(/[^a-zA-Z0-9_-]/g, '')}`
  useEffect(() => {
    const update = () => setToday(localDateInTimezone(new Date(), timezone))
    update()
    const timer = window.setInterval(update, 60000)
    return () => window.clearInterval(timer)
  }, [timezone])
  const rows = useMemo(() => buildGanttRows(nodes, goal.target_date), [nodes, goal.target_date])
  const visible = visibleGanttRows(rows, collapsed)
  const byId = new Map(rows.map(row => [row.task.id, row]))
  const windowRange = monthRange(month)
  const days = calendarDay(windowRange.end)! - calendarDay(windowRange.start)! + 1
  const width = days * DAY_WIDTH
  const todayOffset = calendarDay(today)! - calendarDay(windowRange.start)!
  const selected = byId.get(selectedId)
  const warningCount = rows.filter(row => row.warnings.length).length
  const index = new Map(visible.map((row, position) => [row.task.id, position]))
  const lines = visible.flatMap(row => row.prerequisites.flatMap(id => {
    const predecessor = byId.get(id), fromIndex = index.get(id), toIndex = index.get(row.task.id)!
    const from = clipRange(predecessor?.range, windowRange), to = clipRange(row.range, windowRange)
    if (fromIndex === undefined || !from || !to || id === row.task.id) return []
    const x1 = (from.offset + from.days) * DAY_WIDTH - 3, x2 = to.offset * DAY_WIDTH + 3
    const y1 = fromIndex * ROW_HEIGHT + 44, y2 = toIndex * ROW_HEIGHT + 44
    const bend = Math.min(width - 2, x1 + 12)
    return [{ key: `${id}-${row.task.id}`, path: `M ${x1} ${y1} H ${bend} V ${y2 - 23} H ${Math.max(2, x2 - 10)} V ${y2} H ${x2}` }]
  }))

  function toggle(id: string) {
    setCollapsed(current => { const next = new Set(current); if (next.has(id)) next.delete(id); else next.add(id); return next })
  }
  function edit(task: Task) { setSelectedId(task.id); onEdit(task) }

  return <section className="gantt-view" aria-label={`${goal.title}甘特图`}>
    <div className="gantt-toolbar">
      <div className="actions">
        <mdui-button-icon icon="chevron_left" aria-label="上个月" disabled={month === '0001-01'} onClick={() => setMonth(shiftMonth(month, -1))}/>
        <h3 aria-live="polite">{month.slice(0, 4)} 年 {Number(month.slice(5))} 月</h3>
        <mdui-button-icon icon="chevron_right" aria-label="下个月" disabled={month === '9999-12'} onClick={() => setMonth(shiftMonth(month, 1))}/>
        <mdui-button variant="text" onClick={() => setMonth(today.slice(0, 7))}>回到今天</mdui-button>
      </div>
      <div className="actions"><mdui-button variant="text" onClick={() => setCollapsed(new Set())}>全部展开</mdui-button><mdui-button variant="outlined" icon="add" disabled={busy} onClick={() => onAdd()}>添加阶段</mdui-button></div>
    </div>
    <p className="meta">先定阶段周期，再逐步安排任务。点击名称或时间条编辑。{goal.target_date ? `目标日期：${goal.target_date}。` : ''}</p>
    <p className="gantt-summary" role="status">{rows.length} 项 · {rows.filter(row => !row.range).length} 项未排期{warningCount ? ` · ${warningCount} 项日期提示` : ''}</p>
    {!rows.length ? <div className="empty-state"><h3>从一个阶段开始规划</h3><p>例如文献调研、实验验证或论文撰写，先填阶段周期，再补充子任务。</p></div> : <div className="gantt-scroll" role="region" aria-label="按月浏览甘特时间轴，可横向滚动" tabIndex={0}>
      <div className="gantt-table" role="table" aria-label="阶段与任务计划" style={{ width: `calc(var(--gantt-label-width) + ${width}px)`, '--gantt-timeline-width': `${width}px` } as CSSProperties}>
        <div className="gantt-header" role="row">
          <div className="gantt-label gantt-heading" role="columnheader">阶段与任务</div>
          <div className="gantt-days" role="columnheader" aria-label={`${windowRange.start} 至 ${windowRange.end}`}>
            {Array.from({ length: days }, (_, index) => {
              const date = `${month}-${String(index + 1).padStart(2, '0')}`, weekday = new Date(`${date}T00:00:00Z`).getUTCDay()
              return <div key={date} className={`gantt-day${weekday === 0 || weekday === 6 ? ' weekend' : ''}${date === today ? ' today' : ''}`}><b>{index + 1}</b><small>{'日一二三四五六'[weekday]}</small></div>
            })}
          </div>
        </div>
        <div className="gantt-body">
          {todayOffset >= 0 && todayOffset < days && <div className="gantt-today" aria-hidden="true" style={{ left: `calc(var(--gantt-label-width) + ${todayOffset * DAY_WIDTH + DAY_WIDTH / 2}px)` }}/>}
          <svg className="gantt-dependencies" width={width} height={visible.length * ROW_HEIGHT} aria-hidden="true">
            <defs><marker id={markerId} viewBox="0 0 10 10" refX="9" refY="5" markerWidth="5" markerHeight="5" orient="auto-start-reverse"><path d="M 0 0 L 10 5 L 0 10 z"/></marker></defs>
            {lines.map(line => <path key={line.key} d={line.path} markerEnd={`url(#${markerId})`}/>)}
          </svg>
          {visible.map(row => {
            const bar = clipRange(row.range, windowRange)
            const stage = row.task.type === 'milestone' || row.hasChildren
            const rangeText = row.range ? `${row.range.start} → ${row.range.end}` : '未排期'
            const description = [STATUS[row.task.status] || row.task.status, rangeText, row.source === 'derived' ? '自动汇总' : '', ...row.warnings].filter(Boolean).join('；')
            return <div key={row.task.id} className={`gantt-row${selectedId === row.task.id ? ' selected' : ''}`} role="row">
              <div className="gantt-label" role="cell">
                <div className="gantt-label-main" style={{ paddingLeft: `${Math.min(row.depth, 5) * 12}px` }}>
                  {row.hasChildren ? <button type="button" className="gantt-toggle" onClick={() => toggle(row.task.id)} aria-expanded={!collapsed.has(row.task.id)} aria-label={`${collapsed.has(row.task.id) ? '展开' : '收起'}${row.task.title}`}><mdui-icon name={collapsed.has(row.task.id) ? 'chevron_right' : 'expand_more'}/></button> : <span className="gantt-leaf"/>}
                  <button type="button" className="gantt-task-name" disabled={busy} title={`${row.task.title}；${description}`} onClick={() => edit(row.task)}><b>{row.task.title}</b><small>{stage ? '阶段' : row.task.type === 'action' ? '行动' : '任务'} · {STATUS[row.task.status] || row.task.status}</small></button>
                  {(row.prerequisites.length > 0 || row.warnings.length > 0) && <button type="button" className="gantt-info" onClick={() => setSelectedId(row.task.id)} aria-label={`查看${row.task.title}的依赖与日期提示`}><mdui-icon name={row.warnings.length ? 'warning_amber' : 'account_tree'}/></button>}
                </div>
                <small className="gantt-range">{rangeText}{row.source === 'derived' ? ' · 自动汇总' : ''}{row.unscheduledChildren ? ` · ${row.unscheduledChildren} 项子任务未排期` : ''}</small>
              </div>
              <div className="gantt-track" role="cell" aria-label={description}>
                {bar ? <button type="button" disabled={busy} className={`gantt-bar status-${row.task.status}${stage ? ' stage' : ''}${bar.before ? ' continues-before' : ''}${bar.after ? ' continues-after' : ''}`} style={{ left: bar.offset * DAY_WIDTH + 3, width: bar.days * DAY_WIDTH - 6 }} title={`${row.task.title}；${description}`} aria-label={`编辑${row.task.title}：${description}`} onClick={() => edit(row.task)}><span>{row.task.title}</span></button> : <button type="button" className="gantt-no-bar" disabled={busy} onClick={() => edit(row.task)}>{row.range ? (row.range.end < windowRange.start ? '← 计划在更早月份' : '计划在之后月份 →') : '填写计划日期'}</button>}
              </div>
            </div>
          })}
        </div>
      </div>
    </div>}
    {selected && <section className="gantt-detail" aria-label="任务依赖与日期提示">
      <div className="gantt-toolbar"><h3>{selected.task.title}</h3><div className="actions"><mdui-button variant="text" disabled={busy} onClick={() => edit(selected.task)}>编辑计划</mdui-button>{!['completed', 'cancelled', 'superseded'].includes(selected.task.status) && <mdui-button variant="text" icon="add" disabled={busy} onClick={() => onAdd(selected.task)}>添加子任务</mdui-button>}</div></div>
      <p className="meta">{selected.range ? `${selected.range.start} → ${selected.range.end}${selected.source === 'derived' ? ' · 自动汇总' : ''}` : '未排期'}</p>
      {selected.prerequisites.length > 0 && <div><b>前置成果（含上级继承）</b><ul>{selected.prerequisites.map(id => { const predecessor = byId.get(id); return <li key={id}>{predecessor ? `${predecessor.task.title} · ${STATUS[predecessor.task.status] || predecessor.task.status}` : '前置任务不可用'}</li> })}</ul></div>}
      {selected.warnings.length > 0 && <div className="gantt-warnings"><b>日期提示</b><ul>{selected.warnings.map(warning => <li key={warning}>{warning}</li>)}</ul></div>}
    </section>}
    <p className="meta">计划日期用于长期规划；每日安排按任务状态、真实前置成果和每日预算生成。</p>
  </section>
}
