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
