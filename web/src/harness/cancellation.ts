/** Cancellation is a server operation even when this page owns no local host. */
export async function requestRunCancellation(
  runId: string | undefined,
  send: (runId: string) => Promise<unknown>,
  abortLocal: () => void,
): Promise<void> {
  if (!runId) return
  try {
    await send(runId)
  } finally {
    abortLocal()
  }
}

/** Shared stop-button feedback. Delivery failure stays visible and retryable. */
export async function stopWithFeedback(
  stop: () => Promise<void>,
  notify: (message: string) => void,
  format: (error: unknown) => string = error => error instanceof Error ? error.message : String(error),
): Promise<boolean> {
  try { await stop(); return true }
  catch (error) { notify(`停止请求未送达：${format(error)}`); return false }
}
