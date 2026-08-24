package sqlite

import (
	"context"
	"database/sql"
)

// migrateV34 adds the empty V2 result catalog. It deliberately does not
// backfill legacy result references or rewrite V33 workstream links: neither
// has the producer lineage and exact scope needed to establish V2 identity.
func migrateV34(ctx context.Context, tx *sql.Tx) error {
	return execMigration(ctx, tx, 34, []string{
		`CREATE TABLE result_records (
			result_id TEXT PRIMARY KEY,
			producer_kind TEXT NOT NULL,
			producer_id TEXT NOT NULL,
			producer_revision INTEGER NOT NULL,
			storage_kind TEXT NOT NULL,
			storage_key TEXT NOT NULL,
			sha256 TEXT NOT NULL,
			bytes INTEGER NOT NULL,
			media_type TEXT NOT NULL,
			actor TEXT NOT NULL,
			team_id TEXT NOT NULL,
			conversation_key TEXT NOT NULL,
			project TEXT NOT NULL,
			retention_class TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			state TEXT NOT NULL,
			CHECK (length(result_id) = 64 AND result_id NOT GLOB '*[^0-9a-f]*'),
			CHECK (producer_kind IN ('acp_job', 'tool_operation', 'legacy_projection')),
			CHECK (length(producer_id) > 0),
			CHECK (producer_revision >= 0),
			CHECK (storage_kind IN ('artifact', 'recoverable')),
			CHECK (length(storage_key) > 0),
			CHECK (length(sha256) = 64 AND sha256 NOT GLOB '*[^0-9a-f]*'),
			CHECK (bytes > 0),
			CHECK (length(media_type) > 0),
			CHECK (length(actor) > 0 AND length(team_id) > 0 AND length(conversation_key) > 0 AND length(project) > 0),
			CHECK (retention_class IN ('context', 'conversation', 'workstream', 'exported')),
			CHECK (state IN ('available', 'quarantined'))
		) WITHOUT ROWID`,
		`CREATE UNIQUE INDEX result_records_by_producer
			ON result_records (producer_kind, producer_id, producer_revision)`,
		`CREATE INDEX result_records_by_scope
			ON result_records (actor, team_id, conversation_key, project, state, created_at DESC)`,
		`CREATE INDEX result_records_by_retention_state
			ON result_records (retention_class, state, created_at)`,
		`CREATE TRIGGER result_records_identity_immutable
			BEFORE UPDATE ON result_records
			WHEN NEW.result_id IS NOT OLD.result_id
				OR NEW.producer_kind IS NOT OLD.producer_kind
				OR NEW.producer_id IS NOT OLD.producer_id
				OR NEW.producer_revision IS NOT OLD.producer_revision
				OR NEW.storage_kind IS NOT OLD.storage_kind
				OR NEW.storage_key IS NOT OLD.storage_key
				OR NEW.sha256 IS NOT OLD.sha256
				OR NEW.bytes IS NOT OLD.bytes
				OR NEW.media_type IS NOT OLD.media_type
				OR NEW.actor IS NOT OLD.actor
				OR NEW.team_id IS NOT OLD.team_id
				OR NEW.conversation_key IS NOT OLD.conversation_key
				OR NEW.project IS NOT OLD.project
				OR NEW.retention_class IS NOT OLD.retention_class
				OR NEW.created_at IS NOT OLD.created_at
			BEGIN SELECT RAISE(ABORT, 'result identity is immutable'); END`,
		`CREATE TRIGGER result_records_state_monotonic
			BEFORE UPDATE OF state ON result_records
			WHEN NEW.state != OLD.state
				AND NOT (OLD.state = 'available' AND NEW.state = 'quarantined')
			BEGIN SELECT RAISE(ABORT, 'result state transition is invalid'); END`,

		`CREATE TABLE result_representations (
			representation_id TEXT PRIMARY KEY,
			result_id TEXT NOT NULL,
			kind TEXT NOT NULL,
			state TEXT NOT NULL,
			source_sha256 TEXT NOT NULL,
			source_bytes INTEGER NOT NULL,
			algorithm_or_prompt_version TEXT NOT NULL,
			payload_sha256 TEXT NOT NULL,
			payload_bytes INTEGER NOT NULL,
			created_at INTEGER NOT NULL,
			FOREIGN KEY (result_id) REFERENCES result_records(result_id) ON DELETE RESTRICT,
			CHECK (length(representation_id) = 64 AND representation_id NOT GLOB '*[^0-9a-f]*'),
			CHECK (length(result_id) = 64 AND result_id NOT GLOB '*[^0-9a-f]*'),
			CHECK (kind IN ('producer_handoff_v1', 'host_reduction')),
			CHECK (state IN ('available', 'quarantined')),
			CHECK (length(source_sha256) = 64 AND source_sha256 NOT GLOB '*[^0-9a-f]*'),
			CHECK (source_bytes > 0),
			CHECK (length(algorithm_or_prompt_version) > 0),
			CHECK (length(payload_sha256) = 64 AND payload_sha256 NOT GLOB '*[^0-9a-f]*'),
			CHECK (payload_bytes > 0)
		) WITHOUT ROWID`,
		`CREATE INDEX result_representations_by_result
			ON result_representations (result_id, state, created_at DESC)`,
		`CREATE TRIGGER result_representations_require_source_identity
			BEFORE INSERT ON result_representations
			WHEN NOT EXISTS (
				SELECT 1 FROM result_records r
				WHERE r.result_id = NEW.result_id
					AND r.sha256 = NEW.source_sha256
					AND r.bytes = NEW.source_bytes
					AND r.state = 'available'
			)
			BEGIN SELECT RAISE(ABORT, 'result representation source identity mismatch'); END`,
		`CREATE TRIGGER result_representations_identity_immutable
			BEFORE UPDATE ON result_representations
			WHEN NEW.representation_id IS NOT OLD.representation_id
				OR NEW.result_id IS NOT OLD.result_id
				OR NEW.kind IS NOT OLD.kind
				OR NEW.source_sha256 IS NOT OLD.source_sha256
				OR NEW.source_bytes IS NOT OLD.source_bytes
				OR NEW.algorithm_or_prompt_version IS NOT OLD.algorithm_or_prompt_version
				OR NEW.payload_sha256 IS NOT OLD.payload_sha256
				OR NEW.payload_bytes IS NOT OLD.payload_bytes
				OR NEW.created_at IS NOT OLD.created_at
			BEGIN SELECT RAISE(ABORT, 'result representation is immutable'); END`,
		`CREATE TRIGGER result_representations_state_monotonic
			BEFORE UPDATE OF state ON result_representations
			WHEN NEW.state != OLD.state
				AND NOT (OLD.state = 'available' AND NEW.state = 'quarantined')
			BEGIN SELECT RAISE(ABORT, 'result representation state transition is invalid'); END`,

		`CREATE TABLE result_references (
			reference_id TEXT PRIMARY KEY,
			result_id TEXT NOT NULL,
			owner_kind TEXT NOT NULL,
			owner_id TEXT NOT NULL,
			state TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			released_at INTEGER NOT NULL DEFAULT 0,
			FOREIGN KEY (result_id) REFERENCES result_records(result_id) ON DELETE RESTRICT,
			CHECK (length(reference_id) = 64 AND reference_id NOT GLOB '*[^0-9a-f]*'),
			CHECK (length(result_id) = 64 AND result_id NOT GLOB '*[^0-9a-f]*'),
			CHECK (length(owner_kind) > 0 AND length(owner_id) > 0),
			CHECK (state IN ('live', 'released')),
			CHECK ((state = 'live' AND released_at = 0) OR (state = 'released' AND released_at >= created_at))
		) WITHOUT ROWID`,
		`CREATE UNIQUE INDEX result_references_by_owner_result
			ON result_references (result_id, owner_kind, owner_id)`,
		`CREATE INDEX result_references_live_by_result
			ON result_references (result_id, owner_kind, owner_id) WHERE state = 'live'`,
		`CREATE INDEX result_references_live_by_owner
			ON result_references (owner_kind, owner_id, result_id) WHERE state = 'live'`,
		`CREATE TRIGGER result_references_lifecycle
			BEFORE UPDATE ON result_references
			WHEN NEW.reference_id IS NOT OLD.reference_id
				OR NEW.result_id IS NOT OLD.result_id
				OR NEW.owner_kind IS NOT OLD.owner_kind
				OR NEW.owner_id IS NOT OLD.owner_id
				OR NEW.created_at IS NOT OLD.created_at
				OR NOT (
					(NEW.state = OLD.state AND NEW.released_at = OLD.released_at)
					OR (OLD.state = 'live' AND NEW.state = 'released')
				)
			BEGIN SELECT RAISE(ABORT, 'result reference lifecycle is invalid'); END`,

		`CREATE TABLE result_materializations (
			producer_kind TEXT NOT NULL,
			producer_id TEXT NOT NULL,
			producer_revision INTEGER NOT NULL,
			result_id TEXT NOT NULL UNIQUE,
			state TEXT NOT NULL,
			storage_kind TEXT NOT NULL DEFAULT '',
			storage_key TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			PRIMARY KEY (producer_kind, producer_id, producer_revision),
			CHECK (producer_kind IN ('acp_job', 'tool_operation', 'legacy_projection')),
			CHECK (length(producer_id) > 0),
			CHECK (producer_revision >= 0),
			CHECK (length(result_id) = 64 AND result_id NOT GLOB '*[^0-9a-f]*'),
			CHECK (state IN ('reserved', 'payload_published', 'committed', 'quarantined')),
			CHECK (
				(state = 'reserved' AND length(storage_kind) = 0 AND length(storage_key) = 0)
				OR (state IN ('payload_published', 'committed', 'quarantined')
					AND storage_kind IN ('artifact', 'recoverable') AND length(storage_key) > 0)
			),
			CHECK (updated_at >= created_at)
		) WITHOUT ROWID`,
		`CREATE INDEX result_materializations_by_state
			ON result_materializations (state, updated_at)`,
		`CREATE TRIGGER result_materializations_begin_reserved
			BEFORE INSERT ON result_materializations
			WHEN NEW.state != 'reserved'
			BEGIN SELECT RAISE(ABORT, 'result materialization must begin reserved'); END`,
		`CREATE TRIGGER result_materializations_transition_guard
			BEFORE UPDATE ON result_materializations
			WHEN NEW.producer_kind IS NOT OLD.producer_kind
				OR NEW.producer_id IS NOT OLD.producer_id
				OR NEW.producer_revision IS NOT OLD.producer_revision
				OR NEW.result_id IS NOT OLD.result_id
				OR NEW.created_at IS NOT OLD.created_at
				OR NEW.updated_at < OLD.updated_at
				OR NOT (
					(NEW.state = OLD.state
						AND NEW.storage_kind = OLD.storage_kind AND NEW.storage_key = OLD.storage_key)
					OR (OLD.state = 'reserved' AND NEW.state = 'payload_published')
					OR (OLD.state = 'payload_published' AND NEW.state IN ('committed', 'quarantined')
						AND NEW.storage_kind = OLD.storage_kind AND NEW.storage_key = OLD.storage_key)
				)
			BEGIN SELECT RAISE(ABORT, 'result materialization transition is invalid'); END`,
		`CREATE TRIGGER result_materializations_committed_update_requires_record
			BEFORE UPDATE OF state ON result_materializations
			WHEN NEW.state = 'committed' AND NOT EXISTS (
				SELECT 1 FROM result_records r
				WHERE r.result_id = NEW.result_id
					AND r.producer_kind = NEW.producer_kind
					AND r.producer_id = NEW.producer_id
					AND r.producer_revision = NEW.producer_revision
					AND r.storage_kind = NEW.storage_kind
					AND r.storage_key = NEW.storage_key
					AND r.state = 'available'
			)
			BEGIN SELECT RAISE(ABORT, 'committed materialization requires matching result'); END`,

		`CREATE UNIQUE INDEX workstream_result_links_by_identity
			ON workstream_result_links (workstream_id, result_link_id, result_identity)`,

		`CREATE TABLE workstream_result_link_results (
			workstream_id TEXT NOT NULL,
			result_link_id TEXT NOT NULL,
			result_id TEXT NOT NULL,
			verified_at INTEGER NOT NULL,
			PRIMARY KEY (workstream_id, result_link_id),
			FOREIGN KEY (workstream_id, result_link_id, result_id)
				REFERENCES workstream_result_links(workstream_id, result_link_id, result_identity) ON DELETE RESTRICT,
			FOREIGN KEY (result_id) REFERENCES result_records(result_id) ON DELETE RESTRICT,
			CHECK (length(result_id) = 64 AND result_id NOT GLOB '*[^0-9a-f]*')
		) WITHOUT ROWID`,
		`CREATE INDEX workstream_result_link_results_by_result
			ON workstream_result_link_results (result_id, workstream_id, result_link_id)`,
		`CREATE TRIGGER workstream_result_link_results_require_available
			BEFORE INSERT ON workstream_result_link_results
			WHEN NOT EXISTS (
				SELECT 1 FROM result_records r
				WHERE r.result_id = NEW.result_id AND r.state = 'available'
			)
			BEGIN SELECT RAISE(ABORT, 'workstream result binding requires available result'); END`,
		`CREATE TRIGGER workstream_result_link_results_immutable
			BEFORE UPDATE ON workstream_result_link_results
			BEGIN SELECT RAISE(ABORT, 'verified workstream result binding is immutable'); END`,
	})
}
