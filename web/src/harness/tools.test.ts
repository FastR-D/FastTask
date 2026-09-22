import { describe, expect, it, vi } from 'vitest'
import { buildHostTools, errorResult, manifestNames, TOOL_LIMIT, TOOL_WARN_AT, type ToolDef } from './tools'

// The tool projection on the host side (doc/harness.md §3.4, §4.4.1).
//
// Two things are under test: that a host invents nothing — every tool, description and schema comes
// from the server's manifest — and that same-name calls are serialized, because libfx does not hand a
// tool its call id and the server correlates a result by name.

const manifest: ToolDef[] = [
  { name: 'list_goals', description: '列出目标', input_schema: { type: 'object', properties: {} } },
  { name: 'get_task_tree', description: '读取任务树', input_schema: { type: 'object', properties: { goal_id: { type: 'string' } }, required: ['goal_id'] } },
]

describe('buildHostTools', () => {
  it('projects the manifest one to one, in server order', () => {
    const tools = buildHostTools(manifest, async () => ({ ok: true, value: {} }))
    expect(manifestNames(manifest)).toEqual(['list_goals', 'get_task_tree'])
    expect(tools.map(tool => tool.name)).toEqual(['list_goals', 'get_task_tree'])
    expect(tools[0].description).toBe('列出目标')
    expect(tools[1].inputSchema).toEqual(manifest[1].input_schema)
    // A host must not add a default schema of its own: the server's is what it validates against.
    expect(tools[0].inputSchema).toBe(manifest[0].input_schema)
  })

  it('passes the model input through untouched and returns the server value', async () => {
    const execute = vi.fn(async (_name: string, input: unknown) => ({ ok: true as const, value: { goals: ['Finish paper'], echoed: input } }))
    const tools = buildHostTools(manifest, execute)
    const result = await tools[1].execute({ goal_id: 'goal_1' }, { signal: new AbortController().signal })
    expect(execute).toHaveBeenCalledWith('get_task_tree', { goal_id: 'goal_1' }, expect.any(AbortSignal))
    expect(result).toEqual({ goals: ['Finish paper'], echoed: { goal_id: 'goal_1' } })
  })

  it('turns a failed call into the typed error libfx needs (§5 item 6)', async () => {
    const tools = buildHostTools(manifest, async () => ({ ok: false as const, message: 'invalid arguments: missing required argument "goal_id"' }))
    const result = (await tools[1].execute({}, { signal: new AbortController().signal })) as ReturnType<typeof errorResult>
    expect(result.type).toBe('libfx.tool-result')
    expect(result.isError).toBe(true)
    expect(result.text).toContain('goal_id')
    expect(result.images).toEqual([])
  })

  it('turns a thrown call into the same typed error rather than a crashed turn', async () => {
    const tools = buildHostTools(manifest, async () => {
      throw new Error('the tool surface is unreachable')
    })
    const result = (await tools[0].execute({}, { signal: new AbortController().signal })) as ReturnType<typeof errorResult>
    expect(result.isError).toBe(true)
    expect(result.text).toContain('unreachable')
  })

  it('reports a cancelled call without calling the server', async () => {
    const execute = vi.fn(async () => ({ ok: true as const, value: {} }))
    const tools = buildHostTools(manifest, execute)
    const controller = new AbortController()
    controller.abort()
    const result = (await tools[0].execute({}, { signal: controller.signal })) as ReturnType<typeof errorResult>
    expect(result.isError).toBe(true)
    expect(execute).not.toHaveBeenCalled()
  })

  it('serializes calls of the same name and leaves different names alone (§4.4.1)', async () => {
    // The server correlates a result by (run, tool name, newest unfinished part). That is only
    // unambiguous if two calls of the same name never overlap, which libfx does not guarantee.
    const order: string[] = []
    const execute = vi.fn(async (name: string) => {
      order.push(`start:${name}`)
      await new Promise(resolve => setTimeout(resolve, name === 'list_goals' ? 20 : 1))
      order.push(`end:${name}`)
      return { ok: true as const, value: { name } }
    })
    const tools = buildHostTools(manifest, execute)
    const signal = new AbortController().signal
    await Promise.all([
      tools[0].execute({ first: true }, { signal }),
      tools[0].execute({ second: true }, { signal }),
      tools[1].execute({ goal_id: 'g' }, { signal }),
    ])
    // No two calls of the same name were ever in flight together.
    let open = 0
    let maxOpen = 0
    for (const entry of order) {
      if (entry === 'start:list_goals') {
        open += 1
        maxOpen = Math.max(maxOpen, open)
      }
      if (entry === 'end:list_goals') open -= 1
    }
    expect(maxOpen).toBe(1)
    expect(order.filter(entry => entry === 'start:list_goals')).toHaveLength(2)
    expect(order.filter(entry => entry === 'end:list_goals')).toHaveLength(2)
    // A different tool is not held up by the queue.
    expect(order).toContain('end:get_task_tree')
  })

  it('refuses a manifest over libfx’s cap and warns before it', () => {
    const many = (count: number): ToolDef[] =>
      Array.from({ length: count }, (_, index) => ({
        name: `tool_${index}`,
        description: 'd',
        input_schema: { type: 'object', properties: {} },
      }))
    const onWarn = vi.fn()
    expect(() => buildHostTools(many(TOOL_LIMIT + 1), async () => ({ ok: true, value: {} }))).toThrow(RangeError)
    buildHostTools(many(TOOL_WARN_AT + 1), async () => ({ ok: true, value: {} }), { onWarn })
    expect(onWarn).toHaveBeenCalledWith(expect.stringContaining(String(TOOL_WARN_AT + 1)))
    expect(TOOL_LIMIT).toBe(64)
  })

  it('rejects a manifest that is not an array', () => {
    expect(() => buildHostTools(null as unknown as ToolDef[], async () => ({ ok: true, value: {} }))).toThrow(TypeError)
  })
})
