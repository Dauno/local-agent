# local-agent — agent instructions

Go 1.25 Slack Socket Mode agent using Google ADK + OpenAI-compatible LLM.

## Build & dev commands

```sh
go build -trimpath -o bin/local-agent ./cmd/local-agent   # production binary
go build -trimpath ./cmd/local-agent                        # verify-only (no -o)
go test ./...                                              # includes architecture dep check
go vet ./...
go mod tidy
```

No Makefile, no CI workflows, no `.golangci.yml`. `go vet` is the only lint.

Release metadata: `-ldflags "-X github.com/Dauno/slack-local-agent/internal/buildinfo.Version=vX.Y.Z -X ...Commit=... -X ...Date=..."`

## Commands

| Command | Notes |
|---------|-------|
| `bin/local-agent init` | wizard; creates artifacts + guides setup |
| `bin/local-agent init --reset-state` | destructive: deletes `.local-agent/local-agent.db` and `memory/` projections |
| `bin/local-agent doctor` | offline only; `--live` adds Slack + model checks |
| `bin/local-agent run` | requires `init` first (never bootstraps) |
| `bin/local-agent manifest [--write]` | renders Slack manifest |
| `bin/local-agent version` | build info |

## Architecture

Hexagonal. Strict dependency rules enforced by `internal/architecture/dependencies_test.go`:

| Layer | Owns | Must not own |
|-------|------|--------------|
| `internal/domain` | stdlib only. Pure data + policy. | ADK, OpenAI, Slack, SQLite, Docker types. |
| `internal/port` | domain + stdlib. Shared interfaces. | Framework or transport implementations. |
| `internal/usecase` | domain + port. Business logic. | Adapters or third-party SDKs. |
| `internal/adapter` | Concrete implementations. | Must not import other adapters (composed in `internal/app`). |
| `internal/app` | Composition root. | Must not import CLI layer. |

**Adapters** (13): adkagent, adkartifact, envfile, fsproject, fssandbox, logging, memorycurator, memoryprojector, modelcalllimiter, openaillm, slack, sqlite, toolfactory.

**Usecases** (5): bootstrap, bot, doctor, memory, sandbox.

**Other internal packages**: `agentdef` (agent/provider YAML definitions, stdlib+yaml.v3 only), `manifest` (Slack app manifest rendering), `secure` (credential redaction), `cli` (cobra delivery), `buildinfo` (version metadata).

### ADK durable runtime (post-TRD)

The agent uses **durable ADK sessions** backed by SQLite. Key types:

- `port.AgentRuntime` (replaces ephemeral `port.Agent.Respond`): `Run(ctx, req) (AgentTurn, error)` / `Resume(ctx, decision) (AgentTurn, error)`
- `port.AgentTurn` carries `Text` and optional `*PendingConfirmation` when a mutable tool requires approval.
- `adkagent.Runtime` constructs per-turn `llmagent` with tools from `AgentToolFactory`. Session IDs are deterministic: `adk:{conversation-key}`.
- `adaptersqlite.AdkSessionService` implements ADK's `session.Service` using `database/sql` (no GORM).
- Schema v10 adds: `adk_sessions`, `adk_events`, `adk_app_state`, `adk_user_state`, `tool_confirmation_deliveries`, `tool_execution_audit`.

Backward compat: `port.Agent.Respond` still wired in `run.go`. The bot use case branches: `runtime != nil` → `handleRuntimeTurn()`, else legacy path.

### Confirmation flow

1. Model emits `FunctionCall` → ADK detects `RequireConfirmation: true` → emits `adk_request_confirmation` wrapper
2. `adkagent.Runtime` extracts `PendingConfirmation` from the wrapper event
3. Bot use case creates `ConfirmationDelivery` in SQLite, publishes Slack prompt
4. User replies `approve <id>` / `reject <id>` → `tryResumeConfirmation` → `HandleConfirmation`
5. `HandleConfirmation` validates actor, expiry, status (not consumed), marks consumed atomically, calls `runtime.Resume()`
6. Replay protection: `MarkConsumed` rejects duplicate approvals

### Slack Markdown delivery

- All `ResponsePublisher` text is standard Markdown and is sent with
  `chat.postMessage.markdown_text`; no top-level `text` or app-generated blocks.
- `internal/adapter/slack` owns control-sequence neutralization and deterministic
  splitting at 11,900 Unicode code points, including multipart labels.
- Renderer `markdown_v1` metadata contains correlation ID, one-based part index,
  part count, and submitted-part SHA-256 digest.
- Recovery reconstructs `markdown_v1` parts from canonical sanitized content and
  fails closed on missing, duplicate, reordered, edited, or inconsistent parts.
- Upgrades and rollbacks across renderer formats require operator-run
  `init --reset-state`; `run` never performs a destructive migration.

### OpenAI function calling

`openaillm` adapter translates:
- ADK `FunctionDeclaration` → Chat Completions `tools` array
- Model `FunctionCall` part → assistant message with `tool_calls`
- User `FunctionResponse` part → `tool` message with `tool_call_id`
- `FunctionCallingConfigModeAuto` → `tool_choice: "auto"`, `Any` → `"required"`
- Response `tool_calls` → `genai.FunctionCall` parts with `FinishReasonStop`
- `parallel_tool_calls: false` is a provider hint, not a security control

### Sandbox capabilities

- `internal/domain/sandbox.go`: `Capability` enum (7 types), `ToolAuditRecord`, `ToolLifecycleState`
- `internal/usecase/sandbox/service.go`: validates capability, checks idempotency via `GetAuditByCallID`, delegates to `SandboxExecutor`
- `internal/adapter/sqlite/sandbox_audit.go`: audit over `tool_execution_audit` table
- `internal/adapter/fssandbox/sandbox.go`: filesystem executor (`list_repos`, `list_directory`, `read_file`, `list_worktrees`)
- `internal/adapter/toolfactory/toolfactory.go`: exposes 4 read-only sandbox tools when sandbox is enabled
- Workspace inspection is opt-in through `sandbox.enabled` and `sandbox.projects`; relative roots resolve against the canonical application root
- `list_directory` is non-recursive; `.env`, `.local-agent`, and `.git` are blocked at every depth, including through symlinks

## Data directory

`.local-agent/` is mostly gitignored. Contains: `config.yaml`, `local-agent.db` (SQLite), `app-manifest.local.yaml`, `local.env.example`, and `memory/` (OKF file projections). Exceptions: `agents/` and `providers/` subdirs hold YAML definitions and are tracked in git.

## Testing quirks

- Tests use only local fakes: temp SQLite, HTTP test servers, injected in-memory stores. No live credentials needed.
- `go test ./...` runs all tests, including the architecture dependency check.
- Domain tests: table-driven, package-internal (`package domain`).
- CLI tests inject streams + fakes.
- Integration tests (`internal/integration`) wire real adapters with temp SQLite; no build tags needed.
- Spike tests for tool calling (`internal/integration/adk_tool_spike_test.go`) use HTTP test servers simulating OpenAI Chat Completions.

## Key conventions

- **Secrets** go in `.env` (0600). **Config** goes in `.local-agent/config.yaml`.
- **Redaction**: `internal/secure.Redactor` (via `secure.NewRedactor(secrets...)`) strips credentials from logs/errors/output at the last mile.
- **Context limits**: count Unicode code points, not bytes or rune length.
- **Dedupe**: at-most-once by event + message keys. Ephemeral Slack history recovery is not persisted.
- **Canonical keys**: `slack:{team}:dm:{channel}` or `slack:{team}:channel:{channel}:thread:{root_ts}`.
- **ADK session IDs**: `adk:{canonical-conversation-key}` — deterministic, opaque, never derived from untrusted text.
- **Schema**: `PRAGMA user_version` for SQLite migrations. Current version: 10.
- **Memory**: curated entity memory stored in SQLite; `.local-agent/memory/` holds OKF file projections. Memory retrieval is deterministic (no LLM routing) and runs before each model call. Memory failure is non-fatal.
- **Ephemeral context**: Slack enrichment and memory snippets are injected per-turn via the user message text; they must never become durable ADK events.
- **Confirmation delivery**: `tool_confirmation_deliveries` bridges ADK confirmation events to Slack. Statuses: pending → published → approved/rejected/expired/consumed.

## OpenCode config

`.opencode/opencode.json` enables `lsp: true` (Go gopls) and connects to ADK docs via MCP server. Skills directory has 7 Google ADK skills (scaffold, code, deploy, eval, observability, publish, workflow). No repo-local agents configured.
