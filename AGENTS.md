# local-agent — agent instructions

Go 1.25 Slack Socket Mode agent using Google ADK + OpenAI-compatible LLM.

Module path is `github.com/Dauno/slack-local-agent` (not the directory name `local-agent`).

## Build & dev commands

```sh
go build -trimpath -o bin/local-agent ./cmd/local-agent   # production binary
go build -trimpath ./cmd/local-agent                        # verify-only (no -o)
go test ./...                                              # includes architecture dep check
go vet ./...
go mod tidy
```

No Makefile, no CI workflows, no `.golangci.yml`. `go vet` is the only lint.

## Commands

| Command | Notes |
|---------|-------|
| `bin/local-agent init` | wizard; creates artifacts + guides setup |
| `bin/local-agent init --reset-state` | destructive: deletes `.local-agent/local-agent.db` and `memory/` projections |
| `bin/local-agent doctor` | offline only; `--live` adds Slack + model checks |
| `bin/local-agent run` | requires `init` first (never bootstraps) |
| `bin/local-agent manifest [--write]` | renders Slack manifest |
| `bin/local-agent version` | build info |
| `bin/local-agent shim codex` | hidden; cli-v1 mapper for Codex CLI (same NDJSON contract) |

## Architecture

Hexagonal. Strict dependency rules enforced by `internal/architecture/dependencies_test.go`:

| Layer | Owns | Must not own |
|-------|------|--------------|
| `internal/domain` | stdlib only. Pure data + policy. | ADK, OpenAI, Slack, SQLite, Docker types. |
| `internal/port` | domain + stdlib. Shared interfaces. | Framework or transport implementations. |
| `internal/usecase` | domain + port. Business logic. | Adapters or third-party SDKs. |
| `internal/adapter` | Concrete implementations. | Must not import other adapters (composed in `internal/app`). |
| `internal/app` | Composition root. | Must not import CLI layer. |

**Adapters** (16): acpclient, adkagent, adkartifact, agentcli, codexshim, envfile, fsproject, fssandbox, logging, memorycurator, memoryprojector, modelcalllimiter, openaillm, slack, sqlite, toolfactory.

**Usecases** (6): bootstrap, bot, doctor, memory, opencode, sandbox.

**Other internal packages**: `agentdef` (agent/provider YAML definitions, stdlib+yaml.v3 only), `cliprotocol` (stdlib-only `cli-v1` NDJSON wire contract between the `agent_cli` adapter and shim processes), `manifest` (Slack app manifest rendering), `secure` (credential redaction), `cli` (cobra delivery; also hosts the hidden `shim codex` mapper command), `buildinfo` (version metadata), `config` (path resolution).

### Agent CLI provider (`agent_cli`) and ACP agent (`acp`)

- Three provider types: `openai_compatible`, `agent_cli`, and `acp` (for OpenCode via Agent Client Protocol).
- `agent_cli` providers: `shim.command` (`self` or PATH executable) + `shim.args`; profiles carry `model`, optional `agent`, `approval` (`reject` default | `auto`), `variant`. HTTP fields are rejected.
- `acp` providers: `command` + `args` (e.g., `opencode acp`); profiles carry `model` + `config_options` (ACP session config IDs) + `permission_option_kind` (`reject_once` or `allow_once`). HTTP fields and `shim` are rejected for `acp`.
- `internal/adapter/agentcli` implements ADK `model.LLM` by spawning one shim process per model call: one `cli-v1` NDJSON request on stdin, bounded stdout/stderr, process-group kill on cancellation. Text-only: ADK tools, function history, images, and streaming are rejected before launch.
- `internal/adapter/acpclient` implements `port.ExternalAgentRuntime` by spawning `opencode acp` for ACP v1 JSON-RPC over stdio: initialize, session/new, set_config_option, prompt, and close per invocation. It negotiates optional `loadSession`/`session/resume` for bounded reconciliation; absent support returns an actionable typed failure.
- OpenCode is now an external ACP agent, not a version-pinned CLI shim. ACP profiles use direct session config option IDs (`model`, `effort`, `mode`). `openableshim` adapter has been removed.
- `AcpAgent` agent class: declarative YAML with `runtime: opencode/profile-name` and `confirmation: required`. Becomes a typed ADK FunctionTool with structured `project`/`task` arguments. Each invocation is bound to exactly one registered project; ACP requests never send `additionalDirectories`. Uses `port.ExternalAgentRuntime` for invocation.
- `internal/adapter/codexshim` maps `cli-v1` to `codex exec --json --ephemeral --color never -`. Accepts exactly Codex CLI `0.144.5`; unchanged.
- Every run receives the full canonical `sandbox.projects` registry; the app root must be registered. A CLI-backed root gets **no** ADK tool factory.
- An `openai_compatible` root may declare `agent_tools` referencing leaf agents of three forms: `agent_cli` leaves (no ADK tools, native CLI tools only, must omit `tool_scope`), `openai_compatible` leaves that must declare `tool_scope: invocation_scoped` (e.g. `explore`), and `AcpAgent` leaves (external ACP agents with structured `project`/`task` arguments and required confirmation). Scoped leaves receive the same invocation-scoped read-only tools as the root (`list_messages`, `list_repos`, `list_directory`, `read_file`, `list_worktrees`) bound to the trusted Slack actor and conversation key — never mutable tools or confirmations. All children are exposed through ADK `AgentTool`, use isolated in-memory child sessions, receive the root-owned `delegated_global_instruction` safety policy rather than Slack-specific root context, and do not change the durable root provider family.
- `port.AgentToolFactory.ToolsForInvocation` returns `([]any, error)`; a construction failure fails the turn instead of producing a partial tool list. `internal/app/agent_tools.go` prepares child models at startup and composes scoped children per invocation (`compositeAgentToolFactory`).
- Durable sessions are stamped with `local_agent_provider_family` state; startup and each turn fail closed on family mismatch (`init --reset-state` to switch families).
- Foreground ACP calls composed with the durable job service use a synchronous compatibility facade. Worker calls carry `JobID` and bypass the facade to prevent recursion; probes and management retain direct ACP clients.

### Durable external-agent result delivery

- The external-agent delivery contract is versioned in schema v30 → v31 → v32
  (see "Completion routes" below). Terminal job CAS and outbox insertion are
  one transaction. Result-delivery fields introduced at v22 remain keyed by
  `(job_id, status_revision, kind)`; pre-v22 rows remain `legacy_v1` and are
  never replayed or regenerated.
- Durable ACP results are redacted and control-sanitized before mode selection.
  Results use complete `markdown_v1` multipart delivery up to
  `acp.delivery.max_markdown_parts` (1-8), otherwise use a private verified
  `.md` artifact uploaded to the originating thread. No second confirmation is
  created.
- Result artifacts use bounded 0600 files, opaque references, verified owner
  and SHA-256 reads, and retention skips unpublished delivery references. Raw
  ACP artifacts are never uploaded.
- `externalagent.NotificationWorker` is independent from execution leases. It
  claims `pending`, stale `publishing`, or `unknown` rows and marks publication
  only with owner/attempt CAS. Restart and ambiguous Slack results reconcile
  deterministic metadata before retry; raw result content, artifact paths,
  upload URLs, and provider errors are not logged.
- `internal/adapter/slack.JobNotificationPublisher` uses the existing Markdown
  splitter and external upload transport, with metadata for job ID, status
  revision, kind, mode, policy, whole-result digest, part digest, and file ID.
  Recovered Slack timestamps are persisted; file upload state is persisted
  through URL request, byte upload, completion, and reconciliation.
- `completion_unknown` never replays the original task. `Service.Status`,
  `CancelForConversation`, and `Reconcile` require actor/conversation binding;
  reconciliation uses capability-negotiated ACP sessions and remains
  actionable when the provider cannot load/resume.

### Completion routes

Execution mode decides the terminal completion route. Root activation exists
**only** for detached jobs; no terminal job state, cancellation path, or
recovery ever activates the root for a foreground job.

- `foreground` — synchronous: the durable result verified against its
  identity (final redacted and control-sanitized text, exact UTF-8 bytes, and
  SHA-256 computed over those final bytes) is returned through the original
  tool response, producing exactly one root response. The terminal
  notification is still published, but it never creates a root activation.
- `detached` — notification + activation: `accepted` + job ID, then a durable
  terminal notification, then root activation (only when
  `root_activation_required = 1` and `j.mode = 'detached'`), then verified
  chunk reads, then a root synthesis.

Result identity is complete since v32: notification identity
(`notification_sha256` / `notification_bytes`) is computed over canonical
Markdown; result identity (`result_sha256` / `result_bytes`) over the complete
sanitized result. No field changes meaning depending on policy or mode.

Schema versions for this contract: v30 was the pre-fix baseline; v31 (P0)
repairs historical foreground inline identity and retires foreground
activations while preserving terminal rows as audit; v32 (P1) persists the
explicit completion route (`root_activation_required`) and the full
notification/result identities, with `j.mode = 'detached'` kept as defense in
depth.

### ADK durable runtime

The agent uses **durable ADK sessions** backed by SQLite. Key types:

- `port.AgentRuntime`: `Run(ctx, req) (AgentTurn, error)` / `Resume(ctx, decision) (AgentTurn, error)`
- `port.AgentTurn` carries `Text` and optional `*PendingConfirmation`.
- `adkagent.Runtime` constructs per-turn `llmagent` with tools from `AgentToolFactory`. Session IDs: `adk:{conversation-key}`.
- `adaptersqlite.AdkSessionService` implements ADK's `session.Service` using `database/sql` (no GORM).
- Backward compat: `port.Agent.Respond` still wired in `internal/app/run.go`. Bot use case branches: `runtime != nil` → `handleRuntimeTurn()`, else legacy path.

### Confirmation flow

1. Model emits `FunctionCall` → ADK detects `RequireConfirmation: true` → emits `adk_request_confirmation` wrapper
2. `adkagent.Runtime` extracts `PendingConfirmation` from the wrapper event
3. Bot use case creates `ConfirmationDelivery` in SQLite, publishes Slack prompt
4. User replies `approve <id>` / `reject <id>` → `HandleConfirmation`
5. `HandleConfirmation` validates actor, expiry, status (not consumed), marks consumed atomically, calls `runtime.Resume()`
6. Replay protection: `MarkConsumed` rejects duplicate approvals

### Slack Markdown delivery

- All `ResponsePublisher` text is standard Markdown, sent with `chat.postMessage.markdown_text`; no top-level `text` or app-generated blocks.
- `internal/adapter/slack` owns control-sequence neutralization and deterministic splitting at 11,900 Unicode code points, including multipart labels.
- Renderer `markdown_v1` metadata contains correlation ID, one-based part index, part count, and submitted-part SHA-256 digest.
- Recovery reconstructs parts from canonical sanitized content and fails closed on missing, duplicate, reordered, edited, or inconsistent parts.
- Upgrades across renderer formats require `init --reset-state`; `run` never performs a destructive migration.

## Data directory

`.local-agent/` is mostly gitignored. Contains: `config.yaml`, `local-agent.db` (SQLite), `app-manifest.local.yaml`, `local.env.example`, and `memory/` (OKF file projections). Exceptions: `agents/` and `providers/` subdirs hold YAML definitions and are tracked in git.

`docs/` is gitignored but contains authoritative TRDs — prefer those over guessing architecture.

## Testing

- Tests use local fakes: temp SQLite, HTTP test servers, injected in-memory stores. No live credentials needed.
- `go test ./...` runs everything, including the architecture dependency check.
- Integration tests (`internal/integration`) wire real adapters with temp SQLite; no build tags needed.

## Key conventions

- **Secrets** go in `.env` (0600). **Config** goes in `.local-agent/config.yaml`.
- **Redaction**: `internal/secure.Redactor` strips credentials from logs/errors/output at the last mile.
- **Context limits**: count Unicode code points, not bytes or rune length.
- **Dedupe**: at-most-once by event + message keys. Ephemeral Slack history recovery is not persisted.
- **Canonical keys**: `slack:{team}:dm:{channel}` or `slack:{team}:channel:{channel}:thread:{root_ts}`.
- **ADK session IDs**: `adk:{canonical-conversation-key}` — deterministic, opaque, never derived from untrusted text.
- **Schema**: `PRAGMA user_version` for SQLite migrations. Current version: 32 (external-agent contract chain: v30 → v31 → v32).
- **Memory**: curated entity memory stored in SQLite; `.local-agent/memory/` holds OKF file projections. Memory retrieval is deterministic (no LLM routing) and runs before each model call. Memory failure is non-fatal.
- **Ephemeral context**: Slack enrichment and memory snippets are injected per-turn via the user message text; they must never become durable ADK events.
- **Sandbox**: workspace inspection is enabled by default for the registered application root through `sandbox.enabled` and `sandbox.projects`; `list_directory` is non-recursive and blocks `.env` and `.git` at every depth (including symlinks).
- **ACP artifacts**: private result artifacts live under `<state.dir>/artifacts`, use bounded 0600 files, verified owner/digest reads, and are cleaned by `acp.artifact_retention_days` only when no unpublished delivery references them. Cleanup is non-recursive and never follows symlinks. Offline doctor checks the artifact directory, delivery policy, and v32 job/outbox fields without reading result content or secrets. Durable ACP file fallback requires Slack `files:write`.
- **Worktrees**: new worktrees live under `.worktrees/<name>` relative to the repo root (gitignored). Use the repo alias `git wtadd <name> [git-worktree-add args...]`, which resolves to `git worktree add .worktrees/<name> <args...>`.

## OpenCode config

`.opencode/opencode.json` enables `lsp: true` (Go gopls), connects to ADK docs via MCP server, and references external instruction files (`caveman.md`, `soul-rules.md`) that apply to sessions in this repo. Skills directory has 7 Google ADK skills. No repo-local agents configured.

OpenCode is integrated via ACP (Agent Client Protocol) through `opencode acp`. Provider YAML in `.local-agent/providers/opencode.yaml` uses `type: acp` with `command: opencode` and `args: [acp]`. OpenCode management operators (for upgrade/rollback) are configured via `opencode.management.allowed_user_ids` in `.local-agent/config.yaml`. `openableshim` adapter has been removed.
