/** Fetch rejects network failures with TypeError. HTTP and business errors are ordinary outcomes
 *  (or Error from the HTTP adapter), and aborts must never issue a second request. The callback
 *  closes over the exact call ID and original input, so an uncertain response can only replay that
 *  same execution on the server. There is at most one retry and no retry loop. */
export async function retryToolNetworkRequest<T>(send: () => Promise<T>, signal: AbortSignal): Promise<T> {
  try {
    return await send()
  } catch (error) {
    if (signal.aborted || !(error instanceof TypeError) || error.name !== 'TypeError') throw error
    return send()
  }
}
