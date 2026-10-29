# LLM gateway (chat slice, M2 #32; quotas #33)

> - A signed-in user creates a conversation, sends a message and gets the answer token by token as Server-Sent Events.
> - The model comes from `SYNTHWERK_LLM_PROVIDER` (`ollama`, `openai_compat`, `fake`). No silent fallback.
> - A request without a valid user token gets 401 before any database or model call.
> - A conversation belongs to one user in one org. Postgres row level security enforces it.
> - Logs, spans and events hold metadata only. Never the prompt, never the answer.
> - Every finished answer emits `dev.synthwerk.llm.message.completed.v1` on the stream `EVT_LLM` (kit outbox).
> - Tokens count per user and per org in a UTC day. Over quota: 429 `quota_exhausted` with the scope and the reset, no model call.

## API

Contract: `synthwerk-contracts/openapi/llm/openapi.yaml`. All routes need `Authorization: Bearer <access token>`.

| Method | Path | Result |
|---|---|---|
| POST | `/v1/conversations` | 201, empty conversation. Body `{"model":"<alias>"}` is optional. |
| GET | `/v1/conversations/{id}` | 200 with messages, oldest first. Another owner's id: 404, same body as an unknown id. |
| POST | `/v1/conversations/{id}/messages` | 200 `text/event-stream`: `delta`… `usage` `done`, or `error`. |
| GET | `/healthz`, `/readyz` | Kit health. `/readyz` checks `postgres`, `nats` (critical) and `jwks` (degraded only). |

Refusal order of a send. The model call is last:

1. Token (kit `auth.Middleware`, JWKS) → 401 `unauthenticated`.
2. `synthwerk:origin` claim differs from `Origin` → 403 `forbidden`.
3. Body → 400 `invalid_request`. Conversation of another owner → 404. Over `MAX_INPUT_CHARS` → 413 `input_too_large`.
4. More than `MAX_STREAMS_PER_OWNER` open streams → 429 `too_many_streams`. Read the quota limits.
5. Store the turn and book the quota in one transaction. Over quota → 429 `quota_exhausted`, nothing stored. Wait up to `QUEUE_TIMEOUT` for a model slot → 503 `model_unavailable`. Call the provider.

After the first byte the status stays 200; a failure is an `error` event. A client that closes the stream cancels the model call. The partial answer is stored with status `canceled`.

## Stream

- Frame: `id: <message_id>:<seq>`, `event: <name>`, `data: <one line of JSON>`.
- `: ping` every 15 s. Headers `Cache-Control: no-store`, `X-Accel-Buffering: no`.
- Every frame is flushed at once with its own 10 s write deadline. The server has no `WriteTimeout` and no `ReadTimeout` (a read deadline cancels the request context mid-stream). `STREAM_TIMEOUT` bounds one answer.

## Quotas

- Window: one UTC day. Unit: tokens (input + output). Two limits apply to every send: the user's own (`QUOTA_USER_TOKENS`) and the org's shared one (`QUOTA_ORG_TOKENS`). `0` = no limit; usage is still counted.
- Book before the stream: the turn transaction adds an estimate to `reserved` of the org row, then of the user row. Estimate = the model messages (history, system prompt, new message; about 4 characters per token) + `QUOTA_RESERVE_TOKENS` for the answer. A row where `used + reserved + estimate` would pass the limit stops the send.
- Concurrency: the booking is one conditional `UPDATE` per row. It locks the row until commit, so concurrent sends of one org queue on the org row and each sees the bookings before it. Order is always org → user (book, finish, sweep): no deadlock.
- Settle at the end: the finish transaction replaces the booking with the charge. Completed: the provider's input + output tokens. Ended early after the provider took the request (abort, mid-stream failure, timeout): input estimate + partial answer estimate. Provider refused or never called: 0.
- An answer can cost more than its booking (long answer). Then `used` may pass the limit by that difference once; the next send is refused.
- Crash: the startup sweep marks the answer `failed` and charges its full booking.
- Refusal body (contracts `QuotaExceeded`): `{"code":"quota_exhausted", ..., "quota":{"scope":"user|org","unit":"tokens","limit":N,"resets_at":"<next UTC midnight>"}}`, `Retry-After` = seconds to the reset. No usage numbers of peers.
- Log line `llm.quota.exhausted` with scope and limit. No content.
- Limits come from `quota.Source`. Today `quota.Static` reads config. When identity serves entitlements (#31), a `Source` that reads the org's plan replaces it; the store and the handler stay the same.

## Providers

`internal/provider`: `Provider{Name(); Chat(ctx, Request) (Stream, error)}`, `Stream{Next(); Result(); Close()}`. One message shape (role + text). A new adapter is a new package plus one case in `internal/provider/registry`. The eval runner (#72) uses the same interface.

| Name | Adapter | Notes |
|---|---|---|
| `ollama` | `openaicompat` | `BASE_URL` default `http://ollama:11434/v1`. `stream_options.include_usage`. |
| `openai_compat` | `openaicompat` | `BASE_URL` required. `API_KEY` or `API_KEY_FILE`. |
| `fake` | `fake` | Echoes the last user message, one word per piece, `FAKE_DELAY` between pieces. Refused in `stg` and `prd`. |

`Request.Options` (temperature, seed, max tokens, thinking off) is empty for the chat handler, so the model defaults apply. The eval (`evals/README.md`) pins them.

Adapter errors never hold the provider's body or the prompt. `tests/contract/provider` runs the same conformance checks on every adapter.

## Data

Migrations `0002_conversations.sql` and `0003_quota.sql`, schema `llm`:

- `conversation(id, tenant_id, owner_id, model_alias, created_at, deleted_at)`, `message(id, conversation_id, tenant_id, owner_id, role, content, status, tokens_in, tokens_out, created_at)`.
- `ENABLE` + `FORCE ROW LEVEL SECURITY`: the service role owns the schema and is still bound by the policies. Each transaction sets `synthwerk.tenant_id` and `synthwerk.owner_id` with `set_config(..., true)` from the verified token.
- A startup sweep marks answers left in `streaming` by a crash as `failed` (policy `sweep`, only for that transaction). It touches only rows older than `STREAM_TIMEOUT` + 1 min, so it never fails a live stream of another replica.
- `quota_usage(tenant_id, owner_id, window_start, used, reserved)`: one org row (`owner_id = ''`) and one row per user, per day. RLS: a transaction sees its org row and its own row, never a peer's row. `message.quota_reserved` and `message.quota_window` hold the booking of each answer, so the finish and the sweep settle exactly that amount.

## Event

`dev.synthwerk.llm.message.completed.v1` on `evt.llm.message.completed.v1`, stream `EVT_LLM` (`evt.llm.>`, created or updated by the service at start). Written in the same transaction as the final message (kit outbox), for `completed`, `failed` and `canceled`. `subject` = message id, `orgid` = org, `actor` = `user:<sub>`. Data: ids, model alias, provider, token counts, latency, time to first token, status.

## Config

`SYNTHWERK_LLM_*` (see `cmd/llm/main.go` and `internal/config`): `PROVIDER`, `BASE_URL`, `API_KEY[_FILE]`, `MODEL_ALIASES` (`default=qwen3.5:4b`; the alias `default` must exist), `SYSTEM_PROMPT`, `MAX_INPUT_CHARS` (8000), `MAX_STREAMS_PER_OWNER` (2), `MAX_CONTEXT_CHARS` (24000), `STREAM_TIMEOUT` (120s), `PROVIDER_CONCURRENCY` (1), `QUEUE_TIMEOUT` (10s), `FAKE_DELAY` (0s), `QUOTA_USER_TOKENS` (50000 per UTC day, 0 = no limit), `QUOTA_ORG_TOKENS` (500000, 0 = no limit), `QUOTA_RESERVE_TOKENS` (1024; a non-zero limit must be above it), `OIDC_ISSUER`, `OIDC_AUDIENCE`, `JWKS_URL`, `DATABASE_URL`, `NATS_URL`, `HTTP_ADDR`.

## Tests

- `tests/unit/chat` — `Send - no valid token (none, malformed, expired, bad signature, other issuer, wrong aud, alg none, HS256, no sub, no org) - 401, no store call, no model call`; `Send - other origin - 403`.
- `tests/integration/chat` (Postgres + NATS in Testcontainers, `go test -tags integration`) — streaming with arrival gaps, user isolation and 404 for another owner or org, RLS without session vars, log and span canaries on success, provider down, mid-stream failure, client abort and 401; 413, 429, abort keeps the partial answer, relay to `EVT_LLM` without text, finish rollback, startup sweep.
- `tests/integration/chat/quota_test.go` — `Send - user over quota - 429 quota_exhausted with scope user, no model call, nothing stored`; `Send - org over quota - 429 with scope org for another user of the org`; `Send - many users of one org at once - exactly as many streams start as bookings fit`; `Finish - answer ends early - the booking settles to what was spent`; `Startup - answer stuck in streaming - sweep charges its full booking`.
- `tests/contract/provider` — conformance of `fake` and `openaicompat`.
- `tests/unit/config`, `tests/unit/sse`, `tests/unit/quota` (UTC window, `Retry-After` range, error matching).

## Not in this slice

Entitlement-based limits (#31, the `quota.Source` seam is ready), a `GET` of the quota state for the widget, deletion of old `quota_usage` rows, Anthropic adapter (E4), `GET /v1/models`, list/delete/resume of conversations, a second DB role for migrations, scope checks (claim names wait for #28/#30).
