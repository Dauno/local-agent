# Release Notes

## ADK Context Compaction

- Durable ADK history projection now preserves complete active tool and confirmation protocols, including invocations that began before a concurrent user message.
- Recent raw turns are selected before optional summaries; summaries are bounded, redacted before persistence, revalidated on reload, and protected by accumulated source digests.
- Summary work is bounded by source turns, prompt size, output tokens, job duration, and five total attempts. Older pending targets are coalesced per session.
- Compaction and streaming diagnostics expose bounded sizes, revisions, fallback states, and typed error categories without logging prompts, summaries, arguments, results, or SSE payloads.

Configuration continues to use `context.adk_compaction`; persisted summaries are limited to 8,000 Unicode code points.
