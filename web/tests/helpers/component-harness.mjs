import { fileURLToPath } from 'node:url';
import { existsSync } from 'node:fs';
import { dirname, isAbsolute, resolve } from 'node:path';
import { runInNewContext } from 'node:vm';
import { build } from 'rolldown';

// Execute the actual component and event handlers with deterministic hook and
// transport scheduling. This checks state transitions, not browser layout.
export async function createComponentHarness(file, name, { mocks = {}, globals = {}, commit = () => {} } = {}) {
  const slots = [], pendingEffects = [];
  let cursor = 0, output, props, dirty = false, mounted = true;
  const changed = (previous, next) => !previous || !next || previous.length !== next.length || next.some((value, i) => !Object.is(value, previous[i]));
  const useState = initial => {
    const index = cursor++;
    if (!(index in slots)) slots[index] = { value: typeof initial === 'function' ? initial() : initial };
    return [slots[index].value, update => {
      const next = typeof update === 'function' ? update(slots[index].value) : update;
      if (!Object.is(next, slots[index].value)) { slots[index].value = next; dirty = true; }
    }];
  };
  const useRef = initial => {
    const index = cursor++;
    if (!(index in slots)) slots[index] = { current: initial };
    return slots[index];
  };
  const useEffect = (effect, deps) => {
    const index = cursor++, previous = slots[index];
    if (!previous || changed(previous.deps, deps)) {
      slots[index] = { deps, cleanup: previous?.cleanup };
      pendingEffects.push(() => { slots[index].cleanup?.(); slots[index].cleanup = effect(); });
    }
  };
  const useMemo = (factory, deps) => {
    const index = cursor++;
    if (!slots[index] || changed(slots[index].deps, deps)) slots[index] = { deps, value: factory() };
    return slots[index].value;
  };
  const useId = () => { const index = cursor++; return slots[index] ??= `test-id-${index}`; };
  const useSyncExternalStore = (subscribe, getSnapshot) => {
    const [, update] = useState(0);
    useEffect(() => subscribe(() => update(value => value + 1)), [subscribe]);
    return getSnapshot();
  };
  const react = { useState, useRef, useEffect, useLayoutEffect: useEffect, useMemo, useId, useSyncExternalStore, useCallback: (callback, deps) => useMemo(() => callback, deps) };
  const jsx = (type, props = {}, key) => ({ type, props, key });
  const builtins = { react, 'react/jsx-runtime': { jsx, jsxs: jsx, Fragment: Symbol.for('test.fragment') } };
  const defaultGlobals = { console, Date, Intl, URL, Headers, FormData, AbortController, AbortSignal, TextDecoder, TextEncoder, setTimeout, clearTimeout, setInterval, clearInterval, queueMicrotask };
  const filename = file instanceof URL ? fileURLToPath(file) : resolve(file);
  // Use the same installed toolchain as Vite. TypeScript 7's native compiler
  // intentionally no longer exposes the old transpileModule API.
  const bundle = await build({
    input: filename, write: false, output: { format: 'cjs' },
    transform: { jsx: { runtime: 'automatic' } },
    external: id => Object.hasOwn(mocks, id) || Object.hasOwn(builtins, id) || id.endsWith('.css') || !id.startsWith('.') && !isAbsolute(id),
    plugins: [{ name: 'component-test-source', resolveId(id, importer) {
      if (!id.startsWith('.') || !importer) return null;
      const base = resolve(dirname(importer), id);
      // Be explicit about .ts before .tsx: this repository has both gantt.ts
      // and Gantt.tsx, which need distinct resolution on Windows as well.
      return [`${base}.ts`, `${base}.tsx`].find(path => existsSync(path)) ?? null;
    } }],
  });
  const source = bundle.output.find(chunk => chunk.type === 'chunk')?.code;
  if (!source) throw new Error(`No component output for ${filename}`);
  const module = { exports: {} };
  const require = id => {
    if (Object.hasOwn(mocks, id)) return mocks[id];
    if (Object.hasOwn(builtins, id)) return builtins[id];
    if (id.endsWith('.css')) return {};
    throw new Error(`Provide a mock for ${id} imported by ${filename}`);
  };
  runInNewContext(source, { ...defaultGlobals, ...globals, require, module, exports: module.exports }, { filename });
  const Component = module.exports[name];
  if (typeof Component !== 'function') throw new Error(`Missing component or hook ${name}`);
  function render(nextProps = props) {
    props = nextProps; cursor = 0; dirty = false; output = Component(props);
    commit(output);
    while (pendingEffects.length) pendingEffects.shift()();
    return output;
  }
  return {
    render,
    async flush() {
      // Resolve chained promises and effects, while leaving deliberately
      // deferred network requests pending for the test to release.
      for (let pass = 0; pass < 12; pass++) {
        await new Promise(setImmediate);
        if (dirty && mounted) render();
      }
      return output;
    },
    unmount() { mounted = false; for (const slot of slots) slot?.cleanup?.(); },
    get output() { return output; },
  };
}

export function nodes(tree, predicate = () => true) {
  const result = [];
  function visit(node) {
    if (Array.isArray(node)) { node.forEach(visit); return; }
    if (node && typeof node === 'object' && 'props' in node) {
      if (predicate(node)) result.push(node);
      visit(node.props.children);
    }
  }
  visit(tree); return result;
}

export function text(tree) {
  if (Array.isArray(tree)) return tree.map(text).join('');
  if (tree && typeof tree === 'object') return text(tree.props?.children);
  return tree === null || tree === undefined || typeof tree === 'boolean' ? '' : String(tree);
}

export function deferred() {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}

export function eventTarget(initial = {}) {
  const handlers = new Map();
  return { ...initial,
    addEventListener(type, handler) { if (!handlers.has(type)) handlers.set(type, new Set()); handlers.get(type).add(handler); },
    removeEventListener(type, handler) { handlers.get(type)?.delete(handler); },
    dispatch(type) { for (const handler of [...handlers.get(type) ?? []]) handler({ type }); },
  };
}
