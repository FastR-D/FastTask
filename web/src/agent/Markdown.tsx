import { memo, type ComponentPropsWithoutRef } from 'react'
import ReactMarkdown, { type ExtraProps } from 'react-markdown'
import remarkGfm from 'remark-gfm'
import type { TextMessagePartComponent } from '@assistant-ui/react'

// Assistant replies are CommonMark + GFM (headings, tables, task lists,
// strikethrough, autolinks). The bubble itself is `white-space: pre-wrap`, which
// is right for the composer's plain text but would double every line break once
// markdown owns the layout, so the rendered tree resets it in agent.css.
//
// Security: raw HTML in model output is never rendered — react-markdown skips
// html nodes unless rehype-raw is added, and it is deliberately not added here
// (server.go's CSP would block the inline styles/attributes it needs anyway).
// Links are forced to a new tab without opener access.
const remarkPlugins = [remarkGfm]

// react-markdown hands every custom component the source hast node as `node`.
// It must be destructured off before spreading, or React writes it to the DOM as
// a literal node="[object Object]" attribute.
type MdProps<T extends 'a' | 'table'> = ComponentPropsWithoutRef<T> & ExtraProps

function MarkdownLink({ node: _node, ...props }: MdProps<'a'>) {
  return <a {...props} target="_blank" rel="noopener noreferrer" />
}

// GFM tables are the one block that cannot shrink to the 400px width the
// dialogue page must support (frontend.md §7), so they get a scroll container
// instead of overflowing the bubble.
function MarkdownTable({ node: _node, ...props }: MdProps<'table'>) {
  return (
    <div className="agent-md-table">
      <table {...props} />
    </div>
  )
}

const components = { a: MarkdownLink, table: MarkdownTable }

// memo keeps a streaming reply cheap: assistant-ui re-renders the part on every
// delta, and re-parsing unchanged text (or a sibling part's update) is pure waste.
//
// `MessagePrimitive.Parts` spreads the part state flat onto the Text component
// (`jsx(Text, { ...part })`), so `text` is a direct prop, not `part.text`.
export const MarkdownText: TextMessagePartComponent = memo(function MarkdownText({ text }) {
  return (
    <div className="agent-md">
      <ReactMarkdown remarkPlugins={remarkPlugins} components={components}>
        {text}
      </ReactMarkdown>
    </div>
  )
})
