package sqlite

import (
	"path/filepath"
	"strings"
	"testing"
)

func seedRecoverableResultRow(t *testing.T, store *Store, ref string) {
	t.Helper()
	if _, err := store.DB().ExecContext(t.Context(), `
		INSERT INTO recoverable_results
			(ref, actor, conversation_key, kind, storage_locator, size_bytes, code_points, sha256, created_at, expires_at)
		VALUES (?, 'actor', 'conv', 'text', 'locator', 10, 10, ?, 100, 999999999999)`,
		ref, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
}

func seedRecoverableResultRefRow(t *testing.T, store *Store, ref, ownerKind, ownerID string) {
	t.Helper()
	if _, err := store.DB().ExecContext(t.Context(), `
		INSERT INTO recoverable_result_refs (ref, owner_kind, owner_id, created_at)
		VALUES (?, ?, ?, 100)`, ref, ownerKind, ownerID); err != nil {
		t.Fatal(err)
	}
}

func seedContinuityCapsuleRow(t *testing.T, store *Store, sessionID string) {
	t.Helper()
	if _, err := store.DB().ExecContext(t.Context(), `
		INSERT INTO continuity_capsules (session_id, revision, capsule_json, source_digest, covered_through, created_at, updated_at)
		VALUES (?, 1, '{}', 'digest', 0, 100, 100)`, sessionID); err != nil {
		t.Fatal(err)
	}
}

// TestCheckRecoverableReferenceHealthReportsBoundedCountsWithNoDangling
// builds a healthy relation: every ref row names a live recoverable_results
// row and a live durable owner (TRD 08 checkpoint 6, item 2).
func TestCheckRecoverableReferenceHealthReportsBoundedCountsWithNoDangling(t *testing.T) {
	ctx := t.Context()
	store, err := Create(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Create() = %v", err)
	}
	defer store.Close()

	ref := strings.Repeat("b", 64)
	seedRecoverableResultRow(t, store, ref)
	seedContinuityCapsuleRow(t, store, "session-1")
	seedRecoverableResultRefRow(t, store, ref, recoverableRefOwnerKindCapsule, "session-1")

	health, err := store.CheckRecoverableReferenceHealth(ctx)
	if err != nil {
		t.Fatalf("CheckRecoverableReferenceHealth() = %v", err)
	}
	if health.TotalRefRows != 1 || health.DistinctRefs != 1 || health.CapsuleOwners != 1 || health.EventOwners != 0 {
		t.Fatalf("health = %#v", health)
	}
	if health.DanglingRefs != 0 || health.DanglingOwners != 0 {
		t.Fatalf("healthy relation reported dangling rows: %#v", health)
	}
}

// TestCheckRecoverableReferenceHealthDetectsDanglingRef seeds a ref row
// whose ref never named a recoverable_results row.
func TestCheckRecoverableReferenceHealthDetectsDanglingRef(t *testing.T) {
	ctx := t.Context()
	store, err := Create(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Create() = %v", err)
	}
	defer store.Close()

	seedContinuityCapsuleRow(t, store, "session-1")
	seedRecoverableResultRefRow(t, store, strings.Repeat("c", 64), recoverableRefOwnerKindCapsule, "session-1")

	health, err := store.CheckRecoverableReferenceHealth(ctx)
	if err != nil {
		t.Fatalf("CheckRecoverableReferenceHealth() = %v", err)
	}
	if health.DanglingRefs != 1 {
		t.Fatalf("DanglingRefs = %d, want 1: %#v", health.DanglingRefs, health)
	}
	if health.DanglingOwners != 0 {
		t.Fatalf("DanglingOwners = %d, want 0: %#v", health.DanglingOwners, health)
	}
}

// TestCheckRecoverableReferenceHealthDetectsDanglingOwner seeds a ref row
// whose owner has no durable backing row.
func TestCheckRecoverableReferenceHealthDetectsDanglingOwner(t *testing.T) {
	ctx := t.Context()
	store, err := Create(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Create() = %v", err)
	}
	defer store.Close()

	ref := strings.Repeat("d", 64)
	seedRecoverableResultRow(t, store, ref)
	seedRecoverableResultRefRow(t, store, ref, recoverableRefOwnerKindCapsule, "ghost-session")

	health, err := store.CheckRecoverableReferenceHealth(ctx)
	if err != nil {
		t.Fatalf("CheckRecoverableReferenceHealth() = %v", err)
	}
	if health.DanglingOwners != 1 {
		t.Fatalf("DanglingOwners = %d, want 1: %#v", health.DanglingOwners, health)
	}
	if health.DanglingRefs != 0 {
		t.Fatalf("DanglingRefs = %d, want 0: %#v", health.DanglingRefs, health)
	}
}
