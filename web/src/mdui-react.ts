import { RefObject, useEffect, useRef } from 'react'

export function fieldValue(event: unknown): string {
  const target = (event as { target?: { value?: unknown } }).target
  return String(target?.value ?? '')
}

export function useMduiEvent(ref: RefObject<HTMLElement | null>, name: string, handler: () => void) {
  const saved = useRef(handler)
  saved.current = handler
  useEffect(() => {
    const element = ref.current
    if (!element) return
    const listener = () => saved.current()
    element.addEventListener(name, listener)
    return () => element.removeEventListener(name, listener)
  }, [ref, name])
}
