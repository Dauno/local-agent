# Entity-memory policy

## Problem statement

Global curated memory already stores curator-approved topic revisions. It must
also reliably retain reusable entity facts supplied by authorized users, rather
than relying solely on an LLM to decide whether to propose them.

## User stories

- As an authorized user, when I say `Mi nombre es Dauno y soy el creador de
  local-agent`, my self-declared identity and role are curated with source
  attribution.
- As an authorized user, when I say `Recuerda que producción usa PostgreSQL
  16`, the supplied operational fact is eligible for durable curation.
- As an authorized user in a later Slack thread, I can receive relevant global
  curated facts as untrusted reference material.
- As a product owner, I need credentials, secrets, sensitive personal data,
  and instructions about prompts, tools, authorization, or policy rejected
  before persistence.

## Assumptions

- Existing authorization runs before an exchange can enter the curation
  outbox; the policy therefore consumes only successful authorized exchanges.
- Memory is intentionally global across authorized users and threads.
- Curation remains asynchronous. A successful reply does not promise that an
  irrelevant, contradictory, or unsupported claim will persist.
- Self-declared reusable identity and role facts retain their source evidence.
