import { useAgentRuntime, type AgentRuntimeOptions } from '../../src/agent/runtime'
import type { ServerAgentState } from '../../src/agent/state'

export function AgentRuntimeProbe(props: { initialState: ServerAgentState; onNotice: (message: string) => void; options: AgentRuntimeOptions }) {
  return useAgentRuntime(props.initialState, props.onNotice, props.options)
}
