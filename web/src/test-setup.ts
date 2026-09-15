import '@testing-library/jest-dom/vitest'

// jsdom 29 disables localStorage for opaque worker origins. Keep the browser
// contract available to components and tests even when Vitest runs without a
// localstorage file.
if (typeof globalThis.localStorage === 'undefined') {
  const values = new Map<string, string>()
  Object.defineProperty(globalThis, 'localStorage', {
    configurable: true,
    value: {
      get length() { return values.size },
      clear() { values.clear() },
      getItem(key: string) { return values.get(key) ?? null },
      key(index: number) { return Array.from(values.keys())[index] ?? null },
      removeItem(key: string) { values.delete(key) },
      setItem(key: string, value: string) { values.set(String(key), String(value)) },
    } satisfies Storage,
  })
}
