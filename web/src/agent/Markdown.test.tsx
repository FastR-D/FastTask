import { cleanup, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it } from 'vitest'
import type { TextMessagePartProps } from '@assistant-ui/react'
import { MarkdownText } from './Markdown'

// vitest runs without `globals`, so testing-library's auto-cleanup never fires
// and every query below would match the previous test's tree too.
afterEach(cleanup)

// MarkdownText is the assistant bubble's Text part renderer. assistant-ui
// spreads the message part flat onto it (`jsx(Text, { ...part })`), so the props
// are `{ type, text, status }` and the surrounding MessagePartState is stubbed.
function textPart(text: string) {
  return { type: 'text', text, status: { type: 'complete' } } as unknown as TextMessagePartProps
}

function renderMarkdown(text: string) {
  return render(<MarkdownText {...textPart(text)} />)
}

describe('agent markdown rendering', () => {
  it('renders headings, emphasis and inline code as elements, not literal syntax', () => {
    renderMarkdown('## 三种排序算法\n\n**冒泡** 是 `O(n²)`，*快速* 更快。')
    expect(screen.getByRole('heading', { level: 2 })).toHaveTextContent('三种排序算法')
    expect(screen.getByText('冒泡').tagName).toBe('STRONG')
    expect(screen.getByText('快速').tagName).toBe('EM')
    expect(screen.getByText('O(n²)').tagName).toBe('CODE')
    expect(screen.queryByText(/##/)).not.toBeInTheDocument()
  })

  it('renders GFM tables, ordered lists and fenced code blocks', () => {
    renderMarkdown([
      '| 名称 | 复杂度 |',
      '| --- | --- |',
      '| 冒泡排序 | O(n²) |',
      '| 快速排序 | O(n log n) |',
      '',
      '1. 第一步',
      '2. 第二步',
      '',
      '```js',
      'const a = 1',
      '```',
    ].join('\n'))

    const table = screen.getByRole('table')
    expect(table.querySelectorAll('thead th')).toHaveLength(2)
    expect(table.querySelectorAll('tbody tr')).toHaveLength(2)
    expect(screen.getByRole('cell', { name: '冒泡排序' })).toBeInTheDocument()

    const list = screen.getByRole('list')
    expect(list.tagName).toBe('OL')
    expect(list.querySelectorAll('li')).toHaveLength(2)

    const pre = document.querySelector('.agent-md pre')
    expect(pre).not.toBeNull()
    expect(pre?.querySelector('code')).toHaveTextContent('const a = 1')
  })

  it('renders GFM task lists, strikethrough and autolinks', () => {
    renderMarkdown('- [x] 已完成\n- [ ] 未完成\n\n~~废弃~~ https://opencode.ai')
    expect(document.querySelectorAll('.agent-md input[type="checkbox"]')).toHaveLength(2)
    expect(screen.getByText('废弃').tagName).toBe('DEL')
    expect(screen.getByRole('link', { name: 'https://opencode.ai' })).toHaveAttribute('href', 'https://opencode.ai')
  })

  it('opens links in a new tab without opener access', () => {
    renderMarkdown('[文档](https://opencode.ai/docs)')
    const link = screen.getByRole('link', { name: '文档' })
    expect(link).toHaveAttribute('href', 'https://opencode.ai/docs')
    expect(link).toHaveAttribute('target', '_blank')
    expect(link).toHaveAttribute('rel', 'noopener noreferrer')
  })

  it('never renders raw HTML from model output (XSS)', () => {
    renderMarkdown('<img src=x onerror="window.__pwned=1"><script>window.__pwned=1</script>\n\n<b onclick="window.__pwned=1">粗体</b>')
    expect(document.querySelector('.agent-md img')).toBeNull()
    expect(document.querySelector('.agent-md script')).toBeNull()
    expect(document.querySelector('[onerror]')).toBeNull()
    expect(document.querySelector('[onclick]')).toBeNull()
    expect((window as unknown as { __pwned?: number }).__pwned).toBeUndefined()
  })

  it('keeps blockquotes, rules and nested lists as elements', () => {
    renderMarkdown('> 引用一行\n\n---\n\n- 外层\n  - 内层')
    expect(screen.getByText('引用一行').closest('blockquote')).not.toBeNull()
    expect(document.querySelector('.agent-md hr')).not.toBeNull()
    expect(document.querySelectorAll('.agent-md li')).toHaveLength(2)
  })

  it('tolerates the half-built text a streaming converter produces', () => {
    const { container, rerender } = render(<MarkdownText {...textPart('## 标题\n\n| a | b |\n| --- ')} />)
    expect(container.querySelector('.agent-md')).not.toBeNull()
    rerender(<MarkdownText {...textPart('## 标题\n\n| a | b |\n| --- | --- |\n| 1 | 2 |')} />)
    expect(screen.getByRole('heading', { level: 2 })).toHaveTextContent('标题')
    expect(screen.getByRole('cell', { name: '2' })).toBeInTheDocument()
  })

  it('does not leak react-markdown internals onto DOM elements', () => {
    renderMarkdown('[文档](https://opencode.ai/docs)\n\n| a | b |\n| --- | --- |\n| 1 | 2 |')
    // react-markdown passes the source hast node to every custom component; if it
    // is spread through, React writes a literal node="[object Object]" attribute.
    expect(document.querySelector('[node]')).toBeNull()
    expect(screen.getByRole('link', { name: '文档' }).getAttributeNames().sort()).toEqual(['href', 'rel', 'target'])
    expect(screen.getByRole('table').getAttributeNames()).toEqual([])
  })

  it('marks GFM task list items so the bullet can be suppressed', () => {
    renderMarkdown('- [x] 已完成\n- [ ] 未完成')
    const items = document.querySelectorAll('.agent-md li.task-list-item')
    expect(items).toHaveLength(2)
    expect(document.querySelector('.agent-md ul.contains-task-list')).not.toBeNull()
  })

  it('wraps every table in the horizontal scroll container', () => {
    renderMarkdown('| a | b |\n| --- | --- |\n| 1 | 2 |')
    const wrapper = document.querySelector('.agent-md-table')
    expect(wrapper?.firstElementChild?.tagName).toBe('TABLE')
  })

  it('renders empty text without throwing', () => {
    const { container } = renderMarkdown('')
    expect(container.querySelector('.agent-md')).not.toBeNull()
    expect(container.textContent).toBe('')
  })
})
