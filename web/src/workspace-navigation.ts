const portalEntries: Record<string, string> = {
  fastwrite: 'fast-write', fastread: 'fast-read', fastnews: 'fast-news', fastppt: 'fast-ppt',
}

/** Module navigation accepts only plain pages or the hub's fixed entry selector. */
export function workspaceURL(value?: string, system?: string): string | undefined {
  if (!value || /[\x00-\x1f\x7f\\]/.test(value)) return undefined
  try {
    const url = new URL(value)
    if (!['http:', 'https:'].includes(url.protocol) || !url.hostname || url.username || url.password || url.hash) return undefined
    if (!url.search) return url.href
    if (url.origin === 'https://fastrd.io' && url.pathname === '/workspace' && system && portalEntries[system] &&
        url.searchParams.size === 1 && url.searchParams.get('launch') === portalEntries[system]) return url.href
  } catch { /* Unknown or unsafe destinations are not rendered as links. */ }
  return undefined
}
