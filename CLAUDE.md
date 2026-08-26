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


# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

`local-agent` is a local-first Slack bot in Go. It connects via Slack Socket Mode, uses Google ADK for
agent orchestration, calls an OpenAI-compatible Chat Completions endpoint (or spawns CLI/ACP agent
providers), and persists conversation state in a project-local SQLite database.

**Read `AGENTS.md` first.** It is the authoritative, actively-maintained deep-dive on architecture,
durable-runtime internals, the external-agent result-delivery contract, confirmation flow, and Slack
Markdown delivery. This file only adds the command reference and orientation notes that don't belong
there.

Module path is `github.com/Dauno/slack-local-agent` (not the directory name `local-agent`).

## Build & test

```sh
go build -trimpath -o bin/local-agent ./cmd/local-agent   # production binary
go build -trimpath ./cmd/local-agent                        # verify-only (no -o)
go test ./...                                              # includes architecture dependency check
go vet ./...
go mod tidy
```

Run a single package or test:

```sh
go test ./internal/usecase/workstream/...
go test ./internal/adapter/sqlite/... -run TestExternalAgentJobStore -v
```

No Makefile or CI workflows. `go vet` plus `.golangci.yml` define lint. The configuration owns the exact
linter list, security rules, formatter list, and the initial `gocyclo` complexity baseline.

## CLI commands

| Command | Notes |
|---------|-------|
| `bin/local-agent init` | wizard; creates artifacts + guides Slack/model setup |
| `bin/local-agent init --reset-state` | destructive: deletes `.local-agent/local-agent.db` and `memory/` projections |
| `bin/local-agent doctor` | offline only; `--live` adds Slack + model checks |
| `bin/local-agent run` | requires `init` first (never bootstraps) |
| `bin/local-agent manifest [--write]` | renders Slack app manifest |
| `bin/local-agent version` | build info |
| `bin/local-agent shim codex` | hidden; cli-v1 mapper for Codex CLI |

## Architecture

Hexagonal, with dependency direction enforced by `internal/architecture/dependencies_test.go` (runs as
part of `go test ./...`, parses imports via `go/ast`, fails the build on a violation):

| Layer | Owns | Must not own |
|-------|------|--------------|
| `internal/domain` | stdlib only. Pure data + policy. | ADK, OpenAI, Slack, SQLite, Docker types. |
| `internal/port` | domain + stdlib. Shared interfaces. | Framework or transport implementations. |
| `internal/usecase` | domain + port. Business logic. | Adapters or third-party SDKs. |
| `internal/adapter` | Concrete implementations. | Must not import other adapters (composed in `internal/app`). |
| `internal/app` | Composition root. | Must not import the CLI layer (`internal/cli`). |

Other internal packages: `agentdef` (agent/provider YAML definitions, stdlib+yaml.v3 only), `cliprotocol`
(stdlib-only `cli-v1` NDJSON wire contract between the `agentcli` adapter and shim processes), `manifest`
(Slack app manifest rendering), `secure` (credential redaction), `cli` (cobra delivery), `buildinfo`
(version metadata), `config` (path resolution).

When adding code, place it by what it depends on, not by convenience — the architecture test will catch
a misplaced import (e.g. a usecase reaching into an adapter, or two adapters importing each other) at
`go test ./...` time.

### Orientation for common changes

- **New adapter integration** (new LLM provider, storage backend, external tool): implement against an
  existing `internal/port` interface; wire it up in `internal/app/composition.go`. Adapters never import
  each other directly.
- **New agent-facing tool or capability**: usually spans `internal/adapter/toolfactory` (tool
  registration/scoping) and the owning `internal/usecase/*` package for the business logic.
- **Durable state / schema changes**: SQLite migrations live in `internal/adapter/sqlite`, gated by
  `PRAGMA user_version`. Current version is defined in `internal/adapter/sqlite/migrate.go` — see
  AGENTS.md's "Durable external-agent result delivery" section for the versioning discipline expected
  (CAS transactions, one-way schema bumps, no silent replay of legacy rows).
- **Agent/provider YAML definitions**: `internal/agentdef` (parsing) and `.local-agent/agents/` /
  `.local-agent/providers/` (tracked-in-git instance data, unlike the rest of `.local-agent/`).

## Data directory

`.local-agent/` is mostly gitignored (`config.yaml`, `local-agent.db`, `app-manifest.local.yaml`,
`local.env.example`, `memory/`). Exceptions tracked in git: `agents/`, `providers/`, `workflows/`.

`docs/` is gitignored going forward but still tracked from prior commits — it holds authoritative TRDs
(technical requirements docs) per feature area. Prefer reading the relevant TRD over guessing intent
when working on ACP, context compaction, memory, workstream orchestration, etc.

## Testing conventions

- Tests use local fakes only: temp SQLite, HTTP test servers, injected in-memory stores. No live
  credentials needed, including in `internal/integration` (real adapters + temp SQLite, no build tags).
- `go test ./...` always includes the architecture dependency check — a failure there means an import
  violates the layering table above, not a flaky test.

## Key conventions

- **Secrets** go in `.env` (0600, resolved before `.local-agent/config.yaml`). **Config** goes in
  `.local-agent/config.yaml`.
- **Redaction**: `internal/secure.Redactor` strips credentials from logs/errors/output at the last mile.
- **Context limits**: count Unicode code points, not bytes or rune length.
- **Canonical conversation keys**: `slack:{team}:dm:{channel}` or
  `slack:{team}:channel:{channel}:thread:{root_ts}`. ADK session IDs are `adk:{canonical-conversation-key}`
  — deterministic, opaque, never derived from untrusted text.
- **Worktrees**: `git wtadd <name> [git-worktree-add args...]` (repo alias) → creates
  `.worktrees/<name>` (gitignored).
