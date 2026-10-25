import { makeAssistantToolUI } from '@assistant-ui/react'

const moduleNames: Record<string, string> = {
  fastwrite: 'FastWrite', fastread: 'FastRead', fastnews: 'FastNews', fastppt: 'FastPPT', fastlab: 'FastLab',
}

function workspaceLink(result: unknown): { label: string; url: string } | null {
  if (!result || typeof result !== 'object') return null
  const value = result as Record<string, unknown>
  if (value.action !== 'open_workspace' || value.status !== 'ready_to_open' || typeof value.system !== 'string' ||
      !moduleNames[value.system] || value.label !== moduleNames[value.system] || typeof value.url !== 'string') return null
  try {
    const url = new URL(value.url)
    if (!['http:', 'https:'].includes(url.protocol) || !url.hostname || url.username || url.password || url.search || url.hash || /[\r\n\\]/.test(value.url)) return null
    return { label: moduleNames[value.system], url: url.href }
  } catch { return null }
}

export function WorkspaceLink({ result, isError = false }: { result?: unknown; isError?: boolean }) {
  if (result === undefined && !isError) return <p className="agent-approval-note" role="status">正在准备模块入口…</p>
  const link = !isError && workspaceLink(result)
  if (!link) return <p className="agent-approval-note" role="alert">暂时无法打开模块网页，请稍后重试或联系管理员检查模块地址。</p>
  return <div className="agent-workspace-link">
    <a className="agent-workspace-action" href={link.url} target="_blank" rel="noopener noreferrer">打开 {link.label}<mdui-icon name="open_in_new" /></a>
    <p className="agent-approval-note">在对应网页继续操作，登录由该模块处理。</p>
  </div>
}

export const OpenToolWorkspaceUI = makeAssistantToolUI<Record<string, unknown>, unknown>({
  toolName: 'open_tool_workspace', display: 'standalone',
  render: ({ result, isError }) => <WorkspaceLink result={result} isError={isError} />,
})
