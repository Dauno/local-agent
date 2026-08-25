# Release Notes

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
