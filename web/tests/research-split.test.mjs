import assert from 'node:assert/strict';
import { test } from 'node:test';
import {
  DEFAULT_ASSISTANT_RATIO,
  MIN_PANE_WIDTH,
  RESIZER_WIDTH,
  assistantBounds,
  assistantWidth,
  parseAssistantRatio,
  parseLegacyAssistantWidth,
  ratioAtPointer,
  ratioForWidth,
} from '../src/research-split.ts';

function near(actual, expected) { assert.ok(Math.abs(actual - expected) < 1e-8, `${actual} !== ${expected}`); }

test('absent or corrupt preferences do not become a zero-width preference', () => {
  for (const value of [null, '', ' ', 'NaN', 'Infinity', '-1', '0', '1', '1.2']) {
    assert.equal(parseAssistantRatio(value), null);
  }
  for (const value of [null, '', ' ', 'NaN', 'Infinity', '-1', '0']) {
    assert.equal(parseLegacyAssistantWidth(value), null);
  }
  assert.equal(parseLegacyAssistantWidth('800'), 800);
  assert.equal(DEFAULT_ASSISTANT_RATIO, 0.4);
});

test('preferences above eighty percent survive serialization and refresh', () => {
  const ratio = ratioForWidth(3345, 3000);
  assert.ok(ratio > 0.8);
  near(assistantWidth(3345, parseAssistantRatio(String(ratio))), 3000);
  assert.equal(parseAssistantRatio('0.95'), 0.95);
});

test('wide windows can expand the assistant beyond 800px while preserving both panes', () => {
  const width = 3345;
  const bounds = assistantBounds(width);
  assert.equal(bounds.min, MIN_PANE_WIDTH);
  assert.equal(bounds.max, width - RESIZER_WIDTH - MIN_PANE_WIDTH);
  near(assistantWidth(width, ratioForWidth(width, 1400)), 1400);
  near(assistantWidth(width, ratioForWidth(width, 10000)), bounds.max);
  near(assistantWidth(width, ratioForWidth(width, -100)), bounds.min);
});

test('window resize clamps presentation without losing the preferred proportion', () => {
  const preferred = 0.86;
  assert.equal(assistantWidth(808, preferred), 480);
  near(assistantWidth(2408, preferred), 2064);
  assert.equal(preferred, 0.86);
});

test('dragging back from a resized limit moves immediately from the visible divider', () => {
  const workspaceWidth = 1200;
  const workspaceRight = 1432;
  const visibleWidth = assistantWidth(workspaceWidth, 0.95);
  const visibleCenter = workspaceRight - visibleWidth - RESIZER_WIDTH / 2;
  const grabOffset = 3;
  const ratio = ratioAtPointer(workspaceWidth, workspaceRight, visibleCenter + grabOffset + 20, grabOffset);
  near(assistantWidth(workspaceWidth, ratio), visibleWidth - 20);
});

test('pointer calculation includes the navigation offset and preserves the grab point', () => {
  const workspaceWidth = 2000;
  const workspaceRight = 2232;
  const center = workspaceRight - 1000 - RESIZER_WIDTH / 2;
  for (const grabOffset of [-3, 0, 3]) {
    const ratio = ratioAtPointer(workspaceWidth, workspaceRight, center + grabOffset - 120, grabOffset);
    near(assistantWidth(workspaceWidth, ratio), 1120);
  }
});

test('Home and End use the current container boundaries and can be reversed', () => {
  const workspaceWidth = 2408;
  const { min, max } = assistantBounds(workspaceWidth);
  near(assistantWidth(workspaceWidth, ratioForWidth(workspaceWidth, min)), 320);
  near(assistantWidth(workspaceWidth, ratioForWidth(workspaceWidth, max)), 2080);
  near(assistantWidth(workspaceWidth, ratioForWidth(workspaceWidth, max - 16)), 2064);
  near(assistantWidth(workspaceWidth, DEFAULT_ASSISTANT_RATIO), 960);
});

test('legacy pixel preference migrates against the actual container without an 800px cap', () => {
  const legacy = parseLegacyAssistantWidth('800');
  const migrated = ratioForWidth(2008, legacy);
  near(migrated, 0.4);
  near(assistantWidth(3008, migrated), 1200);
});

test('unmeasured and narrow containers have finite, ordered bounds', () => {
  for (const width of [0, -1, NaN, 8, 320, 647, 648]) {
    const { total, min, max } = assistantBounds(width);
    assert.ok(Number.isFinite(total) && total >= 0);
    assert.ok(min >= 0 && max >= min && max <= total);
    assert.ok(Number.isFinite(assistantWidth(width, 0.4)));
    assert.ok(Number.isFinite(ratioForWidth(width, 400)));
  }
});
