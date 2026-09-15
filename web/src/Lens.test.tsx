import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { GoalMapView, Review, formatMinutes, isoWeekOf, mondayOf, nodeOpacity, nodeRadius, shiftWeek } from './Lens'
import type { Goal, GoalMap, Lens, TaskCoord, WeeklyReview } from './types'

type FetchCall = { url: string; init?: RequestInit }

function jsonResponse(body: unknown, status = 200, headers: Record<string, string> = {}) {
	return {
		ok: status >= 200 && status < 300,
		status,
		statusText: status === 200 ? 'OK' : 'ERROR',
		headers: { get: (key: string) => headers[key] ?? headers[key.toLowerCase()] ?? null },
		json: async () => body,
	} as unknown as Response
}

function installFetch(handler: (url: string, init?: RequestInit) => unknown) {
	const calls: FetchCall[] = []
	vi.stubGlobal('fetch', vi.fn(async (url: unknown, init?: RequestInit) => {
		const text = String(url)
		calls.push({ url: text, init })
		return jsonResponse(handler(text, init))
	}))
	return calls
}

const lensFixture: Lens = {
	id: 'research_risk', title: '科研风险透镜', threshold: 50,
	x: { key: 'uncertainty', label: '不确定性', min: 0, max: 100, low_label: '知道怎么做', high_label: '方法未知' },
	y: { key: 'contribution', label: '贡献度', min: 0, max: 100, low_label: '间接', high_label: '直接决定验收' },
	quadrants: [
		{ key: 'A', label: '关键风险区', advice: '尽早验证，拖延成本最高' },
		{ key: 'B', label: '主推进区', advice: '稳定产出' },
		{ key: 'C', label: '时间黑洞', advice: '降级、拆小或砍掉' },
		{ key: 'D', label: '消耗区', advice: '必要，但不应占主要时间' },
	],
}

const reviewFixture: WeeklyReview = {
	week: '2026-W37', timezone: 'Asia/Shanghai', start_date: '2026-09-07', end_date: '2026-09-13',
	focus: {
		total_minutes: 672, unplotted_minutes: 30, quadrants: [
			{ key: 'A', label: '关键风险区', minutes: 95, share: 0.141, previous_minutes: 135, delta_minutes: -40 },
			{ key: 'B', label: '主推进区', minutes: 180, share: 0.268, previous_minutes: 120, delta_minutes: 60 },
			{ key: 'C', label: '时间黑洞', minutes: 40, share: 0.06, previous_minutes: 40, delta_minutes: 0 },
			{ key: 'D', label: '消耗区', minutes: 357, share: 0.531, previous_minutes: 300, delta_minutes: 57 },
		],
	},
	stalled: [{ task_id: 'task_1', title: '跑通基线实验', goal_id: 'goal_1', quadrant: 'A', days_since_progress: 19 }],
	evidence: { result: 3, step: 5, time: 2, minimum_action: 8, total: 18, minimum_action_share: 0.444 },
	summary: { source: 'template', rule: 'stalled_risk', text: '你标为关键风险的「跑通基线实验」已 19 天没有实质推进。' },
	llm_note: '',
}

const mapFixture: GoalMap = {
	goal_id: 'goal_1', lens: 'research_risk', threshold: 50, target_date: '', unplotted: 1,
	nodes: [
		{ task_id: 'task_1', title: '跑通基线实验', type: 'task', status: 'in_progress', revision: 3, x: 70, y: 85, quadrant: 'A', source: 'agent', pinned: false, rationale: '方法未定', coord_id: 'coord_1', coord_revision: 2, focus_minutes: 125, days_since_progress: 19 },
		{ task_id: 'task_2', title: '整理参考文献', type: 'action', status: 'ready', revision: 1, x: 50, y: 50, quadrant: 'A', source: 'default', pinned: false, rationale: '', coord_id: '', coord_revision: 0, focus_minutes: 0, days_since_progress: 0 },
	],
}

function headerOf(call: FetchCall | undefined, key: string) {
	const headers = call?.init?.headers as Headers | undefined
	return headers ? headers.get(key) : null
}

const goalFixture: Goal = { id: 'goal_1', title: '完成论文初稿', description: '', success_criteria: '通过组内评审', status: 'active', revision: 1 }

beforeEach(() => {
	vi.spyOn(SVGSVGElement.prototype, 'getBoundingClientRect').mockReturnValue({
		width: 400, height: 400, top: 0, left: 0, right: 400, bottom: 400, x: 0, y: 0, toJSON: () => ({}),
	} as DOMRect)
})

afterEach(() => {
	cleanup()
	vi.unstubAllGlobals()
	vi.restoreAllMocks()
})

describe('ISO week helpers', () => {
	it('matches the server ISO week format', () => {
		expect(isoWeekOf(new Date(Date.UTC(2026, 8, 9, 12)))).toBe('2026-W37')
		expect(isoWeekOf(new Date(Date.UTC(2025, 11, 29)))).toBe('2026-W01')
		expect(mondayOf('2026-W01').toISOString().slice(0, 10)).toBe('2025-12-29')
		expect(mondayOf('2026-W37').toISOString().slice(0, 10)).toBe('2026-09-07')
	})

	it('shifts weeks across year boundaries', () => {
		expect(shiftWeek('2026-W37', -1)).toBe('2026-W36')
		expect(shiftWeek('2026-W37', 1)).toBe('2026-W38')
		expect(shiftWeek('2026-W01', -1)).toBe('2025-W52')
		expect(shiftWeek('2025-W52', 1)).toBe('2026-W01')
	})

	it('encodes nodes deterministically', () => {
		expect(nodeRadius(0)).toBeCloseTo(1.6)
		expect(nodeRadius(600)).toBeCloseTo(5.6)
		expect(nodeRadius(9000)).toBeCloseTo(5.6)
		expect(nodeOpacity(0)).toBe(1)
		expect(nodeOpacity(60)).toBeCloseTo(0.35)
		expect(nodeOpacity(600)).toBeCloseTo(0.35)
		expect(formatMinutes(0)).toBe('0 分钟')
		expect(formatMinutes(45)).toBe('45 分钟')
		expect(formatMinutes(90)).toBe('1.5 小时')
	})
})

describe('Review', () => {
	it('renders the deterministic weekly review', async () => {
		installFetch(() => reviewFixture)
		const onNotice = vi.fn()
		render(<Review onNotice={onNotice} />)

		expect(await screen.findByText('你标为关键风险的「跑通基线实验」已 19 天没有实质推进。')).toBeInTheDocument()
		expect(screen.getByText(/stalled_risk/)).toBeInTheDocument()
		expect(screen.getByText('2026-09-07 → 2026-09-13 · Asia/Shanghai')).toBeInTheDocument()
		for (const label of ['关键风险区', '主推进区', '时间黑洞', '消耗区']) {
			expect(screen.getByText(label, { selector: '.quadrant-text span' })).toBeInTheDocument()
		}
		expect(screen.getByText(/14.1%/)).toBeInTheDocument()
		expect(screen.getByText(/环比 \+60/)).toBeInTheDocument()
		expect(screen.getByText('有效专注 11.2 小时 · 未标注坐标 30 分钟')).toBeInTheDocument()
		expect(screen.getByText('19 天没有 result / step 推进')).toBeInTheDocument()
		expect(screen.getByText('result 可验证结果')).toBeInTheDocument()
		expect(screen.getByText('已满足核心项 18 个 · 最小行动依赖度 44%')).toBeInTheDocument()
		expect(onNotice).not.toHaveBeenCalled()
	})

	it('blocks navigation into future weeks and loads the previous week', async () => {
		const calls = installFetch(url => {
			const week = new URL(url, 'http://local').searchParams.get('week')
			return week ? { ...reviewFixture, week, summary: { source: 'template', rule: 'neutral', text: `${week} 的确定性总结。` } } : reviewFixture
		})
		render(<Review onNotice={vi.fn()} />)
		await screen.findByText(/stalled_risk/)

		const next = screen.getByRole('button', { name: '下一周' })
		expect(next).toBeDisabled()
		fireEvent.click(screen.getByRole('button', { name: '上一周' }))

		expect(await screen.findByText('2026-W36 的确定性总结。')).toBeInTheDocument()
		expect(calls.some(call => call.url.endsWith('/reviews/weekly?week=2026-W36'))).toBe(true)
		expect(screen.getByRole('button', { name: '下一周' })).toBeEnabled()
	})

	it('shows the empty state for a week without focus time', async () => {
		installFetch(() => ({
			...reviewFixture,
			focus: { total_minutes: 0, unplotted_minutes: 0, quadrants: reviewFixture.focus.quadrants.map(q => ({ ...q, minutes: 0, share: 0, previous_minutes: 0, delta_minutes: 0 })) },
			stalled: [], evidence: { result: 0, step: 0, time: 0, minimum_action: 0, total: 0, minimum_action_share: 0 },
			summary: { source: 'template', rule: 'empty', text: '本周没有记录到有效专注时间。' },
		}))
		render(<Review onNotice={vi.fn()} />)
		expect(await screen.findByRole('heading', { name: '本周没有记录到有效专注时间' })).toBeInTheDocument()
		expect(screen.queryByText(/stalled_risk/)).not.toBeInTheDocument()
	})

	it('reports load failures through onNotice', async () => {
		installFetch(() => { throw new Error('网络中断') })
		const onNotice = vi.fn()
		render(<Review onNotice={onNotice} />)
		await waitFor(() => expect(onNotice).toHaveBeenCalledWith('网络中断'))
		expect(await screen.findByRole('heading', { name: '复盘暂时不可用' })).toBeInTheDocument()
	})
})

describe('GoalMapView', () => {
	it('renders quadrants, axis labels and hollow unplotted nodes', async () => {
		installFetch(url => url.includes('/lenses') ? { items: [lensFixture] } : mapFixture)
		const { container } = render(<GoalMapView goal={goalFixture} onNotice={vi.fn()} />)
		expect(await screen.findByText('A 关键风险区', { selector: '.quadrant-tag' })).toBeInTheDocument()
		expect(screen.getByText('不确定性 →')).toBeInTheDocument()
		expect(screen.getByText('方法未知')).toBeInTheDocument()
		expect(screen.getByText('尽早验证，拖延成本最高')).toBeInTheDocument()
		expect(screen.getByText('叶子任务 2 个 · 未标注 1 个')).toBeInTheDocument()

		const nodes = container.querySelectorAll('.map-node')
		expect(nodes).toHaveLength(2)
		expect(nodes[0].getAttribute('transform')).toBe('translate(70 15)')
		expect(nodes[0].querySelector('circle')?.getAttribute('class')).toContain('status-in_progress')
		expect(nodes[0].querySelector('circle')?.getAttribute('class')).not.toContain('hollow')
		expect(nodes[1].querySelector('circle')?.getAttribute('class')).toContain('hollow')
		expect(nodes[1].getAttribute('transform')).toBe('translate(50 50)')
	})

	it('opens the task when the node is clicked without dragging', async () => {
		installFetch(url => url.includes('/lenses') ? { items: [lensFixture] } : mapFixture)
		const onOpenTask = vi.fn()
		render(<GoalMapView goal={goalFixture} onNotice={vi.fn()} onOpenTask={onOpenTask} />)
		const node = await screen.findByText(/跑通基线实验/, { selector: 'title' })
		const group = node.parentElement as unknown as Element
		fireEvent.pointerDown(group, { clientX: 280, clientY: 60 })
		fireEvent.pointerUp(document.querySelector('.lens-map-svg')!, { clientX: 280, clientY: 60 })
		expect(onOpenTask).toHaveBeenCalledWith('task_1')
	})

	it('persists dragged coordinates with If-Match', async () => {
		const saved: TaskCoord = { id: 'coord_1', task_id: 'task_1', lens: 'research_risk', x: 80, y: 75, source: 'user', pinned: true, rationale: '', revision: 3 }
		const calls = installFetch((url, init) => {
			if (url.includes('/lenses')) return { items: [lensFixture] }
			if (init?.method === 'PUT') return saved
			return mapFixture
		})
		const onNotice = vi.fn()
		render(<GoalMapView goal={goalFixture} onNotice={onNotice} />)
		const node = await screen.findByText(/跑通基线实验/, { selector: 'title' })
		const group = node.parentElement as unknown as Element
		const svg = document.querySelector('.lens-map-svg')!

		fireEvent.pointerDown(group, { clientX: 280, clientY: 60 })
		fireEvent.pointerMove(svg, { clientX: 320, clientY: 100 })
		fireEvent.pointerUp(svg, { clientX: 320, clientY: 100 })

		await waitFor(() => expect(onNotice).toHaveBeenCalled())
		const put = calls.find(call => call.init?.method === 'PUT')
		expect(put?.url).toBe('/api/v1/tasks/task_1/coords')
		expect(headerOf(put, 'If-Match')).toBe('"coord_coord_1_rev_2"')
		expect(JSON.parse(String(put?.init?.body))).toEqual({ x: 80, y: 75 })
		expect(onNotice.mock.calls[0][0]).toContain('已保存「跑通基线实验」的坐标（80, 75）')
		expect(group.getAttribute('transform')).toBe('translate(80 25)')
	})

	it('rolls the node back when the coordinate write fails', async () => {
		const calls = installFetch((url, init) => {
			if (url.includes('/lenses')) return { items: [lensFixture] }
			if (init?.method === 'PUT') throw new Error('revision mismatch')
			return mapFixture
		})
		const onNotice = vi.fn()
		render(<GoalMapView goal={goalFixture} onNotice={onNotice} />)
		const node = await screen.findByText(/整理参考文献/, { selector: 'title' })
		const group = node.parentElement as unknown as Element
		const svg = document.querySelector('.lens-map-svg')!

		fireEvent.pointerDown(group, { clientX: 200, clientY: 200 })
		fireEvent.pointerMove(svg, { clientX: 40, clientY: 320 })
		fireEvent.pointerUp(svg, { clientX: 40, clientY: 320 })

		await waitFor(() => expect(onNotice).toHaveBeenCalledWith('revision mismatch'))
		const put = calls.find(call => call.init?.method === 'PUT')
		expect(headerOf(put, 'If-Match')).toBeNull()
		expect(group.getAttribute('transform')).toBe('translate(50 50)')
	})
})
