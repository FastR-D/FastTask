# FastResearch today v1

Owner: FastTask. `POST /api/v1/integrations/fastresearch/today`, JSON, `Cache-Control: no-store`.
The path version is the contract release identifier, not a required body field.

Primary identity: FastCAS client_credentials, scope `integrations:fastresearch`.
Issuer registration must issue the Task audience (default `fasttask-api`);
the SDK grant does not accept an arbitrary audience override. Task verifies the
signature, issuer, audience, expiry, access token_use, service=true, sub=client_id,
client allowlist and each normalized receiver's exact allowlist entry.

Legacy identity remains an actual supported path: `X-API-Key` (or existing
`X-FastResearch-Provision-Key`) only when `FASTTASK_FASTRESEARCH_LEGACY_INGEST_KEY_ENABLED=true`
and the configured ingest key matches. Never send it with Authorization; never
retry a rejected Bearer using the key. Credentials are server-side only.

`request.schema.json` describes raw wire input; normalization takes the first 200
unique nonempty trimmed refs, preserving order. Empty normalized lists return
400. `cases.json` supplies synthetic fixtures for both HTTP receiver and Research
client tests. Unknown receivers have found=false, items=[], zero progress.
Known receivers include user/date/plan metadata and items. A per-member `error`
may accompany lookup/refresh failures; HTTP 200 alone is not a complete-member
success. RefreshSchedule may derive today's plan; this API does not create or
complete underlying tasks. Counts reflect satisfied/in_progress/skipped/planned.

The positive fixture maps `account-a` to an active user, a frozen current UTC
plan at revision 3, and three real task/item rows: in_progress, satisfied and
planned. The receiver test seeds domain rows in SQLite; it does not inject HTTP
responses or replace RefreshSchedule/PlanItemViews. It compares the entire wire
response, including Task IDs, commitments, completion metadata, counts and the
FastRead/FastWrite workspace projections. `missing-account` stays unknown.
`$today` is replaced with the current UTC calendar date by both runners; stable
IDs and the fixed updated_at remain exact assertions. `research_today` records
the expected Research member projection, including snake_case to camelCase
fields and null workspace for an unclassified task.

The mapped `outside` account is deliberately absent from the service allowlist:
service queries return 403, while the explicitly enabled legacy shared key can
still read it. Legacy has its existing integration-wide authority, not the
service client's receiver restriction.

Run `GOWORK=off go test ./internal/httpapi -run TestFastResearchTodayWireContract -count=1`.
For a local cross-product run, also set `FASTRESEARCH_CONTRACT_PROBE` to the
absolute Research `scripts/fasttask-today-contract.mjs` path. This makes the same
receiver test run the installed npm SDK through Research's production factory,
against live loopback Task and a disposable signed OIDC issuer, then starts the
actual Research server and queries its member today route with service-only
configuration. The issuer counts grants to verify token reuse. Ordinary product
CI needs only its own checkout and registry dependencies.

Research vendors this fixture under `contracts/fastresearch-today/v1`; updates
must be copied from this receiver-owned directory and reviewed on both sides.
No tasks/progress/accounts endpoint is promised by this contract. These two ends
are not evidence of two independent sending products or a deployed FastCAS run.
