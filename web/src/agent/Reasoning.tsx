import { useMessagePartReasoning } from '@assistant-ui/react'

// The chain-of-thought block (doc/chat-features.md §3.4).
//
// Three rules shape this component. It is folded by default, because a reasoning trace is long and a
// phone screen is not. It is display-only: nothing here parses the text or derives an action from it,
// since model reasoning is model output and therefore untrusted (agent.md §4 invariant 4). And it renders
// nothing at all when the text is empty, so turning persistence off on the server cannot leave a row of
// empty "思考过程" headers in the transcript.

export function ReasoningPart() {
  // The hook returns the part, not its text: a message can hold several reasoning segments, each with its
  // own id, and folding them into one string would lose that (§3.3).
  const part = useMessagePartReasoning()
  const text = part?.text ?? ''
  if (!text.trim()) return null
  return (
    <details className="agent-reasoning" data-testid="reasoning-block">
      <summary className="agent-reasoning-summary">
        <mdui-icon name="psychology" />
        <span>思考过程</span>
      </summary>
      <div className="agent-reasoning-body">{text}</div>
    </details>
  )
}
