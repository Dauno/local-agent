package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildVersionFixture builds a real database at exactly `version` with the
// requested on-disk journal mode, closing every handle before returning.
func buildVersionFixture(t *testing.T, version int, journal string) string {
	t.Helper()
	path, raw := createSchemaAtVersion(t, version)
	if journal == "delete" {
		var mode string
		if err := raw.QueryRow("PRAGMA journal_mode = delete").Scan(&mode); err != nil || mode != "delete" {
			raw.Close()
			t.Fatalf("journal_mode=delete: mode=%q err=%v", mode, err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func fixtureDigest(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func readPragmas(t *testing.T, path string) (int, string) {
	t.Helper()
	raw, err := os.OpenFile(path, os.O_RDONLY, 0o400)
	if err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()
	db, err := open(context.Background(), path, "ro", false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	if err := db.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	var mode string
	if err := db.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	return version, mode
}

func TestOpenCurrentRejectsMissingAndDirectoryPaths(t *testing.T) {
	if _, err := OpenCurrent(context.Background(), filepath.Join(t.TempDir(), "missing.db")); !errors.Is(err, ErrDatabaseNotFound) {
		t.Fatalf("err = %v, want ErrDatabaseNotFound", err)
	}
	if _, err := OpenCurrent(context.Background(), t.TempDir()); err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("directory open err = %v, want directory guard error", err)
	}
}

// hasSidecar reports whether a SQLite sidecar file exists next to the
// database, checking the exact on-disk names (`<db>-wal`, `<db>-shm`).
func hasSidecar(t *testing.T, dbPath string) bool {
	t.Helper()
	for _, suffix := range []string{"-wal", "-shm"} {
		_, err := os.Stat(dbPath + suffix)
		if err == nil {
			return true
		}
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
	}
	return false
}

func TestOpenCurrentCurrentSchemaReturnsUsableStore(t *testing.T) {
	path := buildVersionFixture(t, SchemaVersion, "wal")
	store, err := OpenCurrent(context.Background(), path)
	if err != nil {
		t.Fatalf("OpenCurrent(v%d): %v", SchemaVersion, err)
	}
	defer store.Close()
	var version int
	if err := store.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("user_version=%d err=%v, want %d", version, err, SchemaVersion)
	}
	if err := store.db.PingContext(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}
}

// TestOpenCurrentRejectionLeavesV33FixtureUntouched is the FIND-148 gate:
// the rejection path never opens mode=rw, so the fixture keeps its exact
// bytes, its user_version, and its on-disk journal_mode=delete, and no WAL
// sidecar appears.
func TestOpenCurrentRejectionLeavesV33FixtureUntouched(t *testing.T) {
	path := buildVersionFixture(t, 33, "delete")
	before := fixtureDigest(t, path)

	store, err := OpenCurrent(context.Background(), path)
	if store != nil {
		store.Close()
		t.Fatal("OpenCurrent returned a store for a v33 fixture")
	}
	var upgrade *SchemaUpgradeRequiredError
	if !errors.As(err, &upgrade) || upgrade.Found != 33 || upgrade.Supported != SchemaVersion {
		t.Fatalf("err = %v, want SchemaUpgradeRequiredError{33, %d}", err, SchemaVersion)
	}
	if !errors.Is(err, ErrSchemaUpgradeRequired) {
		t.Fatalf("err = %v, must wrap ErrSchemaUpgradeRequired", err)
	}

	if after := fixtureDigest(t, path); after != before {
		t.Fatalf("fixture bytes changed: before=%s after=%s", before, after)
	}
	version, mode := readPragmas(t, path)
	if version != 33 || mode != "delete" {
		t.Fatalf("post-state user_version=%d journal_mode=%s, want 33/delete", version, mode)
	}
	if hasSidecar(t, path) {
		t.Fatal("sidecar created on rejection path")
	}
}

// TestSidecarGateDetectsExistingSidecar proves the sidecar gate is not a
// no-op: with either exact sidecar name present, the gate reports it
// (FIND-188).
func TestSidecarGateDetectsExistingSidecar(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture.db")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if hasSidecar(t, path) {
		t.Fatal("gate fired without any sidecar")
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.WriteFile(path+suffix, []byte("sidecar"), 0o600); err != nil {
			t.Fatal(err)
		}
		if !hasSidecar(t, path) {
			t.Fatalf("gate missed sidecar %q", suffix)
		}
		if err := os.Remove(path + suffix); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOpenCurrentFutureSchemaIsRejectedWithoutSideEffects(t *testing.T) {
	path := buildVersionFixture(t, SchemaVersion, "delete")
	// Rewind through a plain connection so the DSN never reapplies pragmas
	// and the file keeps its on-disk journal_mode=delete.
	plain, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plain.Exec("PRAGMA user_version = 42"); err != nil {
		t.Fatal(err)
	}
	if err := plain.Close(); err != nil {
		t.Fatal(err)
	}
	before := fixtureDigest(t, path)

	if store, err := OpenCurrent(context.Background(), path); store != nil {
		store.Close()
		t.Fatal("OpenCurrent returned a store for a future schema")
	} else {
		var future *FutureSchemaError
		if !errors.As(err, &future) || future.Found != 42 || future.Supported != SchemaVersion {
			t.Fatalf("err = %v, want FutureSchemaError{42, %d}", err, SchemaVersion)
		}
	}
	if after := fixtureDigest(t, path); after != before {
		t.Fatalf("fixture bytes changed: before=%s after=%s", before, after)
	}
	if _, mode := readPragmas(t, path); mode != "delete" {
		t.Fatalf("journal_mode=%s, want delete untouched", mode)
	}
}
