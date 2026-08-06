# Release Notes

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
