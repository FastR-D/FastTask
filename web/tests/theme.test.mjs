import assert from 'node:assert/strict';
import { test } from 'node:test';
import { readFileSync } from 'node:fs';
import { runInNewContext } from 'node:vm';
import {
  applyTheme,
  readThemePreference,
  resolveTheme,
  setThemePreference,
  writeThemePreference,
} from '../src/theme.ts';

function memoryStorage() {
  const values = new Map();
  return {
    values,
    getItem: key => values.get(key) ?? null,
    setItem: (key, value) => values.set(key, String(value)),
    removeItem: key => values.delete(key),
  };
}

function fakeDocument(initialClass = 'mdui-theme-auto') {
  const classes = new Set(initialClass.split(' '));
  const metas = [];
  const meta = (media, content) => ({
    media,
    content,
    parentNode: {},
    removed: false,
    setAttribute(name, value) { this[name] = value },
    removeAttribute(name) { delete this[name] },
    remove() { this.removed = true },
  });
  metas.push(meta('(prefers-color-scheme: light)', '#f7f8fa'), meta('(prefers-color-scheme: dark)', '#10161e'));
  return {
    classes,
    metas,
    documentElement: {
      classList: {
        add: value => classes.add(value),
        remove: (...values) => values.forEach(value => classes.delete(value)),
      },
    },
    querySelectorAll: () => metas.filter(value => !value.removed),
    createElement: () => meta(),
    head: { appendChild(node) { metas.push(node); return node } },
  };
}

test('the theme choice persists, treats unknown storage as system, and survives blocked storage', () => {
  const storage = memoryStorage();
  assert.equal(readThemePreference(storage), 'system');
  writeThemePreference('dark', storage);
  assert.equal(readThemePreference(storage), 'dark');
  writeThemePreference('system', storage);
  assert.equal(storage.values.size, 0);

  writeThemePreference('sepia', storage);
  storage.setItem('fasttask.theme', 'sepia');
  assert.equal(readThemePreference(storage), 'system');

  const blocked = {
    getItem() { throw new Error('blocked') },
    setItem() { throw new Error('blocked') },
    removeItem() { throw new Error('blocked') },
  };
  assert.equal(readThemePreference(blocked), 'system');
  assert.equal(setThemePreference('light', undefined, blocked), 'light');
});

test('system resolves against the operating system while a pin ignores it', () => {
  assert.equal(resolveTheme('system', true), 'dark');
  assert.equal(resolveTheme('system', false), 'light');
  assert.equal(resolveTheme('light', true), 'light');
  assert.equal(resolveTheme('dark', false), 'dark');
});

test('applying a pin sets the MDUI class and one resolved browser chrome colour', () => {
  const doc = fakeDocument();
  assert.equal(applyTheme('light', doc, true), 'light');
  assert.deepEqual([...doc.classes], ['mdui-theme-light']);
  assert.equal(doc.metas[0].content, '#f7f8fa');
  assert.equal('media' in doc.metas[0], false);
  assert.equal(doc.metas[1].removed, true);

  assert.equal(applyTheme('system', doc, true), 'dark');
  assert.deepEqual([...doc.classes], ['mdui-theme-auto']);
  assert.equal(doc.metas[0].content, '#10161e');
});

test('the blocking boot script restores the theme before React, including when storage is blocked', () => {
  const script = readFileSync(new URL('../public/theme-init.js', import.meta.url), 'utf8');
  for (const [stored, darkOS, expectedClass, expectedColor] of [
    ['light', true, 'mdui-theme-light', '#f7f8fa'],
    ['dark', false, 'mdui-theme-dark', '#10161e'],
    [null, true, 'mdui-theme-auto', '#10161e'],
    ['invalid', false, 'mdui-theme-auto', '#f7f8fa'],
    ['blocked', true, 'mdui-theme-auto', '#10161e'],
  ]) {
    const doc = fakeDocument();
    runInNewContext(script, {
      document: doc,
      localStorage: { getItem() { if (stored === 'blocked') throw new Error('blocked'); return stored } },
      window: { matchMedia: () => ({ matches: darkOS }) },
    });
    assert.deepEqual([...doc.classes], [expectedClass]);
    assert.equal(doc.metas[0].content, expectedColor);
    assert.equal(doc.metas[1].removed, true);
  }
});
