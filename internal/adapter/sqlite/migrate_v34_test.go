package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestMigrationV34FreshCreatesResultCatalog(t *testing.T) {
	ctx := context.Background()
	store, err := Initialize(ctx, filepath.Join(t.TempDir(), "v34.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	var version int
	if err := store.DB().QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersion {
		t.Fatalf("schema version = %d, want %d", version, SchemaVersion)
	}
	for _, table := range []string{
		"result_records",
		"result_representations",
		"result_references",
		"result_materializations",
		"workstream_result_link_results",
	} {
		var name string
		if err := store.DB().QueryRowContext(ctx, `SELECT name FROM sqlite_schema WHERE type = 'table' AND name = ?`, table).Scan(&name); err != nil {
			t.Fatalf("result catalog table %q: %v", table, err)
		}
	}
}

func TestMigrationV34UpgradesV33WithoutRewritingLegacyResultLink(t *testing.T) {
	ctx := context.Background()
	path, raw := createSchemaAtVersion(t, 33)
	seedV33LegacyResultLink(t, raw)
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenExisting(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	var identity string
	if err := store.DB().QueryRowContext(ctx, `SELECT result_identity FROM workstream_result_links
		WHERE workstream_id = 'ws-legacy' AND result_link_id = 'link-legacy'`).Scan(&identity); err != nil {
		t.Fatal(err)
	}
	if identity != "legacy-projection-ref" {
		t.Fatalf("legacy result link identity = %q", identity)
	}
	var bindings int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM workstream_result_link_results`).Scan(&bindings); err != nil {
		t.Fatal(err)
	}
	if bindings != 0 {
		t.Fatalf("legacy v33 link was normalized into %d v34 bindings", bindings)
	}
}

func TestMigrationV34RollbackAndReopenPreserveV33(t *testing.T) {
	ctx := context.Background()
	path, raw := createSchemaAtVersion(t, 33)
	seedV33LegacyResultLink(t, raw)
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	original := migrations[34]
	migrations[34] = func(ctx context.Context, tx *sql.Tx) error {
		if err := migrateV34(ctx, tx); err != nil {
			return err
		}
		return errors.New("injected v34 crash")
	}
	defer func() { migrations[34] = original }()

	store, err := OpenExisting(ctx, path)
	if store != nil {
		_ = store.Close()
		t.Fatal("OpenExisting succeeded after injected v34 crash")
	}
	if err == nil || !strings.Contains(err.Error(), "injected v34 crash") {
		t.Fatalf("OpenExisting error = %v", err)
	}

	raw, err = sql.Open("sqlite", mustDataSourceName(t, path))
	if err != nil {
		t.Fatal(err)
	}
	var version, catalogTables int
	if err := raw.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	if err := raw.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_schema
		WHERE type = 'table' AND name IN ('result_records', 'result_representations', 'result_references', 'result_materializations', 'workstream_result_link_results')`).Scan(&catalogTables); err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	if version != 33 || catalogTables != 0 {
		_ = raw.Close()
		t.Fatalf("failed migration left version/tables = %d/%d, want 33/0", version, catalogTables)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	migrations[34] = original
	store, err = OpenExisting(ctx, path)
	if err != nil {
		t.Fatalf("reopen after v34 crash: %v", err)
	}
	defer func() { _ = store.Close() }()
	if err := store.DB().QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersion {
		t.Fatalf("schema version after reopen = %d, want %d", version, SchemaVersion)
	}
}

func TestMigrationV34CatalogConstraintsIndexesAndReservationCAS(t *testing.T) {
	ctx := context.Background()
	store, err := Initialize(ctx, filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	db := store.DB()
	resultID := strings.Repeat("a", 64)
	secondID := strings.Repeat("b", 64)
	digest := strings.Repeat("c", 64)

	_, err = db.ExecContext(ctx, `INSERT INTO result_records (
		result_id, producer_kind, producer_id, producer_revision, storage_kind, storage_key,
		sha256, bytes, media_type, actor, team_id, conversation_key, project,
		retention_class, created_at, state)
		VALUES (?, 'acp_job', 'job-1', 4, 'artifact', 'job-1-delivery.result', ?, 7,
		'text/plain; charset=utf-8', 'U1', 'T1', 'slack:T1:dm:D1', 'app', 'workstream', 1, 'available')`, resultID, digest)
	if err != nil {
		t.Fatalf("insert result record: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO result_records (
		result_id, producer_kind, producer_id, producer_revision, storage_kind, storage_key,
		sha256, bytes, media_type, actor, team_id, conversation_key, project,
		retention_class, created_at, state)
		VALUES (?, 'acp_job', 'job-1', 4, 'artifact', 'other.result', ?, 7,
		'text/plain', 'U1', 'T1', 'slack:T1:dm:D1', 'app', 'workstream', 2, 'available')`, secondID, digest); err == nil {
		t.Fatal("duplicate producer lineage was accepted")
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO result_records (
		result_id, producer_kind, producer_id, producer_revision, storage_kind, storage_key,
		sha256, bytes, media_type, actor, team_id, conversation_key, project,
		retention_class, created_at, state)
		VALUES ('bad', 'acp_job', 'job-2', 1, 'artifact', 'job-2-delivery.result', ?, 7,
		'text/plain', 'U1', 'T1', 'slack:T1:dm:D1', 'app', 'workstream', 1, 'available')`, digest); err == nil {
		t.Fatal("invalid opaque result ID was accepted")
	}

	if _, err := db.ExecContext(ctx, `INSERT INTO result_materializations (
		producer_kind, producer_id, producer_revision, result_id, state, created_at, updated_at)
		VALUES ('tool_operation', 'operation-1', 1, ?, 'reserved', 1, 1)`, secondID); err != nil {
		t.Fatalf("reserve materialization: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO result_materializations (
		producer_kind, producer_id, producer_revision, result_id, state, created_at, updated_at)
		VALUES ('tool_operation', 'operation-1', 1, ?, 'reserved', 1, 1)`, strings.Repeat("d", 64)); err == nil {
		t.Fatal("duplicate materialization reservation was accepted")
	}
	updated, err := db.ExecContext(ctx, `UPDATE result_materializations
		SET state = 'payload_published', storage_kind = 'recoverable', storage_key = 'private-key', updated_at = 2
		WHERE producer_kind = 'tool_operation' AND producer_id = 'operation-1' AND producer_revision = 1 AND state = 'reserved'`)
	if err != nil {
		t.Fatalf("advance reservation: %v", err)
	}
	if rows, err := updated.RowsAffected(); err != nil || rows != 1 {
		t.Fatalf("first reservation CAS rows/error = %d/%v", rows, err)
	}
	updated, err = db.ExecContext(ctx, `UPDATE result_materializations
		SET state = 'payload_published', updated_at = 3
		WHERE producer_kind = 'tool_operation' AND producer_id = 'operation-1' AND producer_revision = 1 AND state = 'reserved'`)
	if err != nil {
		t.Fatalf("repeat reservation CAS: %v", err)
	}
	if rows, err := updated.RowsAffected(); err != nil || rows != 0 {
		t.Fatalf("repeat reservation CAS rows/error = %d/%v, want 0/nil", rows, err)
	}

	for _, index := range []string{
		"result_records_by_scope",
		"result_records_by_retention_state",
		"result_representations_by_result",
		"result_references_live_by_result",
		"result_references_live_by_owner",
		"result_materializations_by_state",
		"result_records_by_producer",
		"workstream_result_links_by_identity",
	} {
		var name string
		if err := db.QueryRowContext(ctx, `SELECT name FROM sqlite_schema WHERE type = 'index' AND name = ?`, index).Scan(&name); err != nil {
			t.Fatalf("required index %q: %v", index, err)
		}
	}
}

func TestMigrationV34ResultIdentitiesAndRepresentationsAreImmutable(t *testing.T) {
	ctx := context.Background()
	store, err := Initialize(ctx, filepath.Join(t.TempDir(), "immutable.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	db := store.DB()
	resultID := strings.Repeat("a", 64)
	representationID := strings.Repeat("b", 64)
	insertV34ResultRecord(t, db, resultID, "acp_job", "job-immutable", 1, "artifact", "immutable.result")

	if _, err := db.ExecContext(ctx, `UPDATE result_records SET sha256 = ? WHERE result_id = ?`, strings.Repeat("d", 64), resultID); err == nil {
		t.Fatal("immutable result digest was updated")
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO result_representations (
		representation_id, result_id, kind, state, source_sha256, source_bytes,
		algorithm_or_prompt_version, payload_sha256, payload_bytes, created_at)
		VALUES (?, ?, 'producer_handoff_v1', 'available', ?, 7, 'handoff-v1', ?, 3, 1)`,
		representationID, resultID, strings.Repeat("f", 64), strings.Repeat("d", 64)); err != nil {
		t.Fatalf("insert representation: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE result_representations SET payload_sha256 = ? WHERE representation_id = ?`, strings.Repeat("e", 64), representationID); err == nil {
		t.Fatal("immutable representation digest was updated")
	}
	if _, err := db.ExecContext(ctx, `UPDATE result_representations SET state = 'quarantined' WHERE representation_id = ?`, representationID); err != nil {
		t.Fatalf("quarantine representation: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE result_representations SET state = 'available' WHERE representation_id = ?`, representationID); err == nil {
		t.Fatal("quarantined representation returned to available")
	}
	if _, err := db.ExecContext(ctx, `UPDATE result_records SET state = 'quarantined' WHERE result_id = ?`, resultID); err != nil {
		t.Fatalf("quarantine result: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE result_records SET state = 'available' WHERE result_id = ?`, resultID); err == nil {
		t.Fatal("quarantined result returned to available")
	}
}

func TestMigrationV34WorkstreamBindingRequiresExactParentIdentity(t *testing.T) {
	ctx := context.Background()
	store, err := Initialize(ctx, filepath.Join(t.TempDir(), "binding.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	db := store.DB()
	resultID := strings.Repeat("a", 64)
	otherID := strings.Repeat("b", 64)
	insertV34ResultRecord(t, db, resultID, "acp_job", "job-binding", 1, "artifact", "binding.result")
	insertV34ResultRecord(t, db, otherID, "acp_job", "job-other", 1, "artifact", "other.result")

	if _, err := db.ExecContext(ctx, `INSERT INTO workstreams (
		workstream_id, conversation_key, owner_actor, project, status, revision,
		objective, current_phase, continuation_of, created_at, updated_at)
		VALUES ('ws-binding', 'slack:T1:dm:D1', 'U1', 'app', 'completed', 1,
		'bind exact result', '', '', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO workstream_result_links (
		workstream_id, result_link_id, task_id, result_identity, description)
		VALUES ('ws-binding', 'link-1', '', ?, '')`, resultID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO workstream_result_link_results (
		workstream_id, result_link_id, result_id, verified_at)
		VALUES ('ws-binding', 'link-1', ?, 1)`, otherID); err == nil {
		t.Fatal("binding to a different result than the parent link was accepted")
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO workstream_result_link_results (
		workstream_id, result_link_id, result_id, verified_at)
		VALUES ('ws-binding', 'link-1', ?, 1)`, resultID); err != nil {
		t.Fatalf("insert exact binding: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE workstream_result_link_results SET verified_at = 2
		WHERE workstream_id = 'ws-binding' AND result_link_id = 'link-1'`); err == nil {
		t.Fatal("verified binding was updated")
	}
}

func TestMigrationV34MaterializationStateMachine(t *testing.T) {
	ctx := context.Background()
	store, err := Initialize(ctx, filepath.Join(t.TempDir(), "materialization.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	db := store.DB()
	resultID := strings.Repeat("a", 64)

	if _, err := db.ExecContext(ctx, `INSERT INTO result_materializations (
		producer_kind, producer_id, producer_revision, result_id, state,
		storage_kind, storage_key, created_at, updated_at)
		VALUES ('tool_operation', 'invalid-reserved', 1, ?, 'reserved', 'recoverable', 'key', 1, 1)`, resultID); err == nil {
		t.Fatal("reserved materialization with published storage was accepted")
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO result_materializations (
		producer_kind, producer_id, producer_revision, result_id, state, created_at, updated_at)
		VALUES ('tool_operation', 'invalid-published', 1, ?, 'payload_published', 1, 1)`, resultID); err == nil {
		t.Fatal("published materialization without storage was accepted")
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO result_materializations (
		producer_kind, producer_id, producer_revision, result_id, state,
		storage_kind, storage_key, created_at, updated_at)
		VALUES ('tool_operation', 'skip-reservation', 1, ?, 'payload_published', 'recoverable', 'key', 1, 1)`, resultID); err == nil {
		t.Fatal("materialization skipped its durable reservation")
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO result_materializations (
		producer_kind, producer_id, producer_revision, result_id, state, created_at, updated_at)
		VALUES ('tool_operation', 'operation-state', 1, ?, 'reserved', 1, 1)`, resultID); err != nil {
		t.Fatalf("reserve materialization: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE result_materializations
		SET state = 'payload_published', storage_kind = 'recoverable', storage_key = 'operation.result', updated_at = 2
		WHERE producer_kind = 'tool_operation' AND producer_id = 'operation-state' AND producer_revision = 1`); err != nil {
		t.Fatalf("publish materialization: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE result_materializations SET state = 'reserved', storage_kind = '', storage_key = '', updated_at = 3
		WHERE producer_kind = 'tool_operation' AND producer_id = 'operation-state' AND producer_revision = 1`); err == nil {
		t.Fatal("published materialization rewound to reserved")
	}
	if _, err := db.ExecContext(ctx, `UPDATE result_materializations SET state = 'committed', updated_at = 3
		WHERE producer_kind = 'tool_operation' AND producer_id = 'operation-state' AND producer_revision = 1`); err == nil {
		t.Fatal("materialization committed without matching result record")
	}
	insertV34ResultRecord(t, db, resultID, "tool_operation", "operation-state", 1, "recoverable", "operation.result")
	if _, err := db.ExecContext(ctx, `UPDATE result_materializations SET state = 'committed', updated_at = 3
		WHERE producer_kind = 'tool_operation' AND producer_id = 'operation-state' AND producer_revision = 1`); err != nil {
		t.Fatalf("commit matching materialization: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE result_materializations SET state = 'payload_published', updated_at = 4
		WHERE producer_kind = 'tool_operation' AND producer_id = 'operation-state' AND producer_revision = 1`); err == nil {
		t.Fatal("committed materialization was reopened")
	}
}

func TestMigrationV34ReferenceLifecycleIsBoundAndMonotonic(t *testing.T) {
	ctx := context.Background()
	store, err := Initialize(ctx, filepath.Join(t.TempDir(), "references.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	db := store.DB()
	resultID := strings.Repeat("a", 64)
	otherID := strings.Repeat("b", 64)
	referenceID := strings.Repeat("c", 64)
	insertV34ResultRecord(t, db, resultID, "acp_job", "job-reference", 1, "artifact", "reference.result")
	insertV34ResultRecord(t, db, otherID, "acp_job", "job-reference-other", 1, "artifact", "reference-other.result")

	if _, err := db.ExecContext(ctx, `INSERT INTO result_references (
		reference_id, result_id, owner_kind, owner_id, state, created_at)
		VALUES (?, ?, 'workstream_result_link', 'ws-1/link-1', 'live', 1)`, referenceID, resultID); err != nil {
		t.Fatalf("insert live reference: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE result_references SET result_id = ? WHERE reference_id = ?`, otherID, referenceID); err == nil {
		t.Fatal("live reference was retargeted")
	}
	if _, err := db.ExecContext(ctx, `UPDATE result_references SET state = 'released', released_at = 2 WHERE reference_id = ?`, referenceID); err != nil {
		t.Fatalf("release reference: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE result_references SET state = 'live', released_at = 0 WHERE reference_id = ?`, referenceID); err == nil {
		t.Fatal("released reference was reactivated")
	}
	if _, err := db.ExecContext(ctx, `UPDATE result_references SET owner_id = 'other-owner' WHERE reference_id = ?`, referenceID); err == nil {
		t.Fatal("released reference owner was changed")
	}
}

func TestMigrationV34RejectsFutureSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "future-v34.db")
	store, err := Initialize(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", mustDataSourceName(t, path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", SchemaVersion+1)); err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenExisting(ctx, path)
	if store != nil {
		_ = store.Close()
		t.Fatal("OpenExisting accepted a future schema")
	}
	if !errors.Is(err, ErrFutureSchema) {
		t.Fatalf("OpenExisting future schema error = %v", err)
	}
}

func seedV33LegacyResultLink(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `INSERT INTO workstreams (
		workstream_id, conversation_key, owner_actor, project, status, revision,
		objective, current_phase, continuation_of, created_at, updated_at)
		VALUES ('ws-legacy', 'slack:T1:dm:D1', 'U1', 'app', 'completed', 1,
		'legacy objective', '', '', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO workstream_result_links (
		workstream_id, result_link_id, task_id, result_identity, description)
		VALUES ('ws-legacy', 'link-legacy', '', 'legacy-projection-ref', 'historical evidence')`); err != nil {
		t.Fatal(err)
	}
}

func insertV34ResultRecord(t *testing.T, db *sql.DB, resultID, producerKind, producerID string, producerRevision int, storageKind, storageKey string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), `INSERT INTO result_records (
		result_id, producer_kind, producer_id, producer_revision, storage_kind, storage_key,
		sha256, bytes, media_type, actor, team_id, conversation_key, project,
		retention_class, created_at, state)
		VALUES (?, ?, ?, ?, ?, ?, ?, 7, 'text/plain; charset=utf-8',
		'U1', 'T1', 'slack:T1:dm:D1', 'app', 'workstream', 1, 'available')`,
		resultID, producerKind, producerID, producerRevision, storageKind, storageKey, strings.Repeat("f", 64))
	if err != nil {
		t.Fatalf("insert v34 result record %q: %v", resultID, err)
	}
}
