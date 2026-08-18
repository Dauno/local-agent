package sqlite

import (
	"context"
	"database/sql"
)

// analysisFailureCodeCheck is the closed set of TRD 07 typed failure codes,
// shared by result_analyses.failure_code and analysis_steps.failure_code.
// The empty string means "no failure yet" and is always accepted alongside
// the ten closed codes from domain.AnalysisFailureCode.
const analysisFailureCodeCheck = `IN (
	'', 'analysis_source_too_large', 'analysis_segment_invalid', 'analysis_leaf_schema_invalid',
	'analysis_evidence_selector_invalid', 'analysis_reduction_irreducible', 'analysis_coverage_incomplete',
	'analysis_bundle_too_large', 'analysis_wall_time_exceeded', 'analysis_attempts_exhausted',
	'analysis_identity_stale'
)`

// migrateV40 adds the TRD 07 objective-bound result analysis catalog: the
// analysis row, its segment manifest, its leaf/reduction step queue, ordered
// reduction child edges, host-resolved evidence excerpts, and downstream
// bundle selectors. It is purely additive: no v34-v39 table gains a foreign
// key from these tables, and no existing row is rewritten or deleted.
//
// Timestamp unit: every v40 timestamp column is unix seconds. v38 uses
// nanoseconds for its truth tables and v39 uses seconds for its queue
// tables; v40 introduces both a truth table (result_analyses) and a queue
// table (analysis_steps) in the same migration, so mixing units within one
// migration would need two conventions for no benefit. v40 timestamps are
// used only for lease expiry, retry scheduling, and coarse ordering, never
// as an identity or digest input, so second precision is sufficient and one
// consistent unit is chosen for the whole migration.
func migrateV40(ctx context.Context, tx *sql.Tx) error {
	return execMigration(ctx, tx, 40, []string{
		`CREATE TABLE result_analyses (
			analysis_id TEXT PRIMARY KEY,
			source_result_id TEXT NOT NULL,
			source_sha256 TEXT NOT NULL,
			source_bytes INTEGER NOT NULL,
			objective_class TEXT NOT NULL,
			objective_digest TEXT NOT NULL,
			objective_text TEXT NOT NULL,
			segmentation_version TEXT NOT NULL,
			prompt_version TEXT NOT NULL,
			model_fingerprint TEXT NOT NULL,
			limits_digest TEXT NOT NULL,
			limits_json TEXT NOT NULL,
			actor TEXT NOT NULL,
			team_id TEXT NOT NULL,
			conversation_key TEXT NOT NULL,
			project TEXT NOT NULL,
			workstream_id TEXT NOT NULL,
			state TEXT NOT NULL,
			failure_code TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			CHECK (length(analysis_id) = 64 AND analysis_id NOT GLOB '*[^0-9a-f]*'),
			CHECK (length(source_result_id) = 64 AND source_result_id NOT GLOB '*[^0-9a-f]*'),
			CHECK (length(source_sha256) = 64 AND source_sha256 NOT GLOB '*[^0-9a-f]*'),
			CHECK (source_bytes > 0),
			CHECK (objective_class = 'bounded_question_v1'),
			CHECK (length(objective_digest) = 64 AND objective_digest NOT GLOB '*[^0-9a-f]*'),
			CHECK (length(objective_text) BETWEEN 1 AND 8000),
			CHECK (length(segmentation_version) > 0),
			CHECK (length(prompt_version) > 0),
			CHECK (length(model_fingerprint) > 0),
			CHECK (length(limits_digest) = 64 AND limits_digest NOT GLOB '*[^0-9a-f]*'),
			CHECK (length(limits_json) > 0),
			CHECK (length(actor) > 0 AND length(team_id) > 0 AND length(conversation_key) > 0 AND length(project) > 0),
			CHECK (length(workstream_id) > 0),
			CHECK (state IN ('preparing', 'running', 'completed', 'failed')),
			CHECK (failure_code ` + analysisFailureCodeCheck + `),
			CHECK (created_at > 0 AND updated_at >= created_at)
		) WITHOUT ROWID`,
		`CREATE UNIQUE INDEX result_analyses_by_semantic_identity
			ON result_analyses (source_result_id, source_sha256, objective_class, objective_digest,
				segmentation_version, prompt_version, model_fingerprint, limits_digest)`,
		`CREATE INDEX result_analyses_by_scope
			ON result_analyses (actor, team_id, conversation_key, project, created_at DESC)`,
		`CREATE INDEX result_analyses_by_workstream
			ON result_analyses (workstream_id, created_at DESC)`,
		`CREATE TRIGGER result_analyses_identity_immutable
			BEFORE UPDATE ON result_analyses
			WHEN NEW.analysis_id IS NOT OLD.analysis_id
				OR NEW.source_result_id IS NOT OLD.source_result_id
				OR NEW.source_sha256 IS NOT OLD.source_sha256
				OR NEW.source_bytes IS NOT OLD.source_bytes
				OR NEW.objective_class IS NOT OLD.objective_class
				OR NEW.objective_digest IS NOT OLD.objective_digest
				OR NEW.objective_text IS NOT OLD.objective_text
				OR NEW.segmentation_version IS NOT OLD.segmentation_version
				OR NEW.prompt_version IS NOT OLD.prompt_version
				OR NEW.model_fingerprint IS NOT OLD.model_fingerprint
				OR NEW.limits_digest IS NOT OLD.limits_digest
				OR NEW.limits_json IS NOT OLD.limits_json
				OR NEW.actor IS NOT OLD.actor
				OR NEW.team_id IS NOT OLD.team_id
				OR NEW.conversation_key IS NOT OLD.conversation_key
				OR NEW.project IS NOT OLD.project
				OR NEW.workstream_id IS NOT OLD.workstream_id
				OR NEW.created_at IS NOT OLD.created_at
			BEGIN SELECT RAISE(ABORT, 'analysis identity is immutable'); END`,
		`CREATE TRIGGER result_analyses_state_monotonic
			BEFORE UPDATE OF state ON result_analyses
			WHEN NEW.state != OLD.state
				AND NOT (
					(OLD.state = 'preparing' AND NEW.state IN ('running', 'completed', 'failed'))
					OR (OLD.state = 'running' AND NEW.state IN ('completed', 'failed'))
				)
			BEGIN SELECT RAISE(ABORT, 'analysis state transition is invalid'); END`,

		`CREATE TABLE analysis_segments (
			analysis_id TEXT NOT NULL,
			ordinal INTEGER NOT NULL,
			offset_bytes INTEGER NOT NULL,
			length_bytes INTEGER NOT NULL,
			sha256 TEXT NOT NULL,
			segmenter_version TEXT NOT NULL,
			overlap_prev_bytes INTEGER NOT NULL,
			PRIMARY KEY (analysis_id, ordinal),
			FOREIGN KEY (analysis_id) REFERENCES result_analyses(analysis_id) ON DELETE RESTRICT,
			CHECK (ordinal >= 0),
			CHECK (offset_bytes >= 0),
			CHECK (length_bytes > 0),
			CHECK (length(sha256) = 64 AND sha256 NOT GLOB '*[^0-9a-f]*'),
			CHECK (length(segmenter_version) > 0),
			CHECK (overlap_prev_bytes BETWEEN 0 AND 4096)
		) WITHOUT ROWID`,
		`CREATE TRIGGER analysis_segments_immutable
			BEFORE UPDATE ON analysis_segments
			BEGIN SELECT RAISE(ABORT, 'analysis segment manifest is immutable'); END`,

		`CREATE TABLE analysis_steps (
			analysis_id TEXT NOT NULL,
			step_id TEXT NOT NULL,
			kind TEXT NOT NULL,
			state TEXT NOT NULL,
			attempt INTEGER NOT NULL DEFAULT 0,
			generation INTEGER NOT NULL DEFAULT 0,
			next_attempt INTEGER NOT NULL DEFAULT 0,
			lease_until INTEGER NOT NULL DEFAULT 0,
			segment_ordinal INTEGER,
			failure_code TEXT NOT NULL DEFAULT '',
			output_digest TEXT NOT NULL DEFAULT '',
			output_payload TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			PRIMARY KEY (analysis_id, step_id),
			FOREIGN KEY (analysis_id) REFERENCES result_analyses(analysis_id) ON DELETE RESTRICT,
			CHECK (length(step_id) BETWEEN 1 AND 128),
			CHECK (kind IN ('leaf', 'reduction')),
			CHECK (state IN ('prepared', 'claimed', 'completed', 'failed')),
			CHECK (attempt >= 0 AND generation >= 0 AND next_attempt >= 0 AND lease_until >= 0),
			CHECK (
				(kind = 'leaf' AND segment_ordinal >= 0)
				OR (kind = 'reduction' AND segment_ordinal IS NULL)
			),
			CHECK (failure_code ` + analysisFailureCodeCheck + `),
			CHECK (output_digest = '' OR (length(output_digest) = 64 AND output_digest NOT GLOB '*[^0-9a-f]*')),
			CHECK (length(output_payload) <= 65536),
			CHECK (created_at > 0 AND updated_at >= created_at)
		) WITHOUT ROWID`,
		`CREATE INDEX analysis_steps_by_state_ready
			ON analysis_steps (analysis_id, state, next_attempt, step_id)`,
		`CREATE INDEX analysis_steps_by_state_lease
			ON analysis_steps (analysis_id, state, lease_until, step_id)`,
		`CREATE TRIGGER analysis_steps_completed_immutable
			BEFORE UPDATE ON analysis_steps
			WHEN OLD.state = 'completed'
			BEGIN SELECT RAISE(ABORT, 'completed analysis step output is immutable'); END`,

		`CREATE TABLE analysis_step_children (
			analysis_id TEXT NOT NULL,
			parent_step_id TEXT NOT NULL,
			ordinal INTEGER NOT NULL,
			child_step_id TEXT NOT NULL,
			PRIMARY KEY (analysis_id, parent_step_id, ordinal),
			FOREIGN KEY (analysis_id, parent_step_id) REFERENCES analysis_steps(analysis_id, step_id) ON DELETE RESTRICT,
			FOREIGN KEY (analysis_id, child_step_id) REFERENCES analysis_steps(analysis_id, step_id) ON DELETE RESTRICT,
			CHECK (ordinal >= 0),
			CHECK (length(child_step_id) BETWEEN 1 AND 128)
		) WITHOUT ROWID`,
		`CREATE UNIQUE INDEX analysis_step_children_by_child
			ON analysis_step_children (analysis_id, parent_step_id, child_step_id)`,
		`CREATE TRIGGER analysis_step_children_immutable
			BEFORE UPDATE ON analysis_step_children
			BEGIN SELECT RAISE(ABORT, 'analysis step child edges are immutable'); END`,

		`CREATE TABLE analysis_evidence (
			evidence_id TEXT PRIMARY KEY,
			analysis_id TEXT NOT NULL,
			leaf_step_id TEXT NOT NULL,
			segment_ordinal INTEGER NOT NULL,
			offset_bytes INTEGER NOT NULL,
			length_bytes INTEGER NOT NULL,
			sha256 TEXT NOT NULL,
			excerpt_bytes TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			FOREIGN KEY (analysis_id, leaf_step_id) REFERENCES analysis_steps(analysis_id, step_id) ON DELETE RESTRICT,
			CHECK (length(evidence_id) = 64 AND evidence_id NOT GLOB '*[^0-9a-f]*'),
			CHECK (segment_ordinal >= 0),
			CHECK (offset_bytes >= 0),
			CHECK (length_bytes > 0),
			CHECK (length(sha256) = 64 AND sha256 NOT GLOB '*[^0-9a-f]*'),
			CHECK (length(excerpt_bytes) BETWEEN 1 AND 2048),
			CHECK (created_at > 0)
		) WITHOUT ROWID`,
		`CREATE INDEX analysis_evidence_by_leaf_step
			ON analysis_evidence (analysis_id, leaf_step_id)`,
		`CREATE TRIGGER analysis_evidence_immutable
			BEFORE UPDATE ON analysis_evidence
			BEGIN SELECT RAISE(ABORT, 'analysis evidence excerpt is immutable'); END`,

		`CREATE TABLE analysis_bundles (
			bundle_id TEXT PRIMARY KEY,
			analysis_id TEXT NOT NULL,
			actor TEXT NOT NULL,
			team_id TEXT NOT NULL,
			conversation_key TEXT NOT NULL,
			project TEXT NOT NULL,
			state TEXT NOT NULL,
			content_sha256 TEXT NOT NULL,
			content_bytes INTEGER NOT NULL,
			created_at INTEGER NOT NULL,
			FOREIGN KEY (analysis_id) REFERENCES result_analyses(analysis_id) ON DELETE RESTRICT,
			CHECK (length(bundle_id) = 64 AND bundle_id NOT GLOB '*[^0-9a-f]*'),
			CHECK (length(actor) > 0 AND length(team_id) > 0 AND length(conversation_key) > 0 AND length(project) > 0),
			CHECK (state IN ('available', 'quarantined')),
			CHECK (length(content_sha256) = 64 AND content_sha256 NOT GLOB '*[^0-9a-f]*'),
			CHECK (content_bytes > 0 AND content_bytes <= 65536),
			CHECK (created_at > 0)
		) WITHOUT ROWID`,
		`CREATE INDEX analysis_bundles_by_analysis
			ON analysis_bundles (analysis_id, created_at DESC)`,
		`CREATE TRIGGER analysis_bundles_immutable
			BEFORE UPDATE ON analysis_bundles
			BEGIN SELECT RAISE(ABORT, 'analysis bundle selector is immutable'); END`,
	})
}
