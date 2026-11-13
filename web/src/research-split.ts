export const ASSISTANT_RATIO_KEY = 'fasttask.research-assistant.ratio'
export const ASSISTANT_WIDTH_KEY = 'fasttask.research-assistant.width'
export const DEFAULT_ASSISTANT_RATIO = 0.4
export const MIN_PANE_WIDTH = 320
export const RESIZER_WIDTH = 8

export function parseAssistantRatio(value: string | null): number | null {
  if (value === null || value.trim() === '') return null
  const ratio = Number(value)
  return Number.isFinite(ratio) && ratio > 0 && ratio < 1 ? ratio : null
}

export function parseLegacyAssistantWidth(value: string | null): number | null {
  if (value === null || value.trim() === '') return null
  const width = Number(value)
  return Number.isFinite(width) && width > 0 ? width : null
}

export function assistantBounds(workspaceWidth: number) {
  const total = Math.max(0, (Number.isFinite(workspaceWidth) ? workspaceWidth : 0) - RESIZER_WIDTH)
  const min = Math.min(MIN_PANE_WIDTH, total / 2)
  return { total, min, max: Math.max(min, total - MIN_PANE_WIDTH) }
}

export function assistantWidth(workspaceWidth: number, ratio: number): number {
  const { total, min, max } = assistantBounds(workspaceWidth)
  return Math.min(max, Math.max(min, total * ratio))
}

export function ratioForWidth(workspaceWidth: number, requestedWidth: number): number {
  const { total, min, max } = assistantBounds(workspaceWidth)
  return total > 0 ? Math.min(max, Math.max(min, requestedWidth)) / total : DEFAULT_ASSISTANT_RATIO
}

// The grab offset keeps the separator under the same point of the pointer.
// Working from its visible position avoids a saved, clamped width causing a dead zone.
export function ratioAtPointer(workspaceWidth: number, workspaceRight: number, pointerX: number, grabOffset: number): number {
  return ratioForWidth(workspaceWidth, workspaceRight - pointerX + grabOffset - RESIZER_WIDTH / 2)
}
