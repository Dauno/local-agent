package rangedreader

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

func TestReadRange_ExactLineRange(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "test.go", "line1\nline2\nline3\nline4\nline5\n")

	reader := NewReader(root, 4096, 4096, 1<<20)
	result, err := reader.ReadRange(context.Background(), domain.SourceRangeRequest{
		Project:   "test",
		Path:      "test.go",
		StartLine: 2,
		MaxLines:  2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "line2\nline3\n" {
		t.Fatalf("content = %q, want %q", result.Content, "line2\\nline3\\n")
	}
	if result.NextStartLine != 4 {
		t.Fatalf("NextStartLine = %d, want 4", result.NextStartLine)
	}
	if result.EOF {
		t.Fatal("expected EOF=false")
	}
	if result.Location.StartLine != 2 || result.Location.EndLine != 3 || result.Location.StartByte != 6 || result.Location.EndByte != 18 {
		t.Fatalf("location = %#v", result.Location)
	}
}

func TestReadRange_EOF(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "test.go", "line1\nline2\nline3\n")

	reader := NewReader(root, 4096, 4096, 1<<20)
	result, err := reader.ReadRange(context.Background(), domain.SourceRangeRequest{
		Project:   "test",
		Path:      "test.go",
		StartLine: 2,
		MaxLines:  5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.EOF {
		t.Fatal("expected EOF=true")
	}
	if result.NextStartLine != 0 {
		t.Fatalf("NextStartLine = %d, want 0 at EOF", result.NextStartLine)
	}
	if result.Content != "line2\nline3\n" {
		t.Fatalf("content = %q, want %q", result.Content, "line2\\nline3\\n")
	}
}

func TestReadRange_EmptyFile(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "empty.txt", "")

	reader := NewReader(root, 4096, 4096, 1<<20)
	result, err := reader.ReadRange(context.Background(), domain.SourceRangeRequest{
		Project:   "test",
		Path:      "empty.txt",
		StartLine: 1,
		MaxLines:  10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "" {
		t.Fatalf("content = %q, want empty", result.Content)
	}
	if result.NextStartLine != 0 {
		t.Fatalf("NextStartLine = %d, want 0", result.NextStartLine)
	}
	if !result.EOF {
		t.Fatal("expected EOF=true for empty file")
	}
}

func TestReadRange_StartLineBeyondEnd(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "test.go", "line1\nline2\nline3\n")

	reader := NewReader(root, 4096, 4096, 1<<20)
	result, err := reader.ReadRange(context.Background(), domain.SourceRangeRequest{
		Project:   "test",
		Path:      "test.go",
		StartLine: 10,
		MaxLines:  2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "" {
		t.Fatalf("content = %q, want empty", result.Content)
	}
	if result.NextStartLine != 0 {
		t.Fatalf("NextStartLine = %d, want 0", result.NextStartLine)
	}
	if !result.EOF {
		t.Fatal("expected EOF=true when start beyond end")
	}
}

func TestReadRange_NoTrailingNewline(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "test.txt", "line1\nline2\nline3")

	reader := NewReader(root, 4096, 4096, 1<<20)
	result, err := reader.ReadRange(context.Background(), domain.SourceRangeRequest{
		Project:   "test",
		Path:      "test.txt",
		StartLine: 3,
		MaxLines:  1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "line3" {
		t.Fatalf("content = %q, want %q", result.Content, "line3")
	}
	if result.NextStartLine != 0 {
		t.Fatalf("NextStartLine = %d, want 0 at EOF", result.NextStartLine)
	}
	if !result.EOF {
		t.Fatal("expected EOF=true after reading last line")
	}
}

func TestReadRange_CRLF(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "windows.txt", "line1\r\nline2\r\nline3\r\n")

	reader := NewReader(root, 4096, 4096, 1<<20)
	result, err := reader.ReadRange(context.Background(), domain.SourceRangeRequest{
		Project:   "test",
		Path:      "windows.txt",
		StartLine: 2,
		MaxLines:  2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "line2\nline3\n" {
		t.Fatalf("content = %q, want %q", result.Content, "line2\\nline3\\n")
	}
	// EOF: file has 3 lines; reading 2-3 reaches end.
	if !result.EOF {
		t.Fatal("expected EOF=true when all remaining lines consumed")
	}
	if result.NextStartLine != 0 {
		t.Fatalf("NextStartLine = %d, want 0 at EOF", result.NextStartLine)
	}
}

func TestReadRange_LongSingleLineByteCap(t *testing.T) {
	root := t.TempDir()
	// File: "abcde🔥fghij" (5 ASCII + 1 emoji [4 bytes] + 5 ASCII = 14 bytes)
	data := "abcde🔥fghij"
	writeFile(t, root, "long.txt", data)

	// maxOutputBytes=8 — boundary falls in the middle of 🔥 (byte 5-8)
	reader := NewReader(root, 8, 8, 1<<20)
	result, err := reader.ReadRange(context.Background(), domain.SourceRangeRequest{
		Project:   "test",
		Path:      "long.txt",
		StartLine: 1,
		MaxLines:  1,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Should truncate before the 🔥 emoji (at byte 5, "abcde")
	if result.Content != "abcde" {
		t.Fatalf("content = %q, want %q (truncated before multibyte rune)", result.Content, "abcde")
	}
	if !result.Truncated {
		t.Fatal("expected Truncated=true")
	}
	if result.EOF {
		t.Fatal("expected EOF=false (content exists beyond truncation)")
	}
	if result.NextStartLine != 0 {
		t.Fatalf("NextStartLine = %d, want 0 for a partial line", result.NextStartLine)
	}
	if result.NextOffsetBytes != int64(len(result.Content)) {
		t.Fatalf("NextOffsetBytes = %d, want %d", result.NextOffsetBytes, len(result.Content))
	}
}

func TestReadRange_LongSingleLineNoByteCap(t *testing.T) {
	root := t.TempDir()
	data := "abcde🔥fghij"
	writeFile(t, root, "long.txt", data)

	// maxOutputBytes large enough to fit entire file
	reader := NewReader(root, 4096, 4096, 1<<20)
	result, err := reader.ReadRange(context.Background(), domain.SourceRangeRequest{
		Project:   "test",
		Path:      "long.txt",
		StartLine: 1,
		MaxLines:  1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != data {
		t.Fatalf("content = %q, want %q", result.Content, data)
	}
	if result.Truncated {
		t.Fatal("expected Truncated=false")
	}
	if !result.EOF {
		t.Fatal("expected EOF=true for single-line file")
	}
}

func TestReadRange_SHA256Mismatch(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "test.go", "line1\nline2\n")

	reader := NewReader(root, 4096, 4096, 1<<20)
	_, err := reader.ReadRange(context.Background(), domain.SourceRangeRequest{
		Project:        "test",
		Path:           "test.go",
		StartLine:      1,
		MaxLines:       2,
		ExpectedSHA256: "0000000000000000000000000000000000000000000000000000000000000000",
	})
	if err == nil {
		t.Fatal("expected error for SHA-256 mismatch")
	}
	if !strings.Contains(err.Error(), "source changed") {
		t.Fatalf("error = %v, want 'source changed'", err)
	}
}

func TestReadRange_SHA256Match(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "test.go", "line1\nline2\n")

	reader := NewReader(root, 4096, 4096, 1<<20)
	// First read without SHA to get the correct hash from Location
	result1, err := reader.ReadRange(context.Background(), domain.SourceRangeRequest{
		Project:   "test",
		Path:      "test.go",
		StartLine: 1,
		MaxLines:  1,
	})
	if err != nil {
		t.Fatal(err)
	}
	sha := result1.Location.FileSHA256
	if sha == "" {
		t.Fatal("expected non-empty SHA-256")
	}

	// Second read with correct SHA
	result2, err := reader.ReadRange(context.Background(), domain.SourceRangeRequest{
		Project:        "test",
		Path:           "test.go",
		StartLine:      2,
		MaxLines:       1,
		ExpectedSHA256: sha,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result2.Content != "line2\n" {
		t.Fatalf("content = %q, want %q", result2.Content, "line2\\n")
	}
}

func TestReadRange_PathEscape(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "safe.txt", "content\n")

	reader := NewReader(root, 4096, 4096, 1<<20)
	_, err := reader.ReadRange(context.Background(), domain.SourceRangeRequest{
		Project:   "test",
		Path:      "../safe.txt",
		StartLine: 1,
		MaxLines:  1,
	})
	if err == nil {
		t.Fatal("expected error for path escape")
	}
}

func TestReadRange_Symlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "real.txt")
	if err := os.WriteFile(target, []byte("content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	reader := NewReader(root, 4096, 4096, 1<<20)
	_, err := reader.ReadRange(context.Background(), domain.SourceRangeRequest{
		Project:   "test",
		Path:      "link.txt",
		StartLine: 1,
		MaxLines:  1,
	})
	if err == nil {
		t.Fatal("expected error for symlink")
	}
}

func TestReadRange_MaxLinesClamping(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "test.go", "line1\nline2\nline3\n")

	reader := NewReader(root, 4096, 4096, 1<<20)
	result, err := reader.ReadRange(context.Background(), domain.SourceRangeRequest{
		Project:   "test",
		Path:      "test.go",
		StartLine: 2,
		MaxLines:  10, // request more than available
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "line2\nline3\n" {
		t.Fatalf("content = %q, want %q", result.Content, "line2\\nline3\\n")
	}
	if !result.EOF {
		t.Fatal("expected EOF=true when all remaining lines read")
	}
}

func TestReadRange_OneLineFile(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "single.txt", "only line\n")

	reader := NewReader(root, 4096, 4096, 1<<20)
	result, err := reader.ReadRange(context.Background(), domain.SourceRangeRequest{
		Project:   "test",
		Path:      "single.txt",
		StartLine: 1,
		MaxLines:  1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "only line\n" {
		t.Fatalf("content = %q, want %q", result.Content, "only line\\n")
	}
	if result.NextStartLine != 0 {
		t.Fatalf("NextStartLine = %d, want 0 at EOF", result.NextStartLine)
	}
	if !result.EOF {
		t.Fatal("expected EOF=true for one-line file")
	}
}

func TestReadRange_MultibyteUTF8(t *testing.T) {
	root := t.TempDir()
	// 🔥 is 4 bytes (F0 9F 94 A5)
	writeFile(t, root, "emoji.txt", "🔥test\nline2\nline3\n")

	reader := NewReader(root, 4096, 4096, 1<<20)
	result, err := reader.ReadRange(context.Background(), domain.SourceRangeRequest{
		Project:   "test",
		Path:      "emoji.txt",
		StartLine: 1,
		MaxLines:  2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "🔥test\nline2\n" {
		t.Fatalf("content = %q, want %q", result.Content, "🔥test\\nline2\\n")
	}
	if result.NextStartLine != 3 {
		t.Fatalf("NextStartLine = %d, want 3", result.NextStartLine)
	}
}

func TestReadRange_AbsolutePath(t *testing.T) {
	root := t.TempDir()

	reader := NewReader(root, 4096, 4096, 1<<20)
	_, err := reader.ReadRange(context.Background(), domain.SourceRangeRequest{
		Project:   "test",
		Path:      "/etc/passwd",
		StartLine: 1,
		MaxLines:  1,
	})
	if err == nil {
		t.Fatal("expected error for absolute path")
	}
}

func TestReadRangeRejectsRestrictedSegments(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, ".git"), "config", "secret")
	reader := NewReader(root, 4096, 4096, 1<<20)
	_, err := reader.ReadRange(context.Background(), domain.SourceRangeRequest{Project: "test", Path: ".git/config", StartLine: 1, MaxLines: 1})
	if err == nil {
		t.Fatal("expected restricted path to be unavailable")
	}
}

func TestReadRange_MaxFileBytesCap(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "large.txt", "line1\nline2\nline3\nline4\nline5\n")

	// maxFileBytes=10 — only first 10 bytes of the file are read
	reader := NewReader(root, 4096, 4096, 10)
	_, err := reader.ReadRange(context.Background(), domain.SourceRangeRequest{
		Project:   "test",
		Path:      "large.txt",
		StartLine: 1,
		MaxLines:  2,
	})
	if err == nil || !strings.Contains(err.Error(), "snapshot limit") {
		t.Fatalf("ReadRange() error = %v, want snapshot limit error", err)
	}
}

type snapshotStore struct{ put port.PutResultRequest }

func (s *snapshotStore) Put(_ context.Context, req port.PutResultRequest) (domain.RecoverableResult, error) {
	s.put = req
	return domain.RecoverableResult{Ref: "snapshot-ref"}, nil
}
func (*snapshotStore) ReadChunk(context.Context, domain.ResultChunkRequest) (domain.ResultChunk, error) {
	return domain.ResultChunk{}, nil
}
func (*snapshotStore) Stat(context.Context, port.StatResultRequest) (domain.RecoverableResult, error) {
	return domain.RecoverableResult{}, nil
}
func (*snapshotStore) DeleteExpired(context.Context, time.Time, int) (int, error) { return 0, nil }

func TestReadRangePersistsOwnerBoundCompleteSnapshot(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "source.go", "one\ntwo\nthree\n")
	store := &snapshotStore{}
	reader := NewReader(root, 4, 4, 1024).WithResultStore(store)
	result, err := reader.ReadRange(context.Background(), domain.SourceRangeRequest{Project: "workspace", Path: "source.go", StartLine: 1, MaxLines: 2, Actor: "U123", ConversationKey: "conversation"})
	if err != nil {
		t.Fatal(err)
	}
	if result.ResultRef != "snapshot-ref" {
		t.Fatalf("ResultRef = %q", result.ResultRef)
	}
	if store.put.Actor != "U123" || store.put.ConversationKey != "conversation" || store.put.Content != "one\ntwo\nthree\n" {
		t.Fatalf("snapshot Put() = %#v", store.put)
	}
}

// helpers

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(fmt.Errorf("write test file %s: %w", name, err))
	}
}
