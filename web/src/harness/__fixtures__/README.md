# Harness fixtures

`doc/harness.md` §14.1 forbids hand-written protocol JSON: a fixture that was invented rather than
observed proves nothing about the real wire. These two files are transcriptions of what the Phase 0
spike measured against a live OpenAI-compatible endpoint (§16.2, §16.3) — the request libfx's shim
path produces, and the stream a qwen provider returns, including the interleaved fragment order the
spike recorded (`text-end` arriving after `tool-input-start`).

Re-record them after any libfx or `@ai-sdk/*` upgrade, and update the expected part sequence in
`shim.test.ts` in the same commit.
