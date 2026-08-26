# Release Notes

## v0.10.0 (2026-08-25)

- Enabled gofumpt and golines (200-char limit) in the formatter chain and reformatted the tree.
- Applied Modern Go Guidelines rewrites for Go 1.27: `slices.SortFunc`, `slices.Contains`,
  `slices.ContainsFunc`, `slices.Reverse`, `slices.Clone`, `slices.Concat`, `wg.Go`,
  range-over-int, `b.Loop()`, `omitzero` JSON tags, and `fmt.Appendf` in place of an
  intermediate `Sprintf` allocation.

## v0.9.0 (2026-08-25)

- Removed the ACP transport. Every external agent now runs through the declarative agent CLI protocol; `agent_class: AcpAgent` and `runtime:` no longer parse.
- Declarative agent CLI descriptors (`invocation`, `stream`, `session`, `auth`) replace the retired cli-v1 shim hop, with a shared semantic-version parser and content-free progress reporting.
- A durable agent CLI job now captures its native session the moment the CLI announces it, so a job left `completion_unknown` after a crash is resumable instead of stuck.
- Automatic recovery on daemon restart resumes only a job that was actually running and holds a session; a cancelled job and a reconciliation that lost its own lease are never retried, and recovery shares the same worker concurrency limit as an ordinary claim.
- `jobs close` ends a completion-unknown job without resuming it, for the case an operator has inspected external state by hand and decided no recovery is needed.
- The transcript path resolves after the CLI process exits, not when the session is announced, and `jobs inspect` derives it lazily from the persisted session ID when a run never got to record it. Schema v43 adds the column.

## v0.8.0 (2026-08-24)

- Bumped the toolchain to Go 1.27.0 and updated module dependencies.
- Bumped ADK to v2.2.0; the crash-boundary event ordering the pin protects is verified unchanged (byte-for-byte identical protocol fixtures against the new version).
- Applied Go 1.27 modernize rewrites across the codebase and refactored the functions that exceeded the gocyclo complexity threshold.
- Cleared the golangci-lint backlog (errcheck, unused, nilerr, staticcheck, ineffassign, prealloc); migrated `.golangci.yml` to the v2 config schema.

## v0.7.0 (2026-08-23)

- Durable workstreams, native result handles, and bounded result analysis support long-running Slack orchestration with restart-safe state.
- Scope-authorized knowledge documents replace legacy entity memory and add lexical and semantic retrieval, context epochs, and retained source results.
- Schema v42 adds an explicit `local-agent db upgrade` flow with a cross-process lock, verified backup, quarantine, and preflight and postflight checks.
- SQLite WAL pooling, wake-driven workers, bounded context paths, and expanded doctor checks improve load behavior and recovery.

## v0.6.0 (2026-08-06)

- Foreground ACP calls now return one verified terminal result; detached jobs remain the only route for root activation and asynchronous synthesis.
- Durable external-agent delivery persists explicit completion routes and canonical notification/result identities, with schema migrations and restart-safe reconciliation.
- Doctor reports durable result identity health, while Slack and SQLite delivery checks reject malformed or inconsistent persisted evidence.

## v0.5.0 (2026-08-04)

- Durable ADK history projection now preserves complete active tool and confirmation protocols, including invocations that began before a concurrent user message.
- Recent raw turns are selected before optional summaries; summaries are bounded, redacted before persistence, revalidated on reload, and protected by accumulated source digests.
- Summary work is bounded by source turns, prompt size, output tokens, job duration, and five total attempts. Older pending targets are coalesced per session.
- Compaction and streaming diagnostics expose bounded sizes, revisions, fallback states, and typed error categories without logging prompts, summaries, arguments, results, or SSE payloads.

Configuration continues to use `context.adk_compaction`; persisted summaries are limited to 8,000 Unicode code points.
