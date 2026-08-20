package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

func insertResultAnalysis(t *testing.T, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, analysisID string, now int64) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `INSERT INTO result_analyses (
		analysis_id, source_result_id, source_sha256, source_bytes, objective_class, objective_digest,
		objective_text, segmentation_version, prompt_version, model_fingerprint, limits_digest, limits_json,
		actor, team_id, conversation_key, project, workstream_id, state, created_at, updated_at)
		VALUES (?, ?, ?, 100, 'bounded_question_v1', ?, 'objective text', 'text_v1', 'prompt_v1', 'model_v1', ?, '{}',
		'U1', 'T1', 'slack:T1:dm:U1', 'workspace', 'ws-1', 'preparing', ?, ?)`,
		analysisID, strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64), strings.Repeat("d", 64), now, now)
	if err != nil {
		t.Fatalf("insert result_analyses fixture: %v", err)
	}
}

func TestMigrationV40FreshObjectsAndConstraints(t *testing.T) {
	store, err := Initialize(t.Context(), t.TempDir()+"/fresh-v40.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	db := store.DB()

	for _, table := range []string{
		"result_analyses", "analysis_segments", "analysis_steps",
		"analysis_step_children", "analysis_evidence", "analysis_bundles",
	} {
		var present int
		if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name = ?`, table).Scan(&present); err != nil || present != 1 {
			t.Fatalf("v40 table %q present = %d, %v", table, present, err)
		}
	}
	for _, index := range []string{
		"result_analyses_by_semantic_identity", "result_analyses_by_scope", "result_analyses_by_workstream",
		"analysis_steps_by_state_ready", "analysis_steps_by_state_lease",
		"analysis_step_children_by_child", "analysis_evidence_by_leaf_step", "analysis_bundles_by_analysis",
	} {
		var present int
		if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sqlite_schema WHERE type = 'index' AND name = ?`, index).Scan(&present); err != nil || present != 1 {
			t.Fatalf("v40 index %q present = %d, %v", index, present, err)
		}
	}
	for _, trigger := range []string{
		"result_analyses_identity_immutable", "result_analyses_state_monotonic",
		"analysis_segments_immutable", "analysis_steps_completed_immutable",
		"analysis_step_children_immutable", "analysis_evidence_immutable", "analysis_bundles_immutable",
	} {
		var present int
		if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sqlite_schema WHERE type = 'trigger' AND name = ?`, trigger).Scan(&present); err != nil || present != 1 {
			t.Fatalf("v40 trigger %q present = %d, %v", trigger, present, err)
		}
	}

	now := time.Now().UTC().Unix()
	hex64 := strings.Repeat("a", 64)
	insertFails := func(name, statement string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(t.Context(), statement, args...); err == nil {
			t.Errorf("%s: statement unexpectedly succeeded", name)
		}
	}

	insertFails("result_analyses bad analysis id", `INSERT INTO result_analyses (
		analysis_id, source_result_id, source_sha256, source_bytes, objective_class, objective_digest,
		objective_text, segmentation_version, prompt_version, model_fingerprint, limits_digest, limits_json,
		actor, team_id, conversation_key, project, workstream_id, state, created_at, updated_at)
		VALUES ('not-hex', ?, ?, 100, 'bounded_question_v1', ?, 'text', 'text_v1', 'p1', 'm1', ?, '{}', 'U1', 'T1', 'c1', 'p', 'ws', 'preparing', ?, ?)`,
		hex64, hex64, hex64, hex64, now, now)
	insertFails("result_analyses zero source bytes", `INSERT INTO result_analyses (
		analysis_id, source_result_id, source_sha256, source_bytes, objective_class, objective_digest,
		objective_text, segmentation_version, prompt_version, model_fingerprint, limits_digest, limits_json,
		actor, team_id, conversation_key, project, workstream_id, state, created_at, updated_at)
		VALUES (?, ?, ?, 0, 'bounded_question_v1', ?, 'text', 'text_v1', 'p1', 'm1', ?, '{}', 'U1', 'T1', 'c1', 'p', 'ws', 'preparing', ?, ?)`,
		hex64, hex64, hex64, hex64, hex64, now, now)
	insertFails("result_analyses unknown objective class", `INSERT INTO result_analyses (
		analysis_id, source_result_id, source_sha256, source_bytes, objective_class, objective_digest,
		objective_text, segmentation_version, prompt_version, model_fingerprint, limits_digest, limits_json,
		actor, team_id, conversation_key, project, workstream_id, state, created_at, updated_at)
		VALUES (?, ?, ?, 100, 'other_v1', ?, 'text', 'text_v1', 'p1', 'm1', ?, '{}', 'U1', 'T1', 'c1', 'p', 'ws', 'preparing', ?, ?)`,
		hex64, hex64, hex64, hex64, hex64, now, now)
	insertFails("result_analyses unknown state", `INSERT INTO result_analyses (
		analysis_id, source_result_id, source_sha256, source_bytes, objective_class, objective_digest,
		objective_text, segmentation_version, prompt_version, model_fingerprint, limits_digest, limits_json,
		actor, team_id, conversation_key, project, workstream_id, state, created_at, updated_at)
		VALUES (?, ?, ?, 100, 'bounded_question_v1', ?, 'text', 'text_v1', 'p1', 'm1', ?, '{}', 'U1', 'T1', 'c1', 'p', 'ws', 'unknown', ?, ?)`,
		hex64, hex64, hex64, hex64, hex64, now, now)
	insertFails("result_analyses unknown failure code", `INSERT INTO result_analyses (
		analysis_id, source_result_id, source_sha256, source_bytes, objective_class, objective_digest,
		objective_text, segmentation_version, prompt_version, model_fingerprint, limits_digest, limits_json,
		actor, team_id, conversation_key, project, workstream_id, state, failure_code, created_at, updated_at)
		VALUES (?, ?, ?, 100, 'bounded_question_v1', ?, 'text', 'text_v1', 'p1', 'm1', ?, '{}', 'U1', 'T1', 'c1', 'p', 'ws', 'failed', 'invented', ?, ?)`,
		hex64, hex64, hex64, hex64, hex64, now, now)
	// Asserts one past domain.HardMaxAnalysisObjectiveRunes so this test
	// fails, rather than silently drifting, if the migration's objective_text
	// ceiling and the domain rune ceiling ever diverge.
	insertFails("result_analyses objective text over hard max", `INSERT INTO result_analyses (
		analysis_id, source_result_id, source_sha256, source_bytes, objective_class, objective_digest,
		objective_text, segmentation_version, prompt_version, model_fingerprint, limits_digest, limits_json,
		actor, team_id, conversation_key, project, workstream_id, state, created_at, updated_at)
		VALUES (?, ?, ?, 100, 'bounded_question_v1', ?, ?, 'text_v1', 'p1', 'm1', ?, '{}', 'U1', 'T1', 'c1', 'p', 'ws', 'preparing', ?, ?)`,
		hex64, hex64, hex64, hex64, strings.Repeat("a", domain.HardMaxAnalysisObjectiveRunes+1), hex64, now, now)
	insertFails("result_analyses empty workstream id", `INSERT INTO result_analyses (
		analysis_id, source_result_id, source_sha256, source_bytes, objective_class, objective_digest,
		objective_text, segmentation_version, prompt_version, model_fingerprint, limits_digest, limits_json,
		actor, team_id, conversation_key, project, workstream_id, state, created_at, updated_at)
		VALUES (?, ?, ?, 100, 'bounded_question_v1', ?, 'text', 'text_v1', 'p1', 'm1', ?, '{}', 'U1', 'T1', 'c1', 'p', '', 'preparing', ?, ?)`,
		hex64, hex64, hex64, hex64, hex64, now, now)

	insertResultAnalysis(t, db, hex64, now)

	insertFails("analysis_segments negative ordinal", `INSERT INTO analysis_segments
		(analysis_id, ordinal, offset_bytes, length_bytes, sha256, segmenter_version, overlap_prev_bytes)
		VALUES (?, -1, 0, 10, ?, 'text_v1', 0)`, hex64, hex64)
	insertFails("analysis_segments zero length", `INSERT INTO analysis_segments
		(analysis_id, ordinal, offset_bytes, length_bytes, sha256, segmenter_version, overlap_prev_bytes)
		VALUES (?, 0, 0, 0, ?, 'text_v1', 0)`, hex64, hex64)
	// This asserts one past domain.HardMaxAnalysisOverlapBytes, not a bare
	// literal, so the test fails (rather than silently drifting) if the
	// domain constant and the migration's own hardcoded 4096 ceiling ever
	// diverge.
	insertFails("analysis_segments overlap over hard max", `INSERT INTO analysis_segments
		(analysis_id, ordinal, offset_bytes, length_bytes, sha256, segmenter_version, overlap_prev_bytes)
		VALUES (?, 0, 0, 10, ?, 'text_v1', ?)`, hex64, hex64, domain.HardMaxAnalysisOverlapBytes+1)
	if _, err := db.ExecContext(t.Context(), `INSERT INTO analysis_segments
		(analysis_id, ordinal, offset_bytes, length_bytes, sha256, segmenter_version, overlap_prev_bytes)
		VALUES (?, 1, 100, 10, ?, 'text_v1', ?)`, hex64, hex64, domain.HardMaxAnalysisOverlapBytes); err != nil {
		t.Fatalf("overlap at domain.HardMaxAnalysisOverlapBytes must be accepted: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO analysis_segments
		(analysis_id, ordinal, offset_bytes, length_bytes, sha256, segmenter_version, overlap_prev_bytes)
		VALUES (?, 0, 0, 100, ?, 'text_v1', 0)`, hex64, hex64); err != nil {
		t.Fatalf("valid analysis_segments insert failed: %v", err)
	}

	insertFails("analysis_steps unknown kind", `INSERT INTO analysis_steps
		(analysis_id, step_id, kind, state, segment_ordinal, created_at, updated_at)
		VALUES (?, 's1', 'other', 'prepared', 0, ?, ?)`, hex64, now, now)
	insertFails("analysis_steps leaf without ordinal", `INSERT INTO analysis_steps
		(analysis_id, step_id, kind, state, segment_ordinal, created_at, updated_at)
		VALUES (?, 's1', 'leaf', 'prepared', NULL, ?, ?)`, hex64, now, now)
	insertFails("analysis_steps reduction with ordinal", `INSERT INTO analysis_steps
		(analysis_id, step_id, kind, state, segment_ordinal, created_at, updated_at)
		VALUES (?, 's1', 'reduction', 'prepared', 0, ?, ?)`, hex64, now, now)
	insertFails("analysis_steps unknown failure code", `INSERT INTO analysis_steps
		(analysis_id, step_id, kind, state, segment_ordinal, failure_code, created_at, updated_at)
		VALUES (?, 's1', 'leaf', 'failed', 0, 'invented', ?, ?)`, hex64, now, now)
	if _, err := db.ExecContext(t.Context(), `INSERT INTO analysis_steps
		(analysis_id, step_id, kind, state, segment_ordinal, created_at, updated_at)
		VALUES (?, 'leaf-0', 'leaf', 'prepared', 0, ?, ?)`, hex64, now, now); err != nil {
		t.Fatalf("valid leaf step insert failed: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO analysis_steps
		(analysis_id, step_id, kind, state, segment_ordinal, created_at, updated_at)
		VALUES (?, 'reduce-0', 'reduction', 'prepared', NULL, ?, ?)`, hex64, now, now); err != nil {
		t.Fatalf("valid reduction step insert failed: %v", err)
	}

	insertFails("analysis_step_children negative ordinal", `INSERT INTO analysis_step_children
		(analysis_id, parent_step_id, ordinal, child_step_id) VALUES (?, 'reduce-0', -1, 'leaf-0')`, hex64)
	if _, err := db.ExecContext(t.Context(), `INSERT INTO analysis_step_children
		(analysis_id, parent_step_id, ordinal, child_step_id) VALUES (?, 'reduce-0', 0, 'leaf-0')`, hex64); err != nil {
		t.Fatalf("valid analysis_step_children insert failed: %v", err)
	}

	insertFails("analysis_evidence oversized excerpt", `INSERT INTO analysis_evidence
		(evidence_id, analysis_id, leaf_step_id, segment_ordinal, offset_bytes, length_bytes, sha256, excerpt_bytes, created_at)
		VALUES (?, ?, 'leaf-0', 0, 0, 10, ?, ?, ?)`, hex64, hex64, hex64, strings.Repeat("x", 2049), now)
	if _, err := db.ExecContext(t.Context(), `INSERT INTO analysis_evidence
		(evidence_id, analysis_id, leaf_step_id, segment_ordinal, offset_bytes, length_bytes, sha256, excerpt_bytes, created_at)
		VALUES (?, ?, 'leaf-0', 0, 0, 10, ?, 'excerpt', ?)`, hex64, hex64, hex64, now); err != nil {
		t.Fatalf("valid analysis_evidence insert failed: %v", err)
	}

	insertFails("analysis_bundles unknown state", `INSERT INTO analysis_bundles
		(bundle_id, analysis_id, actor, team_id, conversation_key, project, state, content_sha256, content_bytes, created_at)
		VALUES (?, ?, 'U1', 'T1', 'c1', 'p', 'unknown', ?, 10, ?)`, hex64, hex64, hex64, now)
	if _, err := db.ExecContext(t.Context(), `INSERT INTO analysis_bundles
		(bundle_id, analysis_id, actor, team_id, conversation_key, project, state, content_sha256, content_bytes, created_at)
		VALUES (?, ?, 'U1', 'T1', 'c1', 'p', 'available', ?, 10, ?)`, hex64, hex64, hex64, now); err != nil {
		t.Fatalf("valid analysis_bundles insert failed: %v", err)
	}
}

func TestMigrationV40TriggersAbortProtectedUpdates(t *testing.T) {
	store, err := Initialize(t.Context(), t.TempDir()+"/triggers-v40.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	db := store.DB()
	now := time.Now().UTC().Unix()
	hex64 := strings.Repeat("a", 64)

	insertResultAnalysis(t, db, hex64, now)
	if _, err := db.ExecContext(t.Context(), `INSERT INTO analysis_segments
		(analysis_id, ordinal, offset_bytes, length_bytes, sha256, segmenter_version, overlap_prev_bytes)
		VALUES (?, 0, 0, 100, ?, 'text_v1', 0)`, hex64, hex64); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO analysis_steps
		(analysis_id, step_id, kind, state, segment_ordinal, created_at, updated_at)
		VALUES (?, 'leaf-0', 'leaf', 'completed', 0, ?, ?)`, hex64, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO analysis_steps
		(analysis_id, step_id, kind, state, segment_ordinal, created_at, updated_at)
		VALUES (?, 'reduce-0', 'reduction', 'prepared', NULL, ?, ?)`, hex64, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO analysis_step_children
		(analysis_id, parent_step_id, ordinal, child_step_id) VALUES (?, 'reduce-0', 0, 'leaf-0')`, hex64); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO analysis_evidence
		(evidence_id, analysis_id, leaf_step_id, segment_ordinal, offset_bytes, length_bytes, sha256, excerpt_bytes, created_at)
		VALUES (?, ?, 'leaf-0', 0, 0, 10, ?, 'excerpt', ?)`, hex64, hex64, hex64, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO analysis_bundles
		(bundle_id, analysis_id, actor, team_id, conversation_key, project, state, content_sha256, content_bytes, created_at)
		VALUES (?, ?, 'U1', 'T1', 'c1', 'p', 'available', ?, 10, ?)`, hex64, hex64, hex64, now); err != nil {
		t.Fatal(err)
	}

	assertAborts := func(name, statement string, args ...any) {
		t.Helper()
		_, err := db.ExecContext(t.Context(), statement, args...)
		if err == nil {
			t.Fatalf("%s: update unexpectedly succeeded", name)
		}
	}

	assertAborts("result_analyses_identity_immutable",
		`UPDATE result_analyses SET source_result_id = ? WHERE analysis_id = ?`, strings.Repeat("f", 64), hex64)
	if _, err := db.ExecContext(t.Context(), `UPDATE result_analyses SET state = 'running' WHERE analysis_id = ?`, hex64); err != nil {
		t.Fatalf("legal state transition rejected: %v", err)
	}
	assertAborts("result_analyses_state_monotonic (running back to preparing)",
		`UPDATE result_analyses SET state = 'preparing' WHERE analysis_id = ?`, hex64)
	if _, err := db.ExecContext(t.Context(), `UPDATE result_analyses SET state = 'completed' WHERE analysis_id = ?`, hex64); err != nil {
		t.Fatalf("legal state transition rejected: %v", err)
	}
	assertAborts("result_analyses_state_monotonic (terminal)",
		`UPDATE result_analyses SET state = 'running' WHERE analysis_id = ?`, hex64)

	assertAborts("analysis_segments_immutable",
		`UPDATE analysis_segments SET length_bytes = 200 WHERE analysis_id = ? AND ordinal = 0`, hex64)
	assertAborts("analysis_steps_completed_immutable",
		`UPDATE analysis_steps SET output_digest = ? WHERE analysis_id = ? AND step_id = 'leaf-0'`, strings.Repeat("f", 64), hex64)
	if _, err := db.ExecContext(t.Context(), `UPDATE analysis_steps SET state = 'claimed', generation = 1 WHERE analysis_id = ? AND step_id = 'reduce-0'`, hex64); err != nil {
		t.Fatalf("legal update on a non-completed step rejected: %v", err)
	}
	assertAborts("analysis_step_children_immutable",
		`UPDATE analysis_step_children SET ordinal = 1 WHERE analysis_id = ? AND parent_step_id = 'reduce-0'`, hex64)
	assertAborts("analysis_evidence_immutable",
		`UPDATE analysis_evidence SET excerpt_bytes = 'changed' WHERE analysis_id = ?`, hex64)
	assertAborts("analysis_bundles_immutable",
		`UPDATE analysis_bundles SET state = 'quarantined' WHERE analysis_id = ?`, hex64)
}

func TestMigrationV40UpgradePreservesV39State(t *testing.T) {
	path, raw := createSchemaAtVersion(t, 39)
	now := time.Now().UTC().UnixNano()
	insert := func(statement string, args ...any) {
		t.Helper()
		if _, err := raw.ExecContext(t.Context(), statement, args...); err != nil {
			raw.Close()
			t.Fatalf("seed v39 row: %v", err)
		}
	}
	insert(`INSERT INTO knowledge_claims (id, subject, predicate, value_kind, value_text, scope_kind, scope_id, source_class, source_ref, status, valid_from, valid_until, current_rev, created_at, updated_at)
		VALUES ('claim-a', 'api', 'is', 'string', 'x', 'project', 'p', 'human', 'r1', 'asserted', 0, 0, 1, ?, ?)`, now, now)
	// knowledge_claims_enqueue_after_insert already inserts the matching
	// knowledge_lexical_queue row; seeding it here a second time collides
	// with its UNIQUE(item_kind, item_id) index.
	insert(`INSERT INTO result_records (result_id, producer_kind, producer_id, producer_revision, storage_kind, storage_key,
		sha256, bytes, media_type, actor, team_id, conversation_key, project, retention_class, created_at, state)
		VALUES (?, 'tool_operation', 'op-1', 0, 'artifact', 'key-1', ?, 10, 'text/plain', 'U1', 'T1', 'c1', 'p', 'context', 1, 'available')`,
		strings.Repeat("a", 64), strings.Repeat("b", 64))
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	upgraded, err := OpenExisting(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	db := upgraded.DB()

	var version int
	if err := db.QueryRowContext(t.Context(), `PRAGMA user_version`).Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("upgraded version = %d, %v", version, err)
	}
	for table, want := range map[string]int{
		"knowledge_claims":        1,
		"knowledge_lexical_queue": 1,
		"result_records":          1,
	} {
		var count int
		if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM `+table).Scan(&count); err != nil || count != want {
			t.Fatalf("preserved %s rows = %d, %v", table, count, err)
		}
	}
	var claimSubject string
	if err := db.QueryRowContext(t.Context(), `SELECT subject FROM knowledge_claims WHERE id = 'claim-a'`).Scan(&claimSubject); err != nil || claimSubject != "api" {
		t.Fatalf("preserved claim subject = %q, %v", claimSubject, err)
	}
	var resultSHA string
	if err := db.QueryRowContext(t.Context(), `SELECT sha256 FROM result_records WHERE result_id = ?`, strings.Repeat("a", 64)).Scan(&resultSHA); err != nil || resultSHA != strings.Repeat("b", 64) {
		t.Fatalf("preserved result sha256 = %q, %v", resultSHA, err)
	}
	var analysisRows int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM result_analyses`).Scan(&analysisRows); err != nil || analysisRows != 0 {
		t.Fatalf("fresh v40 analysis table rows = %d, %v", analysisRows, err)
	}
}

// TestMigrationV40RejectedAsFutureSchema proves the same current > SchemaVersion
// mechanism TestOpenExistingRejectsFutureSchema pins generically: a binary
// whose SchemaVersion is lower than a database's PRAGMA user_version refuses
// to open it. createSchemaAtVersion always leaves PRAGMA user_version at its
// target version, so it can never itself produce a database claiming a
// version above this binary's own SchemaVersion. To exercise the concrete
// case, a fully migrated database is opened once to reach SchemaVersion,
// then its user_version is bumped one past that to simulate a database
// written by a future binary. The simulated future version is deliberately
// SchemaVersion+1, not a literal 41: this test predates v41 and must keep
// exercising "one past current" as SchemaVersion moves, not stay pinned to
// the value that was current when it was written.
func TestMigrationV40RejectedAsFutureSchema(t *testing.T) {
	path := t.TempDir() + "/future-v40.db"
	fresh, err := Initialize(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := fresh.Close(); err != nil {
		t.Fatal(err)
	}

	future := SchemaVersion + 1
	raw, err := sql.Open("sqlite", mustDataSourceName(t, path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(t.Context(), fmt.Sprintf(`PRAGMA user_version = %d`, future)); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenExisting(t.Context(), path)
	if store != nil {
		store.Close()
		t.Fatalf("OpenExisting succeeded for a SchemaVersion=%d binary opening a database claiming v%d", SchemaVersion, future)
	}
	var versionError *FutureSchemaError
	if !errors.As(err, &versionError) || versionError.Found != future {
		t.Fatalf("OpenExisting error = %v, want FutureSchemaError{Found: %d}", err, future)
	}
}

func TestMigrationV40CrashRollsBackAndReopens(t *testing.T) {
	path, raw := createSchemaAtVersion(t, 39)
	now := time.Now().UTC().UnixNano()
	if _, err := raw.ExecContext(t.Context(), `INSERT INTO knowledge_claims (id, subject, predicate, value_kind, value_text, scope_kind, scope_id, source_class, source_ref, status, created_at, updated_at)
		VALUES ('c1', 'api', 'is', 'string', 'x', 'project', 'p', 'human', 'r1', 'asserted', ?, ?)`, now, now); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	original := migrations[40]
	defer func() { migrations[40] = original }()
	migrations[40] = func(ctx context.Context, tx *sql.Tx) error {
		if err := migrateV40(ctx, tx); err != nil {
			return err
		}
		return errors.New("injected v40 crash")
	}
	store, err := OpenExisting(t.Context(), path)
	if store != nil {
		store.Close()
		t.Fatal("OpenExisting succeeded after injected v40 crash")
	}
	if err == nil || !strings.Contains(err.Error(), "injected v40 crash") {
		t.Fatalf("OpenExisting error = %v", err)
	}
	check, err := sql.Open("sqlite", mustDataSourceName(t, path))
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var version, objects int
	if err := check.QueryRowContext(t.Context(), `PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := check.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sqlite_schema WHERE name = 'result_analyses' OR name = 'analysis_steps'`).Scan(&objects); err != nil {
		t.Fatal(err)
	}
	if version != 39 || objects != 0 {
		t.Fatalf("rolled-back v40 state = version %d/objects %d", version, objects)
	}

	migrations[40] = original
	reopened, err := OpenExisting(t.Context(), path)
	if err != nil {
		t.Fatalf("reopen after crash: %v", err)
	}
	defer reopened.Close()
	if err := reopened.DB().QueryRowContext(t.Context(), `PRAGMA user_version`).Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("reopened version = %d, %v", version, err)
	}
	var claims int
	if err := reopened.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM knowledge_claims`).Scan(&claims); err != nil || claims != 1 {
		t.Fatalf("reopened preserved claims = %d, %v", claims, err)
	}
}

func TestMigrationV40ReopenPreservesRows(t *testing.T) {
	path := t.TempDir() + "/reopen-v40.db"
	store, err := Initialize(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Unix()
	hex64 := strings.Repeat("a", 64)
	insertResultAnalysis(t, store.DB(), hex64, now)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenExisting(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var rows int
	if err := reopened.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM result_analyses`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("reopened result_analyses rows = %d, %v", rows, err)
	}
}
