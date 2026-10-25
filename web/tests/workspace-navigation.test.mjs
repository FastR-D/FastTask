import assert from 'node:assert/strict';
import { test } from 'node:test';
import { workspaceURL } from '../src/workspace-navigation.ts';

test('module links accept only their exact portal entry and never a Key or arbitrary redirect', () => {
  assert.equal(workspaceURL('https://fastrd.io/workspace?launch=fast-write', 'fastwrite'), 'https://fastrd.io/workspace?launch=fast-write');
  assert.equal(workspaceURL('https://custom.example.org/read', 'fastread'), 'https://custom.example.org/read');
  for (const url of ['https://fastrd.io/workspace?launch=fast-read', 'https://fastrd.io/workspace?launch=fast-write&key=fk_secret', 'https://fastrd.io/workspace?launch=fast-write&next=https://evil.example', 'https://fastrd.io/workspace?launch=fast-write&launch=fast-write', 'https://evil.example/workspace?launch=fast-write', 'https://user@fastrd.io/write', 'javascript:alert(1)', 'https://fastrd.io/write#key', 'https://fastrd.io/\\evil']) {
    assert.equal(workspaceURL(url, 'fastwrite'), undefined, url);
  }
});
