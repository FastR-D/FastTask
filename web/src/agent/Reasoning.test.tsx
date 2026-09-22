import { cleanup, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { ReasoningPart } from './Reasoning'

// The chain-of-thought block (doc/chat-features.md §3.4, acceptance §6.3 and §6.4).
//
// assistant-ui hands a part renderer its text through a hook rather than props, so the hook is what a
// test has to stand in for. Everything else here is the real component.

vi.mock('@assistant-ui/react', async (importOriginal) => {
  const actual = await importOriginal<Record<string, unknown>>()
  return {
    ...actual,
    useMessagePartReasoning: () => current,
  }
})

let current: { type: 'reasoning'; text: string } | undefined

afterEach(() => {
  current = undefined
  cleanup()
})

function renderReasoning(text: string | undefined) {
  current = text === undefined ? undefined : { type: 'reasoning', text }
  return render(<ReasoningPart />)
}

describe('the reasoning part', () => {
  it('is folded by default, and says what it is', () => {
    renderReasoning('先看看目标，再决定拆哪一步。')
    const block = screen.getByTestId('reasoning-block')
    // A <details> without `open` is collapsed: on a phone the trace is long and the screen is not.
    expect(block).not.toHaveAttribute('open')
    expect(block.querySelector('summary')).toHaveTextContent('思考过程')
    expect(block).toHaveTextContent('先看看目标，再决定拆哪一步。')
  })

  it('opens on request, since folding is a default and not a restriction', () => {
    renderReasoning('展开来看的细节。')
    const block = screen.getByTestId('reasoning-block')
    block.setAttribute('open', '')
    expect(block).toHaveAttribute('open')
    expect(block).toHaveTextContent('展开来看的细节。')
  })

  it('renders nothing at all when there is no reasoning to show', () => {
    // §6.4: with persistence off the server sends no reasoning, and the transcript must not grow a row of
    // empty "思考过程" headers. Whitespace-only counts as empty for the same reason.
    const { container } = renderReasoning('   \n  ')
    expect(container).toBeEmptyDOMElement()
    expect(screen.queryByTestId('reasoning-block')).not.toBeInTheDocument()
  })

  it('renders nothing when the part itself is missing', () => {
    const { container } = renderReasoning(undefined)
    expect(container).toBeEmptyDOMElement()
  })

  it('treats the text as untrusted output, not as markup or instructions', () => {
    // agent.md §4 invariant 4: reasoning is model output. It is displayed and nothing more — no markdown
    // parsing, no HTML, and certainly no reading an intended tool call out of it.
    renderReasoning('## 标题\n<img src=x onerror="window.pwned=true">\n<script>window.pwned=true</script>')
    const block = screen.getByTestId('reasoning-block')
    expect(block.querySelector('h2')).toBeNull()
    expect(block.querySelector('img')).toBeNull()
    expect(block.querySelector('script')).toBeNull()
    expect(block.textContent).toContain('<img src=x onerror="window.pwned=true">')
    expect((window as unknown as { pwned?: boolean }).pwned).toBeUndefined()
  })
})
