# Entity-memory policy

## Overview

Curated memory now deterministically prioritizes high-confidence entity facts
from authorized users: self-declared identity/role statements and explicit
English or Spanish remember/save requests. It remains asynchronous and global;
the existing curator, validation, SQLite evidence/revisions, budgets,
idempotency receipts, FTS recall, and untrusted-reference rendering remain the
source of operational behavior.

## Behavior

The pure domain policy recognizes these examples:

```text
Mi nombre es Dauno y soy el creador de local-agent
Recuerda que producción usa PostgreSQL 16
```

The first creates or revises `person-dauno`; the second creates or revises
`system-produccion`. Exact matching existing topic references are revised with
the current revision number. Other reusable facts about people, systems,
projects, roles, decisions, preferences, and operational state remain eligible
for the structured curator LLM.

Recalled memory is global to authorized users by product design and is passed
to the foreground agent only as bounded, explicitly untrusted reference
material.

## Internal interfaces

`internal/domain` provides:

```go
domain.EntityMemorySearchQueries(messages)
domain.TrustedEntityMemoryOperations(messages, topics)
domain.EntityMemoryCandidates(messages)
```

These are internal pure-policy functions, not external APIs. The memory use
case uses their factual queries to find a matching FTS topic before the curator
plans an operation. The curator merges deterministic operations before
same-topic LLM operations.

## Safety

No raw transcript is stored as a topic. The policy and normal patch validator
reject recognisable credentials, authentication material, personal contact,
financial, and health identifiers, plus English and Spanish prompt, tool,
authorization, and policy instructions. All persisted fields are validated;
recalled data remains untrusted even after curation.

## Files changed in this delivery

- `internal/domain/memory.go` — entity candidate extraction, deterministic
  create/revise planning, topic lookup terms, and expanded safety validation.
- `internal/domain/memory_test.go` — Spanish identity, remember request,
  revision selection, sensitive-data, and Spanish directive coverage.
- `internal/usecase/memory/service.go` — finds candidate entity topics before
  curator planning while retaining configured recall bounds.
- `internal/adapter/memorycurator/curator.go` — prioritizes approved entity
  categories in the prompt and merges trusted operations over same-topic model
  proposals.
- `internal/adapter/memorycurator/curator_test.go` — verifies deterministic
  operation generation when the LLM proposes no patch.
- `internal/integration/entity_memory_test.go` — verifies Spanish curation,
  evidence attribution, and global later-thread recall.
- `feature_spec.md` and `technical_design.md` — requirements, assumptions,
  design decisions, risks, and rollback plan.

## Configuration and breaking changes

No configuration, schema, public API, or breaking change was added. The policy
runs only when the existing `memory.enabled` runtime feature is enabled.

## Testing

Run:

```sh
gofmt -w internal/domain/memory.go internal/domain/memory_test.go \
  internal/usecase/memory/service.go internal/adapter/memorycurator/curator.go \
  internal/adapter/memorycurator/curator_test.go internal/integration/entity_memory_test.go
go test ./...
go vet ./...
git diff --check
```

The deterministic rules intentionally cover only high-confidence identity/role
and explicit remember/save forms. Less explicit reusable facts remain
curator-judged and are not guaranteed to persist.
