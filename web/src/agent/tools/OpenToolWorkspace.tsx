import { makeAssistantToolUI } from '@assistant-ui/react'
import { workspaceURL } from '../../workspace-navigation'
import { ModuleWorkspaceLink } from '../../ModuleWorkspaceLink'
import { useComposerDraftUserId } from '../ComposerDraft'

const moduleNames: Record<string, string> = {
  fastwrite: 'FastWrite', fastread: 'FastRead', fastnews: 'FastNews', fastppt: 'FastPPT', fastlab: 'FastLab',
}

function workspaceLink(result: unknown): { label: string; url: string; system: string } | null {
  if (!result || typeof result !== 'object') return null
  const value = result as Record<string, unknown>
  if (value.action !== 'open_workspace' || value.status !== 'ready_to_open' || typeof value.system !== 'string' ||
      !moduleNames[value.system] || value.label !== moduleNames[value.system] || typeof value.url !== 'string') return null
  const url = workspaceURL(value.url, value.system)
  return url ? { label: moduleNames[value.system], url, system: value.system } : null
}

export function WorkspaceLink({ result, isError = false }: { result?: unknown; isError?: boolean }) {
  const userId = useComposerDraftUserId()
  if (result === undefined && !isError) return <p className="agent-approval-note" role="status">正在准备模块入口…</p>
  const link = !isError && workspaceLink(result)
  if (!link) return <p className="agent-approval-note" role="alert">暂时无法打开模块网页，请稍后重试或联系管理员检查模块地址。</p>
  return <div className="agent-workspace-link">
    <ModuleWorkspaceLink url={link.url} system={link.system} label={link.label} userId={userId} appearance="link"/>
  </div>
}

export const OpenToolWorkspaceUI = makeAssistantToolUI<Record<string, unknown>, unknown>({
  toolName: 'open_tool_workspace', display: 'standalone',
  render: ({ result, isError }) => <WorkspaceLink result={result} isError={isError} />,
})
