import { PointerEvent as ReactPointerEvent, useEffect, useRef, useState } from 'react'
import './lens.css'
import { request } from './api'
import type { Goal, GoalMap, GoalMapNode, Lens, TaskCoord, WeeklyReview } from './types'

function pad(value: number) { return String(value).padStart(2, '0') }

function addDays(date: Date, days: number) {
  const next = new Date(date.getTime())
  next.setUTCDate(next.getUTCDate() + days)
  return next
}

// isoWeekOf 返回 UTC 日期所在的 ISO 周标识，格式与服务端一致（2026-W37）。
export function isoWeekOf(date: Date): string {
  const day = new Date(Date.UTC(date.getUTCFullYear(), date.getUTCMonth(), date.getUTCDate()))
  const weekday = day.getUTCDay() || 7
  day.setUTCDate(day.getUTCDate() + 4 - weekday)
  const yearStart = new Date(Date.UTC(day.getUTCFullYear(), 0, 1))
  const week = Math.ceil(((day.getTime() - yearStart.getTime()) / 86400000 + 1) / 7)
  return `${day.getUTCFullYear()}-W${pad(week)}`
}

// mondayOf 把 "2026-W37" 还原成该 ISO 周周一的 UTC 日期。
export function mondayOf(week: string): Date {
  const [yearText, weekText] = week.split('-W')
  const year = Number(yearText)
  const weekNumber = Number(weekText)
  const januaryFourth = new Date(Date.UTC(year, 0, 4))
  const weekday = januaryFourth.getUTCDay() || 7
  return addDays(januaryFourth, -(weekday - 1) + (weekNumber - 1) * 7)
}

export function shiftWeek(week: string, delta: number): string {
  return isoWeekOf(addDays(mondayOf(week), delta * 7))
}

export function formatMinutes(minutes: number): string {
  if (!minutes) return '0 分钟'
  if (minutes < 60) return `${minutes} 分钟`
  return `${(minutes / 60).toFixed(1)} 小时`
}

export function nodeRadius(focusMinutes: number): number {
  return 1.6 + Math.min(focusMinutes, 600) / 150
}

export function nodeOpacity(daysSinceProgress: number): number {
  return Math.max(0.35, 1 - daysSinceProgress / 60)
}

function clampCoord(value: number): number {
  if (Number.isNaN(value)) return 50
  return Math.min(100, Math.max(0, value))
}

function etag(kind: string, id: string, revision: number) { return `"${kind}_${id}_rev_${revision}"` }

function errorText(error: unknown) { return error instanceof Error ? error.message : '操作失败' }

export function Review({ onNotice }: { onNotice: (value: string) => void }) {
  const [review, setReview] = useState<WeeklyReview | null>(null)
  const [week, setWeek] = useState('')
  const [currentWeek, setCurrentWeek] = useState('')
  const [loading, setLoading] = useState(true)

  async function load(target: string) {
    setLoading(true)
    try {
      const query = target ? `?week=${encodeURIComponent(target)}` : ''
      const { data } = await request<WeeklyReview>('/reviews/weekly' + query)
      setReview(data)
      setWeek(data.week)
      if (!target) setCurrentWeek(data.week)
    } catch (e) { onNotice(errorText(e)) } finally { setLoading(false) }
  }

  useEffect(() => { load('') }, [])

  const atLatestWeek = !currentWeek || week >= currentWeek
  const empty = Boolean(review && review.summary.rule === 'empty')
  const widest = review ? Math.max(1, ...review.focus.quadrants.map(q => q.minutes)) : 1

  return (
    <section className="review-page">
      <header className="page-head compact">
        <div>
          <p className="eyebrow">WEEKLY REFLECT</p>
          <h1>把你的判断，<br />和<em>真实投入</em>叠在一起。</h1>
        </div>
        <div className="week-switch">
          <mdui-button variant="outlined" icon="arrow_back" onClick={() => load(shiftWeek(week, -1))} disabled={loading || !week}>上一周</mdui-button>
          <b>{review ? `${review.week}` : '加载中'}</b>
          <mdui-button variant="outlined" end-icon="arrow_forward" onClick={() => load(shiftWeek(week, 1))} disabled={loading || !week || atLatestWeek}>下一周</mdui-button>
        </div>
      </header>

      {loading && <div className="loading-block"><mdui-linear-progress /><p className="empty">正在聚合本周的专注时间与推进证据…</p></div>}

      {!loading && !review && (
        <div className="empty-state">
          <span>∴</span>
          <h2 className="ts-headline-small">复盘暂时不可用</h2>
          <p>请稍后重试。复盘只读取已有记录，不会修改任何数据。</p>
        </div>
      )}

      {!loading && review && (
        <>
          <p className="meta review-window">{review.start_date} → {review.end_date} · {review.timezone}</p>

          {empty ? (
            <div className="empty-state">
              <span>∅</span>
              <h2 className="ts-headline-small">本周没有记录到有效专注时间</h2>
              <p>{review.summary.text}完成一次专注或记录一条推进证据后，这里会出现四区分布与停滞提醒。</p>
            </div>
          ) : (
            <>
              <mdui-card variant="elevated" className="summary-card">
                <p className="eyebrow">SUMMARY · {review.summary.rule} · {review.summary.source}</p>
                <h2>{review.summary.text}</h2>
                {review.llm_note && <p className="llm-note">{review.llm_note}</p>}
              </mdui-card>

              <section className="focus-panel">
                <h3>四区投入分布</h3>
                <p className="meta">有效专注 {formatMinutes(review.focus.total_minutes)} · 未标注坐标 {formatMinutes(review.focus.unplotted_minutes)}</p>
                {review.focus.quadrants.map(quadrant => (
                  <div className="quadrant-row" key={quadrant.key}>
                    <b>{quadrant.key}</b>
                    <div className="quadrant-text">
                      <span>{quadrant.label}</span>
                      <small>{(quadrant.share * 100).toFixed(1)}% · 上周 {quadrant.previous_minutes} 分钟 · 环比 {quadrant.delta_minutes >= 0 ? '+' : ''}{quadrant.delta_minutes}</small>
                    </div>
                    <mdui-linear-progress className="quadrant-bar" value={quadrant.minutes / widest} />
                    <strong>{quadrant.minutes}</strong>
                  </div>
                ))}
              </section>

              <section className="stalled-panel">
                <h3>关键风险区停滞任务</h3>
                {review.stalled.length === 0
                  ? <p className="empty">没有 A 区任务超过 14 天缺少实质推进。</p>
                  : <ul>{review.stalled.map(item => (
                    <li key={item.task_id}>
                      <b>{item.title}</b>
                      <span>{item.days_since_progress} 天没有 result / step 推进</span>
                    </li>
                  ))}</ul>}
              </section>

              <section className="evidence-panel">
                <h3>证据构成</h3>
                <div className="evidence-grid">
                  <div><b>{review.evidence.result}</b><span>result 可验证结果</span></div>
                  <div><b>{review.evidence.step}</b><span>step 实质步骤</span></div>
                  <div><b>{review.evidence.time}</b><span>time 时间投入</span></div>
                  <div><b>{review.evidence.minimum_action}</b><span>minimum_action 最小行动</span></div>
                </div>
                <p className="meta">已满足核心项 {review.evidence.total} 个 · 最小行动依赖度 {(review.evidence.minimum_action_share * 100).toFixed(0)}%</p>
              </section>
            </>
          )}
        </>
      )}
    </section>
  )
}

export function GoalMapView({ goal, onNotice, onOpenTask }: { goal: Goal; onNotice: (value: string) => void; onOpenTask?: (taskID: string) => void }) {
  const [lens, setLens] = useState<Lens | null>(null)
  const [map, setMap] = useState<GoalMap | null>(null)
  const [nodes, setNodes] = useState<GoalMapNode[]>([])
  const [loading, setLoading] = useState(true)
  const svg = useRef<SVGSVGElement | null>(null)
  const drag = useRef<{ id: string; moved: boolean; start: { x: number; y: number } } | null>(null)

  async function load() {
    setLoading(true)
    try {
      const [lenses, data] = await Promise.all([
        request<{ items: Lens[] }>('/lenses'),
        request<GoalMap>(`/goals/${goal.id}/map`),
      ])
      setLens(lenses.data.items[0] || null)
      setMap(data.data)
      setNodes(data.data.nodes)
    } catch (e) { onNotice(errorText(e)) } finally { setLoading(false) }
  }

  useEffect(() => { load() }, [goal.id])

  function toCoords(event: ReactPointerEvent<SVGSVGElement>) {
    const element = svg.current
    if (!element) return null
    const rect = element.getBoundingClientRect()
    if (!rect.width || !rect.height) return null
    const x = clampCoord(Math.round(((event.clientX - rect.left) / rect.width) * 100))
    const svgY = clampCoord(Math.round(((event.clientY - rect.top) / rect.height) * 100))
    return { x, y: 100 - svgY }
  }

  function onDown(event: ReactPointerEvent<SVGGElement>, node: GoalMapNode) {
    event.preventDefault()
    drag.current = { id: node.task_id, moved: false, start: { x: node.x, y: node.y } }
  }

  function onMove(event: ReactPointerEvent<SVGSVGElement>) {
    const active = drag.current
    if (!active) return
    const point = toCoords(event)
    if (!point) return
    if (point.x !== active.start.x || point.y !== active.start.y) active.moved = true
    setNodes(current => current.map(node => node.task_id === active.id ? { ...node, x: point.x, y: point.y } : node))
  }

  async function onUp() {
    const active = drag.current
    drag.current = null
    if (!active) return
    const node = nodes.find(item => item.task_id === active.id)
    if (!node) return
    if (!active.moved) { onOpenTask?.(node.task_id); return }
    try {
      const headers: Record<string, string> = {}
      if (node.coord_id) headers['If-Match'] = etag('coord', node.coord_id, node.coord_revision)
      const { data } = await request<TaskCoord>(`/tasks/${node.task_id}/coords`, { method: 'PUT', headers, body: JSON.stringify({ x: node.x, y: node.y }) })
      setNodes(current => current.map(item => item.task_id === node.task_id
        ? { ...item, coord_id: data.id, coord_revision: data.revision, source: data.source, pinned: data.pinned, rationale: data.rationale }
        : item))
      onNotice(`已保存「${node.title}」的坐标（${node.x}, ${node.y}）`)
    } catch (e) {
      setNodes(current => current.map(item => item.task_id === node.task_id ? { ...item, x: active.start.x, y: active.start.y } : item))
      onNotice(errorText(e))
    }
  }

  const threshold = map?.threshold ?? 50
  const size = 100 - threshold
  const dueSoon = Boolean(goal.target_date) && (Date.parse(`${goal.target_date}T23:59:59Z`) - Date.now()) / 86400000 <= 14

  if (loading) return <div className="loading-block"><mdui-linear-progress /><p className="empty">正在绘制目标地图…</p></div>
  if (!map) return <div className="empty-state"><h2 className="ts-headline-small">地图不可用</h2><p>无法读取该目标的坐标数据。</p></div>

  return (
    <mdui-card variant="outlined" className="lens-map">
      <div className="lens-map-head">
        <div>
          <p className="eyebrow">{lens ? `${lens.id} · ${lens.title}` : 'DECISION MAP'}</p>
          <p className="meta">叶子任务 {map.nodes.length} 个 · 未标注 {map.unplotted} 个{map.target_date ? ` · 目标日期 ${map.target_date}` : ''}</p>
        </div>
        <p className="meta hint">拖动节点修改坐标，点击节点查看任务详情。</p>
      </div>

      <svg
        ref={svg}
        className="lens-map-svg"
        viewBox="0 0 100 100"
        role="img"
        aria-label={`${goal.title} 决策地图`}
        onPointerMove={onMove}
        onPointerUp={onUp}
        onPointerCancel={onUp}
      >
        <rect className="quadrant-fill quadrant-B" x={0} y={0} width={threshold} height={threshold} />
        <rect className="quadrant-fill quadrant-A" x={threshold} y={0} width={size} height={threshold} />
        <rect className="quadrant-fill quadrant-D" x={0} y={threshold} width={threshold} height={size} />
        <rect className="quadrant-fill quadrant-C" x={threshold} y={threshold} width={size} height={size} />
        <line className="axis-line" x1={threshold} y1={0} x2={threshold} y2={100} />
        <line className="axis-line" x1={0} y1={threshold} x2={100} y2={threshold} />

        {lens && <>
          <text className="axis-label" x={2} y={98.5}>{lens.x.low_label}</text>
          <text className="axis-label" x={98} y={98.5} textAnchor="end">{lens.x.high_label}</text>
          <text className="axis-label" x={50} y={98.5} textAnchor="middle">{lens.x.label} →</text>
          <text className="axis-label" x={1.5} y={3} >{lens.y.high_label}</text>
          <text className="axis-label" x={1.5} y={threshold - 1.5}>{lens.y.label} ↑</text>
          <text className="axis-label" x={1.5} y={96}>{lens.y.low_label}</text>
          {lens.quadrants.map(quadrant => (
            <text
              className="quadrant-tag"
              key={quadrant.key}
              x={quadrant.key === 'A' || quadrant.key === 'C' ? 98 : 2}
              y={quadrant.key === 'A' || quadrant.key === 'B' ? 6 : 94}
              textAnchor={quadrant.key === 'A' || quadrant.key === 'C' ? 'end' : 'start'}
            >{quadrant.key} {quadrant.label}</text>
          ))}
        </>}

        {nodes.map(node => (
          <g
            key={node.task_id}
            className="map-node"
            transform={`translate(${node.x} ${100 - node.y})`}
            opacity={nodeOpacity(node.days_since_progress)}
            onPointerDown={event => onDown(event, node)}
          >
            <title>{`${node.title} · ${node.status} · 不确定性 ${node.x} / 贡献度 ${node.y} · 有效专注 ${node.focus_minutes} 分钟 · ${node.days_since_progress} 天未实质推进${node.rationale ? ' · ' + node.rationale : ''}`}</title>
            <circle
              r={nodeRadius(node.focus_minutes)}
              className={`node-dot status-${node.status}${node.source === 'default' ? ' hollow' : ''}${dueSoon && node.status !== 'completed' ? ' urgent' : ''}`}
            />
          </g>
        ))}
      </svg>

      <div className="lens-legend">
        {lens?.quadrants.map(quadrant => (
          <p key={quadrant.key}><b>{quadrant.key} {quadrant.label}</b><span>{quadrant.advice}</span></p>
        ))}
        <p className="legend-encoding"><b>编码</b><span>位置=判断，大小=累计有效专注分钟，空心=尚未标注，描边高亮=目标日期临近，越淡=越久没有实质推进。</span></p>
      </div>
    </mdui-card>
  )
}
