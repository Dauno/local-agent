# Entity-memory policy design

## Decision

Add a pure `domain` entity-memory policy. It recognizes high-confidence
self-declared identity/role facts and explicit English/Spanish remember/save
requests from user messages. It creates a deterministic topic operation or,
when a matching current topic is supplied, a revision operation. The curator
merges those operations with its structured LLM proposal, keeping the
deterministic operation when both target the same slug.

This preserves asynchronous curation while ensuring approved facts do not
depend solely on model discretion. The existing use case remains the only
write-validation boundary and SQLite remains the only source of truth.

## Work plan

1. `internal/domain/memory.go`, `internal/domain/memory_test.go`
   - Add candidate extraction, ASCII-safe topic slugs, deterministic operation
     generation, lookup-query generation, and sensitive-personal-data/safety
     validation.
2. `internal/usecase/memory/service.go`, `internal/usecase/memory/service_test.go`
   - Query candidate entity terms as well as the current user message so a
     deterministic candidate can revise an existing global topic. Verify that
     unsafe personal data cannot reach the store.
3. `internal/adapter/memorycurator/curator.go`,
   `internal/adapter/memorycurator/curator_test.go`
   - State entity policy in the curator prompt and merge deterministic
     operations with parsed model operations, preferring trusted operations.
4. `internal/integration/entity_memory_test.go`
   - Exercise Spanish self-declaration, explicit remember request, global
     cross-thread recall, source evidence, and safety rejection through the
     policy, curator, store, and bot boundaries.

## Contracts

`domain.EntityMemoryCandidate` is pure data:

```go
type EntityMemoryCandidate struct {
    Slug, Title, Description, Content, ChangeReason, SearchQuery string
    Tags []string
}
```

`EntityMemoryCandidates(messages)` returns only sanitized, high-confidence
facts from user messages. `TrustedEntityMemoryOperations(messages, topics)`
returns `create_topic` or `revise` operations, using `ExpectedRev` from an
exactly matching supplied topic. `EntityMemorySearchQueries(messages)` returns
bounded factual terms for FTS matching.

## Test strategy

- Domain unit tests: Spanish identity, explicit Spanish remember request,
  update to a matching topic, and safety refusal for credentials, sensitive
  personal data, and Spanish prompt/tool directives.
- Curator adapter tests: deterministic operations are returned even when the
  LLM returns `[]`; model operations with the same slug do not override them;
  prompt states approved entity categories and explicit priority.
- Integration test: persist a curated Spanish fact with evidence, then have a
  later independent DM/thread request receive it through global FTS recall.
- Existing SQLite tests continue to prove receipts, revisions, evidence,
  topic budgets, and FTS bounds.

## Risks and mitigations

| Risk | Mitigation |
| --- | --- |
| Command disguised as a fact | Extract only payload after explicit request and validate every persisted field for instruction-like content. |
| Sensitive personal data | Central validator rejects recognisable emails, phone/financial/government/health identifiers before SQLite writes. |
| Existing-topic conflict | Use supplied current revision for exact deterministic slug; SQLite still enforces revision checks and all-or-nothing patches. |
| FTS misses updates | Search deterministic factual query terms in addition to raw conversation text and deduplicate bounded references. |
| Over-curation | Policy only covers self-declaration and explicit requests; other facts remain curator-judged. |
| Rollback | Revert policy/curator changes; no schema migration or configuration change is introduced. |
