import { memo, type ComponentProps, type ComponentPropsWithoutRef } from 'react'
import ReactMarkdown, { type ExtraProps } from 'react-markdown'
import remarkBreaks from 'remark-breaks'
import remarkGfm from 'remark-gfm'
import type { TextMessagePartComponent } from '@assistant-ui/react'

// Both bubbles render CommonMark + GFM (headings, tables, task lists,
// strikethrough, autolinks). The bubble itself is `white-space: pre-wrap`, which
// is right for the composer's plain text but would double every line break once
// markdown owns the layout, so the rendered tree resets it in agent.css.
//
// Security: raw HTML is never rendered — react-markdown skips html nodes unless
// rehype-raw is added, and it is deliberately not added here (server.go's CSP
// would block the inline styles/attributes it needs anyway). Links are forced to
// a new tab without opener access.
const gfm = [remarkGfm]
// A user types Shift+Enter for a line break, and CommonMark folds a soft break
// into a space — their message would come back as one run-on paragraph.
// remark-breaks turns soft breaks into <br>, the way GitHub comments do. Model
// output stays on strict CommonMark: it already emits real paragraph breaks, and
// honouring every newline there would double-space deliberately hard-wrapped text.
const gfmWithBreaks = [remarkGfm, remarkBreaks]

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
type RemarkPlugins = ComponentProps<typeof ReactMarkdown>['remarkPlugins']

function makeMarkdownText(displayName: string, remarkPlugins: RemarkPlugins): TextMessagePartComponent {
  const rendered = memo(function MarkdownPart({ text }: { text: string }) {
    return (
      <div className="agent-md">
        <ReactMarkdown remarkPlugins={remarkPlugins} components={components}>
          {text}
        </ReactMarkdown>
      </div>
    )
  })
  rendered.displayName = displayName
  return rendered
}

export const MarkdownText = makeMarkdownText('MarkdownText', gfm)
export const UserMarkdownText = makeMarkdownText('UserMarkdownText', gfmWithBreaks)
