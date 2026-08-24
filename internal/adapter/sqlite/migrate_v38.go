package sqlite

import (
	"context"
	"database/sql"
)

// migrateV38 adds the scoped knowledge store: atomic claims with immutable
// identity and revision history, evidence references to the conversation
// ledger, owner-bound preferences, provident documents, content-free
// tombstones, and the knowledge projection outbox. It does not alter or
// backfill legacy memory tables, workstream tables, results, jobs, or
// activations.
func migrateV38(ctx context.Context, tx *sql.Tx) error {
	return execMigration(ctx, tx, 38, []string{
		`CREATE TABLE knowledge_claims (
			id TEXT NOT NULL,
			subject TEXT NOT NULL,
			predicate TEXT NOT NULL CHECK (predicate IN ('is', 'uses', 'runs_on', 'located_in', 'owns', 'relates_to')),
			value_kind TEXT NOT NULL CHECK (value_kind IN ('string', 'number', 'boolean', 'reference')),
			value_text TEXT NOT NULL DEFAULT '',
			value_number REAL NOT NULL DEFAULT 0,
			value_boolean INTEGER NOT NULL DEFAULT 0,
			value_reference TEXT NOT NULL DEFAULT '',
			scope_kind TEXT NOT NULL CHECK (scope_kind IN ('global', 'team', 'user', 'project', 'conversation', 'workstream')),
			scope_id TEXT NOT NULL DEFAULT '',
			source_class TEXT NOT NULL CHECK (source_class IN ('human', 'decision', 'observation', 'worker', 'root')),
			source_ref TEXT NOT NULL,
			author_id TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL CHECK (status IN ('asserted', 'verified', 'disputed', 'superseded', 'archived')),
			valid_from INTEGER NOT NULL DEFAULT 0,
			valid_until INTEGER NOT NULL DEFAULT 0,
			supersedes_id TEXT NOT NULL DEFAULT '',
			current_rev INTEGER NOT NULL DEFAULT 1,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			PRIMARY KEY (id),
			UNIQUE (subject, scope_kind, scope_id, source_ref),
			CHECK ((scope_kind = 'global') = (scope_id = '')),
			CHECK ((predicate IN ('owns', 'relates_to')) = (value_kind = 'reference')),
			CHECK (
				(value_kind = 'string' AND length(value_text) > 0 AND value_number = 0 AND value_boolean = 0 AND value_reference = '')
				OR (value_kind = 'number' AND value_text = '' AND value_reference = '' AND value_boolean = 0)
				OR (value_kind = 'boolean' AND value_text = '' AND value_number = 0 AND value_reference = '' AND value_boolean IN (0, 1))
				OR (value_kind = 'reference' AND length(value_reference) > 0 AND value_text = '' AND value_number = 0 AND value_boolean = 0)
			),
			CHECK (length(subject) > 0 AND length(source_ref) > 0 AND current_rev >= 1),
			CHECK (valid_from >= 0 AND valid_until >= 0 AND (valid_from = 0 OR valid_until = 0 OR valid_until >= valid_from)),
			CHECK (created_at > 0 AND updated_at > 0)
		)`,
		`CREATE TABLE knowledge_claim_revisions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			claim_id TEXT NOT NULL,
			revision_number INTEGER NOT NULL,
			subject TEXT NOT NULL,
			predicate TEXT NOT NULL,
			value_kind TEXT NOT NULL,
			value_text TEXT NOT NULL DEFAULT '',
			value_number REAL NOT NULL DEFAULT 0,
			value_boolean INTEGER NOT NULL DEFAULT 0,
			value_reference TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL CHECK (status IN ('asserted', 'verified', 'disputed', 'superseded', 'archived')),
			source_class TEXT NOT NULL CHECK (source_class IN ('human', 'decision', 'observation', 'worker', 'root')),
			operation TEXT NOT NULL CHECK (operation IN ('create', 'transition', 'supersede')),
			change_reason TEXT NOT NULL,
			source_ref TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			FOREIGN KEY (claim_id) REFERENCES knowledge_claims(id) ON DELETE CASCADE,
			UNIQUE (claim_id, revision_number),
			UNIQUE (claim_id, source_ref),
			CHECK (revision_number >= 1 AND length(change_reason) > 0 AND length(source_ref) > 0 AND created_at > 0),
			CHECK (predicate IN ('is', 'uses', 'runs_on', 'located_in', 'owns', 'relates_to')),
			CHECK (value_kind IN ('string', 'number', 'boolean', 'reference')),
			CHECK (
				(value_kind = 'string' AND length(value_text) > 0 AND value_number = 0 AND value_boolean = 0 AND value_reference = '')
				OR (value_kind = 'number' AND value_text = '' AND value_reference = '' AND value_boolean = 0)
				OR (value_kind = 'boolean' AND value_text = '' AND value_number = 0 AND value_reference = '' AND value_boolean IN (0, 1))
				OR (value_kind = 'reference' AND length(value_reference) > 0 AND value_text = '' AND value_number = 0 AND value_boolean = 0)
			),
			CHECK ((predicate IN ('owns', 'relates_to')) = (value_kind = 'reference')),
			CHECK (length(subject) > 0)
		)`,
		`CREATE TABLE knowledge_evidence (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			claim_revision INTEGER NOT NULL,
			conversation_key TEXT NOT NULL,
			exchange_ts TEXT NOT NULL,
			author_id TEXT NOT NULL,
			kind TEXT NOT NULL CHECK (kind IN ('source', 'decision')),
			FOREIGN KEY (claim_revision) REFERENCES knowledge_claim_revisions(id) ON DELETE CASCADE,
			CHECK (length(conversation_key) > 0 AND length(exchange_ts) > 0 AND length(author_id) > 0)
		)`,
		`CREATE TABLE knowledge_preferences (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			owner_key TEXT NOT NULL,
			key TEXT NOT NULL,
			value_kind TEXT NOT NULL CHECK (value_kind IN ('string', 'number', 'boolean')),
			value_text TEXT NOT NULL DEFAULT '',
			value_number REAL NOT NULL DEFAULT 0,
			value_boolean INTEGER NOT NULL DEFAULT 0,
			status TEXT NOT NULL CHECK (status IN ('active', 'archived')),
			source_ref TEXT NOT NULL,
			current_rev INTEGER NOT NULL DEFAULT 1,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			UNIQUE (owner_key, key),
			CHECK (
				(value_kind = 'string' AND length(value_text) > 0 AND value_number = 0 AND value_boolean = 0)
				OR (value_kind = 'number' AND value_text = '' AND value_boolean = 0)
				OR (value_kind = 'boolean' AND value_text = '' AND value_number = 0 AND value_boolean IN (0, 1))
			),
			CHECK (length(owner_key) > 0 AND length(key) > 0 AND length(source_ref) > 0),
			CHECK (current_rev >= 1 AND created_at > 0 AND updated_at > 0)
		)`,
		`CREATE TABLE knowledge_preference_revisions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			preference_id INTEGER NOT NULL,
			revision_number INTEGER NOT NULL,
			value_kind TEXT NOT NULL,
			value_text TEXT NOT NULL DEFAULT '',
			value_number REAL NOT NULL DEFAULT 0,
			value_boolean INTEGER NOT NULL DEFAULT 0,
			status TEXT NOT NULL CHECK (status IN ('active', 'archived')),
			source_ref TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			FOREIGN KEY (preference_id) REFERENCES knowledge_preferences(id) ON DELETE CASCADE,
			UNIQUE (preference_id, revision_number),
			UNIQUE (preference_id, source_ref),
			CHECK (revision_number >= 1 AND length(source_ref) > 0 AND created_at > 0),
			CHECK (value_kind IN ('string', 'number', 'boolean')),
			CHECK (
				(value_kind = 'string' AND length(value_text) > 0 AND value_number = 0 AND value_boolean = 0)
				OR (value_kind = 'number' AND value_text = '' AND value_boolean = 0)
				OR (value_kind = 'boolean' AND value_text = '' AND value_number = 0 AND value_boolean IN (0, 1))
			)
		)`,
		`CREATE TABLE knowledge_documents (
			id TEXT NOT NULL,
			subject TEXT NOT NULL,
			scope_kind TEXT NOT NULL CHECK (scope_kind IN ('global', 'team', 'user', 'project', 'conversation', 'workstream')),
			scope_id TEXT NOT NULL DEFAULT '',
			content_digest TEXT NOT NULL CHECK (length(content_digest) = 64 AND content_digest NOT GLOB '*[^0-9a-f]*'),
			content_handle TEXT NOT NULL,
			source_id TEXT NOT NULL DEFAULT '',
			source_rev INTEGER NOT NULL DEFAULT 0,
			provenance TEXT NOT NULL CHECK (provenance IN ('legacy_curated_document', 'curated')),
			status TEXT NOT NULL CHECK (status IN ('active', 'archived')),
			current_rev INTEGER NOT NULL DEFAULT 1,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			PRIMARY KEY (id),
			UNIQUE (subject, scope_kind, scope_id),
			CHECK ((scope_kind = 'global') = (scope_id = '')),
			CHECK ((provenance = 'legacy_curated_document') = (source_id != '' AND source_rev >= 1)),
			CHECK (length(subject) > 0 AND length(content_handle) > 0 AND current_rev >= 1 AND created_at > 0 AND updated_at > 0)
		)`,
		`CREATE TABLE knowledge_document_revisions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			document_id TEXT NOT NULL,
			revision_number INTEGER NOT NULL,
			status TEXT NOT NULL CHECK (status IN ('active', 'archived')),
			source_ref TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			FOREIGN KEY (document_id) REFERENCES knowledge_documents(id) ON DELETE CASCADE,
			UNIQUE (document_id, revision_number),
			UNIQUE (document_id, source_ref),
			CHECK (revision_number >= 1 AND length(source_ref) > 0 AND created_at > 0)
		)`,
		`CREATE TABLE knowledge_command_receipts (
			source_ref TEXT NOT NULL,
			action TEXT NOT NULL CHECK (action IN ('remember', 'correct', 'forget', 'archive', 'dispute')),
			payload_digest TEXT NOT NULL CHECK (length(payload_digest) = 64 AND payload_digest NOT GLOB '*[^0-9a-f]*'),
			target TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			PRIMARY KEY (source_ref),
			CHECK (length(source_ref) > 0 AND length(target) > 0 AND created_at > 0)
		)`,
		`CREATE TABLE knowledge_document_receipts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			document_id TEXT NOT NULL,
			subject TEXT NOT NULL,
			scope_kind TEXT NOT NULL CHECK (scope_kind IN ('global', 'team', 'user', 'project', 'conversation', 'workstream')),
			scope_id TEXT NOT NULL DEFAULT '',
			content_digest TEXT NOT NULL CHECK (length(content_digest) = 64 AND content_digest NOT GLOB '*[^0-9a-f]*'),
			content_handle TEXT NOT NULL,
			source_id TEXT NOT NULL DEFAULT '',
			source_rev INTEGER NOT NULL DEFAULT 0,
			provenance TEXT NOT NULL CHECK (provenance IN ('legacy_curated_document', 'curated')),
			status TEXT NOT NULL CHECK (status IN ('active', 'archived')),
			created_at INTEGER NOT NULL,
			FOREIGN KEY (document_id) REFERENCES knowledge_documents(id) ON DELETE CASCADE,
			UNIQUE (document_id),
			CHECK ((scope_kind = 'global') = (scope_id = '')),
			CHECK ((provenance = 'legacy_curated_document') = (source_id != '' AND source_rev >= 1)),
			CHECK (length(subject) > 0 AND length(content_handle) > 0 AND created_at > 0)
		)`,
		`CREATE TABLE knowledge_tombstones (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			subject_digest TEXT NOT NULL CHECK (length(subject_digest) = 64 AND subject_digest NOT GLOB '*[^0-9a-f]*'),
			scope_kind TEXT NOT NULL CHECK (scope_kind IN ('global', 'team', 'user', 'project', 'conversation', 'workstream')),
			scope_id TEXT NOT NULL DEFAULT '',
			forgotten_at INTEGER NOT NULL,
			source_ref TEXT NOT NULL,
			UNIQUE (subject_digest),
			CHECK ((scope_kind = 'global') = (scope_id = '')),
			CHECK (forgotten_at > 0 AND length(source_ref) > 0)
		)`,
		`CREATE TABLE knowledge_projection_outbox (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			status TEXT NOT NULL CHECK (status IN ('pending', 'processing', 'done', 'failed')),
			attempts INTEGER NOT NULL DEFAULT 0,
			next_attempt INTEGER NOT NULL,
			lease_until INTEGER NOT NULL DEFAULT 0,
			last_error TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE INDEX knowledge_claims_by_scope_status
			ON knowledge_claims (scope_kind, scope_id, status, updated_at DESC)`,
		`CREATE INDEX knowledge_claims_by_supersedes
			ON knowledge_claims (supersedes_id) WHERE supersedes_id != ''`,
		`CREATE INDEX knowledge_claim_revisions_by_claim
			ON knowledge_claim_revisions (claim_id, revision_number)`,
		`CREATE INDEX knowledge_evidence_by_revision
			ON knowledge_evidence (claim_revision)`,
		`CREATE UNIQUE INDEX knowledge_evidence_by_exchange
			ON knowledge_evidence (claim_revision, conversation_key, exchange_ts)`,
		`CREATE INDEX knowledge_preferences_by_owner
			ON knowledge_preferences (owner_key, status, updated_at DESC)`,
		`CREATE INDEX knowledge_preference_revisions_by_preference
			ON knowledge_preference_revisions (preference_id, revision_number)`,
		`CREATE TRIGGER knowledge_preference_revisions_immutable_update
			BEFORE UPDATE ON knowledge_preference_revisions
			BEGIN SELECT RAISE(ABORT, 'knowledge preference revision is immutable'); END`,
		`CREATE INDEX knowledge_documents_by_scope_status
			ON knowledge_documents (scope_kind, scope_id, status)`,
		`CREATE INDEX knowledge_document_revisions_by_document
			ON knowledge_document_revisions (document_id, revision_number)`,
		`CREATE INDEX knowledge_projection_outbox_by_status_and_next
			ON knowledge_projection_outbox (status, next_attempt)`,
		`CREATE INDEX knowledge_projection_outbox_by_processing_lease
			ON knowledge_projection_outbox (status, lease_until)`,
		`CREATE TRIGGER knowledge_claims_identity_immutable
			BEFORE UPDATE ON knowledge_claims
			WHEN NEW.id != OLD.id OR NEW.subject != OLD.subject OR NEW.predicate != OLD.predicate
				OR NEW.value_kind != OLD.value_kind OR NEW.value_text != OLD.value_text
				OR NEW.value_number != OLD.value_number OR NEW.value_boolean != OLD.value_boolean
				OR NEW.value_reference != OLD.value_reference OR NEW.scope_kind != OLD.scope_kind
				OR NEW.scope_id != OLD.scope_id OR NEW.source_class != OLD.source_class
				OR NEW.source_ref != OLD.source_ref OR NEW.author_id != OLD.author_id
				OR NEW.valid_from != OLD.valid_from OR NEW.valid_until != OLD.valid_until
				OR NEW.supersedes_id != OLD.supersedes_id OR NEW.created_at != OLD.created_at
			BEGIN SELECT RAISE(ABORT, 'knowledge claim identity is immutable'); END`,
		`CREATE TRIGGER knowledge_document_receipts_immutable_update
			BEFORE UPDATE ON knowledge_document_receipts
			BEGIN SELECT RAISE(ABORT, 'knowledge document receipt is immutable'); END`,
		`CREATE TRIGGER knowledge_document_revisions_immutable_update
			BEFORE UPDATE ON knowledge_document_revisions
			BEGIN SELECT RAISE(ABORT, 'knowledge document revision is immutable'); END`,
		`CREATE TRIGGER knowledge_command_receipts_immutable_update
			BEFORE UPDATE ON knowledge_command_receipts
			BEGIN SELECT RAISE(ABORT, 'knowledge command receipt is immutable'); END`,
		`CREATE TRIGGER knowledge_command_receipts_immutable_delete
			BEFORE DELETE ON knowledge_command_receipts
			BEGIN SELECT RAISE(ABORT, 'knowledge command receipt is immutable'); END`,
		`CREATE TRIGGER knowledge_claim_revisions_immutable_update
			BEFORE UPDATE ON knowledge_claim_revisions
			BEGIN SELECT RAISE(ABORT, 'knowledge claim revision is immutable'); END`,
		`CREATE TRIGGER knowledge_preferences_identity_immutable
			BEFORE UPDATE ON knowledge_preferences
			WHEN NEW.owner_key != OLD.owner_key OR NEW.key != OLD.key
				OR NEW.created_at != OLD.created_at
			BEGIN SELECT RAISE(ABORT, 'knowledge preference owner identity is immutable'); END`,
		`CREATE TRIGGER knowledge_documents_identity_immutable
			BEFORE UPDATE ON knowledge_documents
			WHEN NEW.id != OLD.id OR NEW.subject != OLD.subject OR NEW.scope_kind != OLD.scope_kind
				OR NEW.scope_id != OLD.scope_id OR NEW.content_digest != OLD.content_digest
				OR NEW.content_handle != OLD.content_handle OR NEW.source_id != OLD.source_id
				OR NEW.source_rev != OLD.source_rev OR NEW.provenance != OLD.provenance
				OR NEW.created_at != OLD.created_at
			BEGIN SELECT RAISE(ABORT, 'knowledge document identity is immutable'); END`,
		`CREATE TRIGGER knowledge_tombstones_immutable_update
			BEFORE UPDATE ON knowledge_tombstones
			BEGIN SELECT RAISE(ABORT, 'knowledge tombstone is immutable'); END`,
		`CREATE TRIGGER knowledge_tombstones_immutable_delete
			BEFORE DELETE ON knowledge_tombstones
			BEGIN SELECT RAISE(ABORT, 'knowledge tombstone is immutable'); END`,
		`CREATE TRIGGER memory_topic_revisions_guard_referenced_update
			BEFORE UPDATE ON memory_topic_revisions
			WHEN EXISTS (
				SELECT 1 FROM knowledge_documents d
				WHERE d.content_handle = 'memory_topics:' || OLD.topic_id || ':revision:' || OLD.id
					AND d.status IN ('active', 'archived')
			)
			BEGIN SELECT RAISE(ABORT, 'memory topic revision is referenced by a knowledge document'); END`,
		`CREATE TRIGGER memory_topic_revisions_guard_referenced_delete
			BEFORE DELETE ON memory_topic_revisions
			WHEN EXISTS (
				SELECT 1 FROM knowledge_documents d
				WHERE d.content_handle = 'memory_topics:' || OLD.topic_id || ':revision:' || OLD.id
					AND d.status IN ('active', 'archived')
			)
			BEGIN SELECT RAISE(ABORT, 'memory topic revision is referenced by a knowledge document'); END`,
	})
}
