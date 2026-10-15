import { RefObject, useEffect, useRef } from 'react'

export function fieldValue(event: unknown): string {
  const target = (event as { target?: { value?: unknown } }).target
  return String(target?.value ?? '')
}

export function useMduiEvent(ref: RefObject<HTMLElement | null>, name: string, handler: (event: Event) => void) {
  const saved = useRef(handler)
  saved.current = handler
  useEffect(() => {
    const element = ref.current
    if (!element) return
    // MDUI events bubble across shadow roots. Closing a select/dropdown inside
    // a dialog must not be mistaken for closing the dialog itself.
    const listener = (event: Event) => { if (event.target === element) saved.current(event) }
    element.addEventListener(name, listener)
    return () => element.removeEventListener(name, listener)
  // Dialog refs may be null on the first render and mounted later. Rebind after
  // each render so conditional MDUI elements receive events too; cleanup keeps
  // a single listener and detaches it from replaced nodes.
  })
}
