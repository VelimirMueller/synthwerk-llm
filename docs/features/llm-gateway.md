# LLM gateway (chat slice, M2 #32)

> - A signed-in user creates a conversation, sends a message and gets the answer token by token as Server-Sent Events.
> - The model comes from `SYNTHWERK_LLM_PROVIDER` (`ollama`, `openai_compat`, `fake`). No silent fallback.
> - A request without a valid user token gets 401 before any database or model call.
> - A conversation belongs to one user in one org. Postgres row level security enforces it.
> - Logs, spans and events hold metadata only. Never the prompt, never the answer.
> - Every finished answer emits `dev.synthwerk.llm.message.completed.v1` on the stream `EVT_LLM` (kit outbox).

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
4. More than `MAX_STREAMS_PER_OWNER` open streams → 429 `too_many_streams`. Quota guard (no-op until #33).
5. Store the turn. Wait up to `QUEUE_TIMEOUT` for a model slot → 503 `model_unavailable`. Call the provider.

After the first byte the status stays 200; a failure is an `error` event. A client that closes the stream cancels the model call. The partial answer is stored with status `canceled`.

## Stream

- Frame: `id: <message_id>:<seq>`, `event: <name>`, `data: <one line of JSON>`.
- `: ping` every 15 s. Headers `Cache-Control: no-store`, `X-Accel-Buffering: no`.
- Every frame is flushed at once with its own 10 s write deadline. The server has no `WriteTimeout` and no `ReadTimeout` (a read deadline cancels the request context mid-stream). `STREAM_TIMEOUT` bounds one answer.

## Providers

`internal/provider`: `Provider{Name(); Chat(ctx, Request) (Stream, error)}`, `Stream{Next(); Result(); Close()}`. One message shape (role + text). A new adapter is a new package plus one case in `internal/provider/registry`. The eval runner (#72) uses the same interface.

| Name | Adapter | Notes |
|---|---|---|
| `ollama` | `openaicompat` | `BASE_URL` default `http://ollama:11434/v1`. `stream_options.include_usage`. |
| `openai_compat` | `openaicompat` | `BASE_URL` required. `API_KEY` or `API_KEY_FILE`. |
| `fake` | `fake` | Echoes the last user message, one word per piece, `FAKE_DELAY` between pieces. Refused in `stg` and `prd`. |

Adapter errors never hold the provider's body or the prompt. `tests/contract/provider` runs the same conformance checks on every adapter.

## Data

Migration `0002_conversations.sql`, schema `llm`:

- `conversation(id, tenant_id, owner_id, model_alias, created_at, deleted_at)`, `message(id, conversation_id, tenant_id, owner_id, role, content, status, tokens_in, tokens_out, created_at)`.
- `ENABLE` + `FORCE ROW LEVEL SECURITY`: the service role owns the schema and is still bound by the policies. Each transaction sets `synthwerk.tenant_id` and `synthwerk.owner_id` with `set_config(..., true)` from the verified token.
- A startup sweep marks answers left in `streaming` by a crash as `failed` (policy `sweep`, only for that transaction). It touches only rows older than `STREAM_TIMEOUT` + 1 min, so it never fails a live stream of another replica.

## Event

`dev.synthwerk.llm.message.completed.v1` on `evt.llm.message.completed.v1`, stream `EVT_LLM` (`evt.llm.>`, created or updated by the service at start). Written in the same transaction as the final message (kit outbox), for `completed`, `failed` and `canceled`. `subject` = message id, `orgid` = org, `actor` = `user:<sub>`. Data: ids, model alias, provider, token counts, latency, time to first token, status.

## Config

`SYNTHWERK_LLM_*` (see `cmd/llm/main.go` and `internal/config`): `PROVIDER`, `BASE_URL`, `API_KEY[_FILE]`, `MODEL_ALIASES` (`default=qwen3.5:4b`; the alias `default` must exist), `SYSTEM_PROMPT`, `MAX_INPUT_CHARS` (8000), `MAX_STREAMS_PER_OWNER` (2), `MAX_CONTEXT_CHARS` (24000), `STREAM_TIMEOUT` (120s), `PROVIDER_CONCURRENCY` (1), `QUEUE_TIMEOUT` (10s), `FAKE_DELAY` (0s), `OIDC_ISSUER`, `OIDC_AUDIENCE`, `JWKS_URL`, `DATABASE_URL`, `NATS_URL`, `HTTP_ADDR`.

## Tests

- `tests/unit/chat` — `Send - no valid token (none, malformed, expired, bad signature, other issuer, wrong aud, alg none, HS256, no sub, no org) - 401, no store call, no model call`; `Send - other origin - 403`.
- `tests/integration/chat` (Postgres + NATS in Testcontainers, `go test -tags integration`) — streaming with arrival gaps, user isolation and 404 for another owner or org, RLS without session vars, log and span canaries on success, provider down, mid-stream failure, client abort and 401; 413, 429, abort keeps the partial answer, relay to `EVT_LLM` without text, finish rollback, startup sweep.
- `tests/contract/provider` — conformance of `fake` and `openaicompat`.
- `tests/unit/config`, `tests/unit/sse`.

## Not in this slice

Quotas (#33), eval (#72), Anthropic adapter (E4), `GET /v1/models`, list/delete/resume of conversations, a second DB role for migrations, scope checks (claim names wait for #28/#30).
