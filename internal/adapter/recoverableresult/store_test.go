package recoverableresult_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Dauno/slack-local-agent/internal/adapter/recoverableresult"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"

	_ "modernc.org/sqlite"
)

func setupTestDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.ExecContext(t.Context(), `CREATE TABLE recoverable_results (
		ref TEXT PRIMARY KEY,
		actor TEXT NOT NULL,
		conversation_key TEXT NOT NULL,
		kind TEXT NOT NULL,
		storage_locator TEXT NOT NULL,
		size_bytes INTEGER NOT NULL,
		code_points INTEGER NOT NULL,
		sha256 TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		expires_at INTEGER NOT NULL,
		cleanup_claim TEXT NOT NULL DEFAULT '',
		cleanup_version INTEGER NOT NULL DEFAULT 0,
		cleanup_claimed_at INTEGER NOT NULL DEFAULT 0,
		CHECK (length(ref) > 0),
		CHECK (length(actor) > 0),
		CHECK (length(conversation_key) > 0),
		CHECK (length(kind) > 0),
		CHECK (size_bytes > 0),
		CHECK (length(sha256) == 64),
		CHECK (cleanup_version >= 0),
		CHECK (cleanup_claimed_at >= 0),
		CHECK (expires_at > created_at)
	) WITHOUT ROWID`); err != nil {
		t.Fatal(err)
	}

	if _, err := db.ExecContext(t.Context(), `CREATE INDEX recoverable_results_by_expiry ON recoverable_results (expires_at)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `CREATE INDEX recoverable_results_by_conversation ON recoverable_results (conversation_key, actor)`); err != nil {
		t.Fatal(err)
	}

	// Enable WAL for test isolation
	if _, err := db.ExecContext(t.Context(), "PRAGMA journal_mode=WAL"); err != nil {
		t.Logf("WAL not available: %v", err)
	}
	return db, dir
}

func TestPutAndStatRoundTrip(t *testing.T) {
	db, dir := setupTestDB(t)
	storageDir := filepath.Join(dir, "results")
	store := recoverableresult.NewStore(db, storageDir, 1024*1024, 4096, 7, 100)

	ctx := t.Context()
	result, err := store.Put(ctx, port.PutResultRequest{
		Actor:           "U123",
		ConversationKey: "slack:T:dm:D",
		Kind:            "acp_result",
		Content:         "Hello World",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Ref == "" {
		t.Fatal("ref must not be empty")
	}
	if len(result.Ref) != 64 {
		t.Fatalf("ref length = %d, want 64", len(result.Ref))
	}
	if result.Actor != "U123" {
		t.Fatalf("actor = %q", result.Actor)
	}
	if result.ConversationKey != "slack:T:dm:D" {
		t.Fatalf("conversation_key = %q", result.ConversationKey)
	}
	if result.Kind != "acp_result" {
		t.Fatalf("kind = %q", result.Kind)
	}
	if result.SizeBytes != int64(len("Hello World")) {
		t.Fatalf("size_bytes = %d", result.SizeBytes)
	}
	if result.CodePoints != 11 {
		t.Fatalf("code_points = %d, want 11", result.CodePoints)
	}
	if result.SHA256 == "" || len(result.SHA256) != 64 {
		t.Fatalf("sha256 = %q", result.SHA256)
	}
	if result.CreatedAt.IsZero() {
		t.Fatal("created_at is zero")
	}
	if result.ExpiresAt.Before(result.CreatedAt) {
		t.Fatal("expires_at before created_at")
	}

	stat, err := store.Stat(ctx, port.StatResultRequest{
		Ref:             result.Ref,
		Actor:           "U123",
		ConversationKey: "slack:T:dm:D",
	})
	if err != nil {
		t.Fatal(err)
	}
	if stat.Ref != result.Ref {
		t.Fatalf("stat ref = %q, want %q", stat.Ref, result.Ref)
	}
	if stat.SizeBytes != result.SizeBytes {
		t.Fatalf("stat size_bytes = %d, want %d", stat.SizeBytes, result.SizeBytes)
	}
	if stat.SHA256 != result.SHA256 {
		t.Fatalf("stat sha256 mismatch")
	}
}

func TestReadChunkFull(t *testing.T) {
	db, dir := setupTestDB(t)
	storageDir := filepath.Join(dir, "results")
	store := recoverableresult.NewStore(db, storageDir, 1024*1024, 4096, 7, 100)

	ctx := t.Context()
	const content = "Hello, World! This is a test result."
	result, err := store.Put(ctx, port.PutResultRequest{
		Actor:           "U123",
		ConversationKey: "slack:T:dm:D",
		Kind:            "acp_result",
		Content:         content,
	})
	if err != nil {
		t.Fatal(err)
	}

	chunk, err := store.ReadChunk(ctx, domain.ResultChunkRequest{
		Ref:             result.Ref,
		Actor:           "U123",
		ConversationKey: "slack:T:dm:D",
		OffsetBytes:     0,
		MaxBytes:        4096,
	})
	if err != nil {
		t.Fatal(err)
	}
	if chunk.Content != content {
		t.Fatalf("content = %q, want %q", chunk.Content, content)
	}
	if chunk.OffsetBytes != 0 {
		t.Fatalf("offset = %d", chunk.OffsetBytes)
	}
	if !chunk.EOF {
		t.Fatal("expected EOF")
	}
	if chunk.SHA256 != result.SHA256 {
		t.Fatalf("sha256 mismatch")
	}
}

func TestReadChunkPartialSequential(t *testing.T) {
	db, dir := setupTestDB(t)
	storageDir := filepath.Join(dir, "results")
	store := recoverableresult.NewStore(db, storageDir, 1024*1024, 10, 7, 100)

	ctx := t.Context()
	content := "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	result, err := store.Put(ctx, port.PutResultRequest{
		Actor:           "U123",
		ConversationKey: "slack:T:dm:D",
		Kind:            "acp_result",
		Content:         content,
	})
	if err != nil {
		t.Fatal(err)
	}

	var assembled strings.Builder
	offset := int64(0)
	chunks := 0
	for {
		chunk, err := store.ReadChunk(ctx, domain.ResultChunkRequest{
			Ref:             result.Ref,
			Actor:           "U123",
			ConversationKey: "slack:T:dm:D",
			OffsetBytes:     offset,
			MaxBytes:        10,
		})
		if err != nil {
			t.Fatal(err)
		}
		assembled.WriteString(chunk.Content)
		chunks++
		if chunk.EOF {
			break
		}
		offset = chunk.NextOffsetBytes
	}
	if assembled.String() != content {
		t.Fatalf("assembled = %q, want %q", assembled.String(), content)
	}
	if chunks < 2 {
		t.Fatalf("partial chunks = %d, want at least 2", chunks)
	}
}

func TestUTF8Boundaries(t *testing.T) {
	db, dir := setupTestDB(t)
	storageDir := filepath.Join(dir, "results")
	store := recoverableresult.NewStore(db, storageDir, 1024*1024, 32, 7, 100)

	ctx := t.Context()
	content := "a🔥b"
	result, err := store.Put(ctx, port.PutResultRequest{
		Actor:           "U123",
		ConversationKey: "slack:T:dm:D",
		Kind:            "acp_result",
		Content:         content,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Bytes: 'a'=0, '🔥'=1-4, 'b'=5
	// Valid UTF-8 boundaries: 0, 1, 5, 6
	validOffsets := []int64{0, 1, 5, 6}
	for _, offset := range validOffsets {
		chunk, err := store.ReadChunk(ctx, domain.ResultChunkRequest{
			Ref:             result.Ref,
			Actor:           "U123",
			ConversationKey: "slack:T:dm:D",
			OffsetBytes:     offset,
			MaxBytes:        32,
		})
		if err != nil {
			t.Fatalf("valid offset %d: %v", offset, err)
		}
		if !utf8.ValidString(chunk.Content) {
			t.Fatalf("offset %d: chunk content is not valid UTF-8", offset)
		}
	}

	// Invalid UTF-8 boundaries (mid-rune in emoji): 2, 3, 4
	invalidOffsets := []int64{2, 3, 4}
	for _, offset := range invalidOffsets {
		_, err := store.ReadChunk(ctx, domain.ResultChunkRequest{
			Ref:             result.Ref,
			Actor:           "U123",
			ConversationKey: "slack:T:dm:D",
			OffsetBytes:     offset,
			MaxBytes:        32,
		})
		if err == nil {
			t.Fatalf("expected error for invalid offset %d (mid-rune)", offset)
		}
	}
}

func TestReadChunkRejectsNegativeAndPastEndOffsets(t *testing.T) {
	db, dir := setupTestDB(t)
	store := recoverableresult.NewStore(db, filepath.Join(dir, "results"), 1024, 32, 7, 100)
	result, err := store.Put(t.Context(), port.PutResultRequest{
		Actor: "U123", ConversationKey: "slack:T:dm:D", Kind: "test", Content: "abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, offset := range []int64{-1, 4} {
		_, err := store.ReadChunk(t.Context(), domain.ResultChunkRequest{
			Ref: result.Ref, Actor: "U123", ConversationKey: "slack:T:dm:D", OffsetBytes: offset, MaxBytes: 32,
		})
		if err == nil {
			t.Fatalf("offset %d should be rejected", offset)
		}
	}
}

func TestReadChunkRejectsLimitSmallerThanRune(t *testing.T) {
	db, dir := setupTestDB(t)
	store := recoverableresult.NewStore(db, filepath.Join(dir, "results"), 1024, 8, 7, 100)
	result, err := store.Put(t.Context(), port.PutResultRequest{
		Actor: "U123", ConversationKey: "slack:T:dm:D", Kind: "test", Content: "🔥",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadChunk(t.Context(), domain.ResultChunkRequest{
		Ref: result.Ref, Actor: "U123", ConversationKey: "slack:T:dm:D", MaxBytes: 1,
	}); err == nil {
		t.Fatal("chunk limit smaller than one rune should be rejected")
	}
	chunk, err := store.ReadChunk(t.Context(), domain.ResultChunkRequest{
		Ref: result.Ref, Actor: "U123", ConversationKey: "slack:T:dm:D", MaxBytes: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if chunk.Content != "🔥" || !chunk.EOF {
		t.Fatalf("chunk = %#v, want complete rune at EOF", chunk)
	}
}

func TestMissingRefReturnsUnavailable(t *testing.T) {
	db, dir := setupTestDB(t)
	storageDir := filepath.Join(dir, "results")
	store := recoverableresult.NewStore(db, storageDir, 1024*1024, 4096, 7, 100)

	ctx := t.Context()
	_, err := store.ReadChunk(ctx, domain.ResultChunkRequest{
		Ref:             "nonexistent_ref_hex_hex_hex_hex_hex_hex_hex_hex__",
		Actor:           "U123",
		ConversationKey: "slack:T:dm:D",
		OffsetBytes:     0,
		MaxBytes:        4096,
	})
	if err == nil {
		t.Fatal("expected error for missing ref")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "unavailable") {
		t.Fatalf("error does not contain 'unavailable': %v", err)
	}
}

func TestOwnerMismatch(t *testing.T) {
	db, dir := setupTestDB(t)
	storageDir := filepath.Join(dir, "results")
	store := recoverableresult.NewStore(db, storageDir, 1024*1024, 4096, 7, 100)

	ctx := t.Context()
	result, err := store.Put(ctx, port.PutResultRequest{
		Actor:           "U123",
		ConversationKey: "slack:T:dm:D",
		Kind:            "acp_result",
		Content:         "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = store.ReadChunk(ctx, domain.ResultChunkRequest{
		Ref:             result.Ref,
		Actor:           "U999", // different actor
		ConversationKey: "slack:T:dm:D",
		OffsetBytes:     0,
		MaxBytes:        4096,
	})
	if err == nil {
		t.Fatal("expected error for actor mismatch")
	}
}

func TestConversationMismatch(t *testing.T) {
	db, dir := setupTestDB(t)
	storageDir := filepath.Join(dir, "results")
	store := recoverableresult.NewStore(db, storageDir, 1024*1024, 4096, 7, 100)

	ctx := t.Context()
	result, err := store.Put(ctx, port.PutResultRequest{
		Actor:           "U123",
		ConversationKey: "slack:T:dm:D",
		Kind:            "acp_result",
		Content:         "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = store.ReadChunk(ctx, domain.ResultChunkRequest{
		Ref:             result.Ref,
		Actor:           "U123",
		ConversationKey: "slack:T:dm:Z99", // different conversation
		OffsetBytes:     0,
		MaxBytes:        4096,
	})
	if err == nil {
		t.Fatal("expected error for conversation mismatch")
	}
}

func TestSHA256Integrity(t *testing.T) {
	db, dir := setupTestDB(t)
	storageDir := filepath.Join(dir, "results")
	store := recoverableresult.NewStore(db, storageDir, 1024*1024, 4096, 7, 100)

	ctx := t.Context()
	content := "verify my integrity"
	result, err := store.Put(ctx, port.PutResultRequest{
		Actor:           "U123",
		ConversationKey: "slack:T:dm:D",
		Kind:            "acp_result",
		Content:         content,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Corrupt the file on disk
	filePath := filepath.Join(storageDir, result.Ref)
	origData, err := os.ReadFile(filePath)
	if err != nil {
		// The file might be named by storage_locator, not ref.
		// Let's find it by listing dir.
		entries, listErr := os.ReadDir(storageDir)
		if listErr != nil {
			t.Fatal(listErr)
		}
		var foundPath string
		for _, e := range entries {
			if e.Type().IsRegular() {
				foundPath = filepath.Join(storageDir, e.Name())
				break
			}
		}
		if foundPath == "" {
			t.Fatal("no storage file found")
		}
		origData, err = os.ReadFile(foundPath)
		if err != nil {
			t.Fatal(err)
		}
		filePath = foundPath
	}
	_ = origData

	// Corrupt the first byte
	if err := os.WriteFile(filePath, []byte("X"+content[1:]), 0600); err != nil {
		t.Fatal(err)
	}

	_, err = store.ReadChunk(ctx, domain.ResultChunkRequest{
		Ref:             result.Ref,
		Actor:           "U123",
		ConversationKey: "slack:T:dm:D",
		OffsetBytes:     0,
		MaxBytes:        4096,
	})
	if err == nil {
		t.Fatal("expected error for corrupted file")
	}
	// Error should be opaque - contain "unavailable"
	if !strings.Contains(strings.ToLower(err.Error()), "unavailable") {
		t.Fatalf("error does not contain 'unavailable': %v", err)
	}
	if _, err := store.Stat(ctx, port.StatResultRequest{
		Ref: result.Ref, Actor: "U123", ConversationKey: "slack:T:dm:D",
	}); err == nil {
		t.Fatal("Stat should verify content integrity")
	}
}

func TestReadRejectsSymlinkStorageFile(t *testing.T) {
	db, dir := setupTestDB(t)
	storageDir := filepath.Join(dir, "results")
	store := recoverableresult.NewStore(db, storageDir, 1024, 32, 7, 100)
	result, err := store.Put(t.Context(), port.PutResultRequest{
		Actor: "U123", ConversationKey: "slack:T:dm:D", Kind: "test", Content: "same content",
	})
	if err != nil {
		t.Fatal(err)
	}
	var locator string
	if err := db.QueryRowContext(t.Context(), `SELECT storage_locator FROM recoverable_results WHERE ref = ?`, result.Ref).Scan(&locator); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("same content"), 0o600); err != nil {
		t.Fatal(err)
	}
	storagePath := filepath.Join(storageDir, locator)
	if err := os.Remove(storagePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, storagePath); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadChunk(t.Context(), domain.ResultChunkRequest{
		Ref: result.Ref, Actor: "U123", ConversationKey: "slack:T:dm:D", MaxBytes: 32,
	}); err == nil {
		t.Fatal("symlink storage file should be rejected")
	}
}

func TestStatRejectsMetadataSizeMismatch(t *testing.T) {
	db, dir := setupTestDB(t)
	store := recoverableresult.NewStore(db, filepath.Join(dir, "results"), 1024, 32, 7, 100)
	result, err := store.Put(t.Context(), port.PutResultRequest{
		Actor: "U123", ConversationKey: "slack:T:dm:D", Kind: "test", Content: "content",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `UPDATE recoverable_results SET size_bytes = size_bytes + 1 WHERE ref = ?`, result.Ref); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Stat(t.Context(), port.StatResultRequest{
		Ref: result.Ref, Actor: "U123", ConversationKey: "slack:T:dm:D",
	}); err == nil {
		t.Fatal("Stat should verify metadata size")
	}
}

func TestMaxResultBytesEnforcement(t *testing.T) {
	db, dir := setupTestDB(t)
	storageDir := filepath.Join(dir, "results")
	store := recoverableresult.NewStore(db, storageDir, 10, 4096, 7, 100) // max 10 bytes

	ctx := t.Context()
	_, err := store.Put(ctx, port.PutResultRequest{
		Actor:           "U123",
		ConversationKey: "slack:T:dm:D",
		Kind:            "acp_result",
		Content:         strings.Repeat("x", 11),
	})
	if err == nil {
		t.Fatal("expected error for oversized result")
	}
}

func TestDeleteExpired(t *testing.T) {
	db, dir := setupTestDB(t)
	storageDir := filepath.Join(dir, "results")
	store := recoverableresult.NewStore(db, storageDir, 1024*1024, 4096, 7, 100)

	ctx := t.Context()
	result1, err := store.Put(ctx, port.PutResultRequest{
		Actor:           "U123",
		ConversationKey: "slack:T:dm:D",
		Kind:            "acp_result",
		Content:         "expired content",
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := db.ExecContext(ctx, `UPDATE recoverable_results SET expires_at = created_at + 1 WHERE ref = ?`, result1.Ref); err != nil {
		t.Fatal(err)
	}

	store.SetReferenceChecker(referenceChecker(func(ctx context.Context, ref string) (bool, error) {
		return false, nil
	}))

	count, err := store.DeleteExpired(ctx, time.Now().UTC().Add(time.Minute), 100)
	if err != nil {
		t.Fatal(err)
	}
	if count < 1 {
		t.Fatalf("deleted = %d, want at least 1", count)
	}

	// Verify result1 is gone
	_, err = store.Stat(ctx, port.StatResultRequest{
		Ref:             result1.Ref,
		Actor:           "U123",
		ConversationKey: "slack:T:dm:D",
	})
	if err == nil {
		t.Fatal("expired result should not be stat-able")
	}

	// Create a second DB+dir with longer retention
	db2, dir2 := setupTestDB(t)
	storageDir2 := filepath.Join(dir2, "results")
	store2 := recoverableresult.NewStore(db2, storageDir2, 1024*1024, 4096, 365, 100)
	store2.SetReferenceChecker(referenceChecker(func(ctx context.Context, ref string) (bool, error) {
		return false, nil
	}))
	result2, err := store2.Put(ctx, port.PutResultRequest{
		Actor:           "U123",
		ConversationKey: "slack:T:dm:D",
		Kind:            "acp_result",
		Content:         "non-expired content",
	})
	if err != nil {
		t.Fatal(err)
	}

	count2, err := store2.DeleteExpired(ctx, time.Now().UTC().Add(time.Minute), 100)
	if err != nil {
		t.Fatal(err)
	}
	if count2 != 0 {
		t.Fatalf("deleted = %d, want 0 (non-expired)", count2)
	}

	// Verify result2 still exists
	_, err = store2.Stat(ctx, port.StatResultRequest{
		Ref:             result2.Ref,
		Actor:           "U123",
		ConversationKey: "slack:T:dm:D",
	})
	if err != nil {
		t.Fatalf("non-expired stat error: %v", err)
	}
}

func TestDeleteExpiredSkipsReferenced(t *testing.T) {
	db, dir := setupTestDB(t)
	storageDir := filepath.Join(dir, "results")
	store := recoverableresult.NewStore(db, storageDir, 1024*1024, 4096, 7, 100)

	ctx := t.Context()
	result, err := store.Put(ctx, port.PutResultRequest{
		Actor:           "U123",
		ConversationKey: "slack:T:dm:D",
		Kind:            "acp_result",
		Content:         "referenced content",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Force expiry by updating DB directly (must respect CHECK: expires_at > created_at)
	if _, err := db.ExecContext(ctx, `UPDATE recoverable_results SET expires_at = created_at + 1 WHERE ref = ?`, result.Ref); err != nil {
		t.Fatal(err)
	}

	store.SetReferenceChecker(referenceChecker(func(ctx context.Context, ref string) (bool, error) {
		return ref == result.Ref, nil
	}))

	// Wait long enough for expires_at to be in the past
	time.Sleep(2 * time.Second)

	count, err := store.DeleteExpired(ctx, time.Now().UTC(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("deleted = %d, want 0 (referenced result should be skipped)", count)
	}

	// Result is expired, should not be readable regardless of reference status.
	_, err = store.Stat(ctx, port.StatResultRequest{
		Ref:             result.Ref,
		Actor:           "U123",
		ConversationKey: "slack:T:dm:D",
	})
	if err == nil {
		t.Fatalf("expired result should not be stat-able even when referenced")
	}
}

func TestDeleteExpiredReturnsZeroWithoutChecker(t *testing.T) {
	db, dir := setupTestDB(t)
	storageDir := filepath.Join(dir, "results")
	store := recoverableresult.NewStore(db, storageDir, 1024*1024, 4096, 7, 100)

	ctx := t.Context()
	result, err := store.Put(ctx, port.PutResultRequest{
		Actor:           "U123",
		ConversationKey: "slack:T:dm:D",
		Kind:            "acp_result",
		Content:         "some content",
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := db.ExecContext(ctx, `UPDATE recoverable_results SET expires_at = created_at + 1 WHERE ref = ?`, result.Ref); err != nil {
		t.Fatal(err)
	}

	// No reference checker set — DeleteExpired must return 0
	count, err := store.DeleteExpired(ctx, time.Now().UTC().Add(time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("deleted = %d, want 0 (no reference checker means skip all)", count)
	}
}

func TestDeleteExpiredReportsCheckerFailureAndReleasesClaim(t *testing.T) {
	db, dir := setupTestDB(t)
	store := recoverableresult.NewStore(db, filepath.Join(dir, "results"), 1024, 32, 7, 100)
	result, err := store.Put(t.Context(), port.PutResultRequest{
		Actor: "U123", ConversationKey: "slack:T:dm:D", Kind: "test", Content: "content",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `UPDATE recoverable_results SET expires_at = created_at + 1 WHERE ref = ?`, result.Ref); err != nil {
		t.Fatal(err)
	}
	store.SetReferenceChecker(referenceChecker(func(context.Context, string) (bool, error) {
		return false, context.DeadlineExceeded
	}))
	if _, err := store.DeleteExpired(t.Context(), time.Now().UTC().Add(time.Hour), 100); err == nil {
		t.Fatal("checker failure should be reported")
	}
	var claim string
	if err := db.QueryRowContext(t.Context(), `SELECT cleanup_claim FROM recoverable_results WHERE ref = ?`, result.Ref).Scan(&claim); err != nil {
		t.Fatal(err)
	}
	if claim != "" {
		t.Fatalf("cleanup claim was not released: %q", claim)
	}
}

func TestDeleteExpiredReclaimsStaleCleanupClaim(t *testing.T) {
	db, dir := setupTestDB(t)
	store := recoverableresult.NewStore(db, filepath.Join(dir, "results"), 1024, 32, 7, 100)
	result, err := store.Put(t.Context(), port.PutResultRequest{
		Actor: "U123", ConversationKey: "slack:T:dm:D", Kind: "test", Content: "content",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `UPDATE recoverable_results
		SET expires_at = created_at + 1, cleanup_claim = 'stale', cleanup_version = 4, cleanup_claimed_at = ?
		WHERE ref = ?`, time.Now().UTC().Add(-2*time.Hour).Unix(), result.Ref); err != nil {
		t.Fatal(err)
	}
	store.SetReferenceChecker(referenceChecker(func(context.Context, string) (bool, error) {
		return false, nil
	}))
	deleted, err := store.DeleteExpired(t.Context(), time.Now().UTC().Add(time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1", deleted)
	}
}

func TestRestartRecovery(t *testing.T) {
	db, dir := setupTestDB(t)
	storageDir := filepath.Join(dir, "results")
	store1 := recoverableresult.NewStore(db, storageDir, 1024*1024, 4096, 7, 100)

	ctx := t.Context()
	content := "persistent across restarts"
	result1, err := store1.Put(ctx, port.PutResultRequest{
		Actor:           "U123",
		ConversationKey: "slack:T:dm:D",
		Kind:            "acp_result",
		Content:         content,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Simulate restart: new store with same DB and dir
	store2 := recoverableresult.NewStore(db, storageDir, 1024*1024, 4096, 7, 100)

	chunk, err := store2.ReadChunk(ctx, domain.ResultChunkRequest{
		Ref:             result1.Ref,
		Actor:           "U123",
		ConversationKey: "slack:T:dm:D",
		OffsetBytes:     0,
		MaxBytes:        4096,
	})
	if err != nil {
		t.Fatalf("restart recovery failed: %v", err)
	}
	if chunk.Content != content {
		t.Fatalf("recovered content = %q, want %q", chunk.Content, content)
	}
}

type referenceChecker func(ctx context.Context, ref string) (bool, error)

func (c referenceChecker) IsRecoverableResultReferenced(ctx context.Context, ref string) (bool, error) {
	return c(ctx, ref)
}
