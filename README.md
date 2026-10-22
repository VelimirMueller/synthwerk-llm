<p>
<picture>
  <source media="(prefers-color-scheme: light)" srcset="assets/banner/hero-v2-light.svg">
  <img alt="SYNTHWERK-LLM. LLM gateway. Local models in dev, any provider in prod. Scoped, audited SSE streaming. Rewrite." src="assets/banner/hero-v2-dark.svg" width="100%">
</picture>
</p>

<p>
<picture>
  <source media="(prefers-color-scheme: light)" srcset="assets/readme/lead-v2-light.svg">
  <img alt="synthwerk-llm is the LLM gateway for the Synthwerk family. One API for chat and text tasks, scoped access, audited streaming. This repo is being rewritten. There is no code on main. It is very good at waiting." src="assets/readme/lead-v2-dark.svg" width="100%">
</picture>
</p>

<p>
<picture>
  <source media="(prefers-color-scheme: light)" srcset="assets/readme/stats-v2-light.svg">
  <img alt="1 API, CHAT AND TEXT. 3 SCOPES: ADMIN, TENANT, USER. 2 PROVIDERS NAMED. 0 CODE ON MAIN" src="assets/readme/stats-v2-dark.svg" width="100%">
</picture>
</p>

<br>

<p>
<img alt="01 WHAT IT DOES. ONE API. EVERY PROVIDER." src="assets/readme/divider-what-v2.svg" width="100%">
</p>

<p>
<picture>
  <source media="(prefers-color-scheme: light)" srcset="assets/readme/features-v2-light.svg">
  <img alt="ONE API: Chat and text tasks behind one endpoint. The provider is chosen by env: Ollama locally, Anthropic or others in prod. SCOPES: platform-admin, tenant, user. Services enforce the scope on the token, never the prompt. AUDITED STREAM: Tokens stream over SSE. Every answer and tool call is audited" src="assets/readme/features-v2-dark.svg" width="100%">
</picture>
</p>

<p>
<picture>
  <source media="(prefers-color-scheme: light)" srcset="assets/readme/what-v2-light.svg">
  <img alt="One API: Chat and text tasks. The provider is chosen by env: Ollama in dev, Anthropic or any provider in prod. Streamed and audited: Tokens stream over SSE. Every answer and tool call is audited. Scoped on the token: Scopes are platform-admin, tenant and user. The service enforces the scope on the token, never the prompt" src="assets/readme/what-v2-dark.svg" width="100%">
</picture>
</p>

<br>

<p>
<img alt="02 QUICK START. NOTHING TO RUN. YET." src="assets/readme/divider-start-v2.svg" width="100%">
</p>

<p>
<picture>
  <source media="(prefers-color-scheme: light)" srcset="assets/readme/start-v2-light.svg">
  <img alt="Terminal: $ gh repo clone VelimirMueller/synthwerk-llm | $ cd synthwerk-llm | $ git checkout legacy-final | # the old Flask + GPT4All WebSocket demo" src="assets/readme/start-v2-dark.svg" width="100%">
</picture>
</p>

<details>
<summary><picture><source media="(prefers-color-scheme: light)" srcset="assets/readme/links-6-v2-light.svg"><img alt="Plain text" src="assets/readme/links-6-v2-dark.svg" width="32%"></picture></summary>

```bash
gh repo clone VelimirMueller/synthwerk-llm
cd synthwerk-llm
git checkout legacy-final   # the old Flask + GPT4All WebSocket demo
```

</details>

<p>
<picture>
  <source media="(prefers-color-scheme: light)" srcset="assets/readme/startnotes-v2-light.svg">
  <img alt="No code on main: There is no code to run on main yet. Tag legacy-final holds the old demo. Ecosystem map: The ecosystem map is in synthwerk (https://github.com/VelimirMueller/synthwerk). Blueprint: Shared CI, lint configs and templates come from synthwerk-blueprint (https://github.com/VelimirMueller/synthwerk-blueprint)" src="assets/readme/startnotes-v2-dark.svg" width="100%">
</picture>
</p>

<p align="center">
<a href="https://github.com/VelimirMueller/synthwerk"><picture><source media="(prefers-color-scheme: light)" srcset="assets/readme/links-1-v2-light.svg"><img alt="synthwerk (https://github.com/VelimirMueller/synthwerk)" src="assets/readme/links-1-v2-dark.svg" width="32%"></picture></a>
<a href="https://github.com/VelimirMueller/synthwerk-blueprint"><picture><source media="(prefers-color-scheme: light)" srcset="assets/readme/links-2-v2-light.svg"><img alt="Blueprint (https://github.com/VelimirMueller/synthwerk-blueprint)" src="assets/readme/links-2-v2-dark.svg" width="32%"></picture></a>
<a href="https://github.com/VelimirMueller/synthwerk-llm/tree/legacy-final"><picture><source media="(prefers-color-scheme: light)" srcset="assets/readme/links-3-v2-light.svg"><img alt="legacy-final (https://github.com/VelimirMueller/synthwerk-llm/tree/legacy-final)" src="assets/readme/links-3-v2-dark.svg" width="32%"></picture></a>
</p>

## Run locally

```bash
just test
```

The service reads its configuration from the environment:

| Variable | Default | Description |
| --- | --- | --- |
| `SYNTHWERK_PLATFORM_ENV` | `local` | `local`, `ci`, `dev`, `stg`, `prd` |
| `SYNTHWERK_LLM_HTTP_ADDR` | `:8080` | Listen address |
| `SYNTHWERK_LLM_DATABASE_URL` | — | Postgres URL (required) |
| `SYNTHWERK_LLM_LOG_LEVEL` | `INFO` | `DEBUG`, `INFO`, `WARN`, `ERROR` |
| `SYNTHWERK_LLM_LOG_FORMAT` | `json` | `json` or `console` (local only) |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | — | OTLP gRPC collector. Unset = no export |

Start it against a running Postgres:

```bash
export SYNTHWERK_LLM_DATABASE_URL=postgres://user:pass@127.0.0.1:5432/llm
go run ./cmd/llm
```

`GET /healthz` is the liveness probe; `GET /readyz` is the readiness probe and pings Postgres.

<br>

<p>
<img alt="03 HOW IT WORKS. IN ONE SIDE, OUT THE OTHER." src="assets/readme/divider-how-v2.svg" width="100%">
</p>

<p>
<picture>
  <source media="(prefers-color-scheme: light)" srcset="assets/readme/flow-v2-light.svg">
  <img alt="CLIENTS -&gt; LLM GATEWAY -&gt; PROVIDERS -&gt; NATS BUS. The gateway also reads the other services through read-only MCP tools." src="assets/readme/flow-v2-dark.svg" width="100%">
</picture>
</p>

<p>
<picture>
  <source media="(prefers-color-scheme: light)" srcset="assets/readme/hownotes-v2-light.svg">
  <img alt="Clients in: synthwerk-widgets and SDK apps call the gateway over HTTP. Provider out: The gateway forwards to Ollama, Anthropic or another provider, chosen by env. Read-only MCP: The gateway reads the other services through read-only MCP tools. It never writes. Events out: Facts go to the NATS bus as events" src="assets/readme/hownotes-v2-dark.svg" width="100%">
</picture>
</p>

<p>
<picture>
  <source media="(prefers-color-scheme: light)" srcset="assets/readme/branching-v2-light.svg">
  <img alt="FEAT/* -&gt; MAIN -&gt; DEV -&gt; STG -&gt; PRD. Trunk-based. No env branches. The digest is promoted by approval." src="assets/readme/branching-v2-dark.svg" width="100%">
</picture>
</p>

<br>

<p>
<img alt="04 USAGE. THE REFERENCE. CONDENSED." src="assets/readme/divider-usage-v2.svg" width="100%">
</p>

<p>
<picture>
  <source media="(prefers-color-scheme: light)" srcset="assets/readme/usage-v2-light.svg">
  <img alt="Scopes: platform-admin, tenant, user. Enforced on the token, never the prompt. Where it fits: synthwerk-widgets and SDK apps call the gateway. It reads service tools through read-only MCP tools and publishes events to the NATS bus. Ecosystem: Ecosystem map: synthwerk (https://github.com/VelimirMueller/synthwerk). Shared CI: synthwerk-blueprint (https://github.com/VelimirMueller/synthwerk-blueprint). The old code: Tag legacy-final holds a Flask + GPT4All WebSocket demo, one shared session for all users. Renamed: This repo was renamed. The old URL still redirects here. Branching: main deploys to dev, a vX.Y.Z tag to stg, an approved digest to prd" src="assets/readme/usage-v2-dark.svg" width="100%">
</picture>
</p>

<br>

<p>
<img alt="05 STATUS. HONEST. NO CODE YET." src="assets/readme/divider-status-v2.svg" width="100%">
</p>

<p>
<picture>
  <source media="(prefers-color-scheme: light)" srcset="assets/readme/status-v2-light.svg">
  <img alt="Rewrite: planned, epic E3. Old code: tag legacy-final. Providers: Ollama, Anthropic, …. Streaming: SSE. Branching: main→dev, tag→stg, digest→prd" src="assets/readme/status-v2-dark.svg" width="100%">
</picture>
</p>

<p>
<picture>
  <source media="(prefers-color-scheme: light)" srcset="assets/readme/honest-v2-light.svg">
  <img alt="Tests: There are no tests. There is no code to test. Changelog: There is no CHANGELOG yet. Stack: Go 1.27. Licensed under MIT" src="assets/readme/honest-v2-dark.svg" width="100%">
</picture>
</p>

<p align="center">
<a href="LICENSE"><picture><source media="(prefers-color-scheme: light)" srcset="assets/readme/links-4-v2-light.svg"><img alt="License (LICENSE)" src="assets/readme/links-4-v2-dark.svg" width="32%"></picture></a>
<a href="https://github.com/VelimirMueller"><picture><source media="(prefers-color-scheme: light)" srcset="assets/readme/links-5-v2-light.svg"><img alt="VM. flagship (https://github.com/VelimirMueller)" src="assets/readme/links-5-v2-dark.svg" width="32%"></picture></a>
</p>

<p>
<picture>
  <source media="(prefers-color-scheme: light)" srcset="assets/readme/footer-v2-light.svg">
  <img alt="VM. studio / flagship · open source · look per vm-brand playbook · MIT © 2026 Velimir Mueller · -- EOF -- no code. yet. --" src="assets/readme/footer-v2-dark.svg" width="100%">
</picture>
</p>
