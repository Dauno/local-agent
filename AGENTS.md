

## Tool Preferences

*Which CLI tool to use when you shell out. All of these are installed.*
- **Built-in Tools First**: Use the Read, Grep, and Glob tools before Bash. Shell out only when no built-in tool fits
- **Text Search**: Use `rg` instead of `grep`. It is faster and it respects .gitignore
- **Code Structure Search**: Use `ast-grep` to match code patterns such as classes, functions, and interfaces. Call it as `ast-grep`, not `sg`
- **Data Processing**: Use `jq` for JSON. Use `yq` for YAML and XML
- **Text Processing**: Use `sed` to edit streams. Use `awk` to scan patterns


## Code Standards

*Universal principles for writing quality code*
- **KISS**: Keep It Simple. Favor simple, maintainable solutions over clever code
- **YAGNI**: You Ain't Gonna Need It. Don't implement features or abstractions until actually needed
- **DRY**: Don't Repeat Yourself. Extract repeated logic into utility functions
- **Naming**: Use descriptive, self-documenting names. Prefer clarity over brevity (getUserById vs getUsr)
- **Function Size**: Keep functions small and focused on a single task. Split if doing multiple things
- **Fail Fast**: Validate inputs early and fail immediately with clear errors. Don't let invalid data propagate
- **Security**: Never log/commit secrets, validate all inputs, redact sensitive data in logs
- **Imports**: Group (stdlib → third-party → local), sort alphabetically within groups
- **Error Handling**: Handle errors gracefully with meaningful, actionable messages
- **Comments**: Explain "why" decisions were made, not "what" the code does
- **Testing**: Add tests following existing project patterns before marking work complete
- **Changes**: Make minimal, focused changes that solve one problem at a time
- **Read before you write**: Before adding code, read exports, immediate callers, shared utilities. "Looks orthogonal" is dangerous. If unsure why code is structured a way, ask.


## Communication Style

*Preferences for how you talk to me, and for code, comments, and documentation*

### ASD-STE100 (the specification)

Write in Simplified Technical English. This covers chat replies, plan and summary text, code comments, docs, commit messages, and PR descriptions.

Apply the 53 writing rules. Do **not** apply the dictionary of about 900 approved words: it rejects roughly 1,200 common words, which is correct for a maintenance manual and wrong for technical discussion. Apply the word counts to documentation. In chat, keep the same discipline without counting words.

- **Sentence Length**: A procedural sentence has 20 words at most. A descriptive sentence has 25 words at most
- **One Instruction Per Sentence**: One topic per paragraph, six sentences per paragraph at most
- **Verb Forms**: Use the infinitive, the imperative, the simple present, the simple past, the simple future, and the past participle as an adjective. Do not stack auxiliary verbs. The present perfect is not permitted: write "We received the report", not "We have received the report"
- **The "-ing" Form**: Use it only as a technical noun, or as a modifier inside a technical noun
- **Active Voice**: Write "The script reads the config file", not "The config file is read by the script". Use the passive voice only in descriptive text, and only when the agent is unknown
- **Noun Clusters**: Three words at most. "Backup task log file" has four, so write "the log file for the backup task"
- **Keep Sentence Parts**: Do not drop the verb, the subject, or an article to make a sentence shorter. A shorter sentence that loses a part becomes ambiguous
- **One Word, One Meaning**: Explain the same thing the same way. Do not reach for a synonym to add variety
- **Approved Word Pairs**: Prefer "make sure" over "ensure/verify/check/confirm", "start" over "initiate/commence", "use" over "utilize"

Source: <https://www.asd-ste100.org> (Issue 9, 15 January 2025, free to download).

### House rules (NOT ASD-STE100)

These are mine. Do not cite the specification as their source.

- **No Emojis**: Never use emojis in code, comments, commit messages, or documentation
- **No Em or En Dashes**: Never build a sentence with an em dash (—) or an en dash (–). Use a period, a comma, a colon, or parentheses instead. A plain hyphen (-) is fine in compound words and flags
- **Plain English, Not Academic**: Write as if English is your second language. Use short, common words and simple sentence patterns. No academic tone, no rhetorical flourish, no long subordinate clauses. Say the thing in the most direct way
- **Clarity**: Write in clear, direct language without unnecessary embellishment
- **Review First**: When asked to review or analyze something, do that first and report findings before making any changes
- **Humble Language**: Avoid claiming "success" without verification. Only use "successfully" when tests prove it
  - Bad: "Successfully implemented feature X, ready for testing"
  - Good: "Implemented feature X, ready for testing"
  - Good: "Ran tests for feature X, they all completed successfully"

# local-agent — agent instructions

Go 1.25 Slack Socket Mode agent using Google ADK + OpenAI-compatible LLM.

Module path is `github.com/Dauno/slack-local-agent` (not the directory name `local-agent`).

## Build & dev commands

```sh
go build -trimpath -o bin/local-agent ./cmd/local-agent   # production binary
go build -trimpath ./cmd/local-agent                        # verify-only (no -o)
go test ./...                                              # includes architecture dep check
go vet ./...
golangci-lint run
go mod tidy
```

No Makefile or CI workflows. `.golangci.yml` defines the local lint baseline.

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

**Adapters** (25): adkagent, adkartifact, agentcli, codexshim, envfile, filesystem, fsartifact, fsproject, fssandbox, goast, logging, lspclient, lspdiscovery, memoryprojector, metrics, modelcalllimiter, openaillm, openaistt, rangedreader, recoverableresult, slack, sqlite, tokencounter, toolfactory, toolrunner.

**Usecases** (16): agentbuilder, bootstrap, bot, canvas, contextcompiler, contextsummary, doctor, externalagent, generatedfile, knowledge, resultanalysis, results, rollout, sandbox, workpoll, workstream.

**Other internal packages**: `agentdef` (agent/provider YAML definitions, stdlib+yaml.v3 only), `cliprotocol` (stdlib-only `cli-v1` NDJSON wire contract between the `agent_cli` adapter and shim processes), `manifest` (Slack app manifest rendering), `secure` (credential redaction), `cli` (cobra delivery; also hosts the hidden `shim codex` mapper command), `buildinfo` (version metadata), `config` (path resolution).

### Agent CLI provider (`agent_cli`)

- Two provider types: `openai_compatible` and `agent_cli`. The earlier `acp` provider type, its client adapter, and its dedicated use case have been removed. Every external CLI agent now runs through `agent_cli`.
- `agent_cli` providers: `shim.command` (`self` or PATH executable) + `shim.args`; profiles carry `model`, optional `agent`, `approval` (`reject` default | `auto`), `variant`. HTTP fields are rejected.
- `internal/adapter/agentcli` implements ADK `model.LLM` by spawning one shim process per model call: one `cli-v1` NDJSON request on stdin, bounded stdout/stderr, process-group kill on cancellation. Text-only: ADK tools, function history, images, and streaming are rejected before launch.
- The `AcpAgent` agent class is retired: `internal/agentdef` rejects it. Every agent, including durable `agent_cli` leaves, declares `agent_class: LlmAgent`.
- `internal/adapter/codexshim` maps `cli-v1` to `codex exec --json --ephemeral --color never -`. Accepts exactly Codex CLI `0.144.5`; unchanged.
- `confirmation: required` is opt-in per agent, not mandatory for `execution_mode: durable_job`. An `agent_cli` leaf that omits `confirmation` runs its durable job without a Slack approval gate; declare `confirmation: required` on any leaf whose CLI profile can mutate files or run commands. `internal/app/agent_tools.go` (`newAgentCLIDurableTool`) reads this field at tool-build time. The Slack agent-builder wizard (`internal/usecase/agentbuilder/service.go`) no longer forces `confirmation: required` either; a wizard-built `agent_cli` agent also runs without a confirmation gate unless the installed YAML is hand-edited to add it.
- Every run receives the full canonical `sandbox.projects` registry; the app root must be registered. A CLI-backed root gets **no** ADK tool factory.
- An `openai_compatible` root may declare `agent_tools` referencing leaf agents of two forms: `agent_cli` leaves (no ADK tools, native CLI tools only, must omit `tool_scope`; may run `execution_mode: foreground` or `durable_job`, with an opt-in `confirmation: required` gate) and `openai_compatible` leaves that must declare `tool_scope: invocation_scoped` (e.g. `explore`). Scoped leaves receive the same invocation-scoped read-only tools as the root (`list_messages`, `list_repos`, `list_directory`, `read_file`, `list_worktrees`) bound to the trusted Slack actor and conversation key — never mutable tools or confirmations. All children are exposed through ADK `AgentTool`, use isolated in-memory child sessions, receive the root-owned `delegated_global_instruction` safety policy rather than Slack-specific root context, and do not change the durable root provider family.
- `port.AgentToolFactory.ToolsForInvocation` returns `([]any, error)`; a construction failure fails the turn instead of producing a partial tool list. `internal/app/agent_tools.go` prepares child models at startup and composes scoped children per invocation (`compositeAgentToolFactory`).
- Durable sessions are stamped with `local_agent_provider_family` state; startup and each turn fail closed on family mismatch (`init --reset-state` to switch families).

### Durable external-agent result delivery

- The external-agent delivery contract is versioned in schema v30 → v31 → v32
  (see "Completion routes" below). Terminal job CAS and outbox insertion are
  one transaction. Result-delivery fields introduced at v22 remain keyed by
  `(job_id, status_revision, kind)`; pre-v22 rows remain `legacy_v1` and are
  never replayed or regenerated.
- Durable external-agent results are redacted and control-sanitized before
  mode selection. Results use complete `markdown_v1` multipart delivery up to
  `external_agent.delivery.max_markdown_parts` (1-8), otherwise use a private
  verified `.md` artifact uploaded to the originating thread. No second
  confirmation is created.
- Result artifacts use bounded 0600 files, opaque references, verified owner
  and SHA-256 reads, and retention skips unpublished delivery references. Raw
  external-agent artifacts are never uploaded.
- `externalagent.NotificationWorker` is independent from execution leases. It
  claims `pending`, stale `publishing`, or `unknown` rows and marks publication
  only with owner/attempt CAS. Workstream job admission stores the association
  only on `workstream_tasks.job_id`; jobs do not persist workstream keys. Restart and ambiguous Slack results reconcile
  deterministic metadata before retry; raw result content, artifact paths,
  upload URLs, and provider errors are not logged.
- `internal/adapter/slack.JobNotificationPublisher` uses the existing Markdown
  splitter and external upload transport, with metadata for job ID, status
  revision, kind, mode, policy, whole-result digest, part digest, and file ID.
  Recovered Slack timestamps are persisted; file upload state is persisted
  through URL request, byte upload, completion, and reconciliation.
- `completion_unknown` never replays the original task. `Service.Status`,
  `CancelForConversation`, and `Reconcile` require actor/conversation binding;
  reconciliation uses capability-negotiated external-agent sessions and
  remains actionable when the provider cannot load/resume.

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

`.local-agent/` is gitignored. It contains config, state, generated files, memory, and local YAML definitions.

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
- **Schema**: `PRAGMA user_version` for SQLite migrations. Current version: 47 (external-agent contract chain: v30 → v31 → v32 → v45; workstream-owned job admission in v46 → v47).
- **Memory**: curated entity memory stored in SQLite; `.local-agent/memory/` holds OKF file projections. Memory retrieval is deterministic (no LLM routing) and runs before each model call. Memory failure is non-fatal.
- **Ephemeral context**: Slack enrichment and memory snippets are injected per-turn via the user message text; they must never become durable ADK events.
- **Sandbox**: workspace inspection is enabled by default for the registered application root through `sandbox.enabled` and `sandbox.projects`; `list_directory` is non-recursive and blocks `.env` and `.git` at every depth (including symlinks).
- **External-agent artifacts**: private result artifacts live under `<state.dir>/artifacts`, use bounded 0600 files, verified owner/digest reads, and are cleaned by `external_agent.artifact_retention_days` only when no unpublished delivery references them. Cleanup is non-recursive and never follows symlinks. Offline doctor checks the artifact directory, delivery policy, and v32 job/outbox fields without reading result content or secrets. Durable external-agent file fallback requires Slack `files:write`.
- **Worktrees**: new worktrees live under `.worktrees/<name>` relative to the repo root (gitignored). Use the repo alias `git wtadd <name> [git-worktree-add args...]`, which resolves to `git worktree add .worktrees/<name> <args...>`.
