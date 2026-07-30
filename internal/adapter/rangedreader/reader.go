// Package rangedreader provides a filesystem-backed code reader that extracts
// line ranges from text files within a pre-resolved project root directory.
package rangedreader

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

var _ port.CodeReader = (*Reader)(nil)

// Reader reads line ranges from text files under a single project root.
// Path validation and symlink resolution are the caller's responsibility;
// the reader enforces that path is relative and rejects symlinks via Lstat.
type Reader struct {
	rootDir        string
	maxOutputBytes int
	chunkMaxBytes  int
	maxFileBytes   int64
	resultStore    port.RecoverableResultStore
	preserveCRLF   bool
}

// WithResultStore enables immutable owner-bound snapshots for every read.
func (r *Reader) WithResultStore(store port.RecoverableResultStore) *Reader {
	r.resultStore = store
	return r
}

// WithPreservedLineEndings keeps snapshot bytes unchanged for syntax engines
// whose byte offsets must remain digest-bound to the original file.
func (r *Reader) WithPreservedLineEndings() *Reader {
	r.preserveCRLF = true
	return r
}

// NewReader creates a filesystem-backed code reader.
// rootDir is the resolved absolute path of the project root.
// maxOutputBytes is the byte limit for returned content.
// chunkMaxBytes is ignored in Wave 2 (reserved for future chunked delivery).
// maxFileBytes caps the total file size read for snapshot computation.
func NewReader(rootDir string, maxOutputBytes int, chunkMaxBytes int, maxFileBytes int64) *Reader {
	return &Reader{
		rootDir:        rootDir,
		maxOutputBytes: maxOutputBytes,
		chunkMaxBytes:  chunkMaxBytes,
		maxFileBytes:   maxFileBytes,
	}
}

// ReadRange reads a line range from a text file. See port.CodeReader.
func (r *Reader) ReadRange(ctx context.Context, req domain.SourceRangeRequest) (domain.SourceRange, error) {
	if err := ctx.Err(); err != nil {
		return domain.SourceRange{}, err
	}

	// 1. Validate path is relative (no / prefix, no .. segments).
	cleanPath := filepath.Clean(req.Path)
	if filepath.IsAbs(cleanPath) {
		return domain.SourceRange{}, fmt.Errorf("path must be relative: %q", req.Path)
	}
	if strings.HasPrefix(cleanPath, ".."+string(os.PathSeparator)) || cleanPath == ".." {
		return domain.SourceRange{}, fmt.Errorf("path must be relative: %q", req.Path)
	}
	for _, segment := range strings.Split(cleanPath, string(os.PathSeparator)) {
		switch segment {
		case ".env", ".local-agent", ".git":
			return domain.SourceRange{}, fmt.Errorf("path %q is unavailable", req.Path)
		}
	}

	// 2. Open relative to the trusted root without following symlinks.
	file, err := openSecure(r.rootDir, cleanPath)
	if err != nil {
		return domain.SourceRange{}, fmt.Errorf("path %q is unavailable", req.Path)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return domain.SourceRange{}, fmt.Errorf("path %q is not a supported text file", req.Path)
	}

	// 4. Read one extra byte so an oversized file cannot be mistaken for a
	// complete immutable snapshot.
	limitReader := io.LimitReader(file, r.maxFileBytes+1)
	data, err := io.ReadAll(limitReader)
	if err != nil {
		return domain.SourceRange{}, fmt.Errorf("path %q is unavailable", req.Path)
	}
	if int64(len(data)) > r.maxFileBytes {
		return domain.SourceRange{}, fmt.Errorf("path %q exceeds the immutable snapshot limit", req.Path)
	}

	// 6. Validate UTF-8 and check for NUL bytes.
	if !utf8.Valid(data) || bytes.Contains(data, []byte{0}) {
		return domain.SourceRange{}, fmt.Errorf("path %q is not a supported text file", req.Path)
	}

	// 7. Compute SHA-256 of complete content.
	hash := sha256.Sum256(data)
	fileSHA256 := hex.EncodeToString(hash[:])

	// 8. If ExpectedSHA256 is non-empty and doesn't match, return error.
	if req.ExpectedSHA256 != "" && req.ExpectedSHA256 != fileSHA256 {
		return domain.SourceRange{}, fmt.Errorf("source changed: file %q has different SHA-256", req.Path)
	}

	// 9. Build line-start byte offsets in one pass.
	lineStarts := buildLineStarts(data)

	// 10. StartLine is 1-based; clamp to valid range.
	startLine := req.StartLine
	if startLine < 1 {
		startLine = 1
	}

	if startLine > len(lineStarts) {
		// Beyond end of file.
		return domain.SourceRange{
			Location:      CodeLocationForFile(req.Project, req.Path, fileSHA256, data, lineStarts),
			Content:       "",
			NextStartLine: 0,
			EOF:           true,
		}, nil
	}

	// 11. Extract line range [startLine, startLine+maxLines).
	if req.MaxLines <= 0 {
		return domain.SourceRange{}, errors.New("max_lines must be positive")
	}
	const maxLinesCap = 10000
	if req.MaxLines > maxLinesCap {
		req.MaxLines = maxLinesCap
	}
	// Guard against integer overflow: if startLine is near MaxInt, adding
	// MaxLines would wrap negative and index lineStarts out of bounds.
	if startLine > len(lineStarts) || startLine > (1<<31)-maxLinesCap {
		return domain.SourceRange{
			Location:      CodeLocationForFile(req.Project, req.Path, fileSHA256, data, lineStarts),
			Content:       "",
			NextStartLine: 0,
			EOF:           true,
		}, nil
	}
	requestedEndLine := startLine + req.MaxLines
	endLine := requestedEndLine
	if endLine > len(lineStarts)+1 {
		endLine = len(lineStarts) + 1
	}
	overRequested := requestedEndLine > len(lineStarts)+1

	startIdx := startLine - 1 // zero-based index into lineStarts
	endIdx := endLine - 1

	var startByte int64
	var endByte int64
	if startIdx < len(lineStarts) {
		startByte = lineStarts[startIdx]
	}
	if endIdx < len(lineStarts) {
		endByte = lineStarts[endIdx] // exclusive end within known lines
	} else {
		endByte = int64(len(data)) // read to end of file
	}

	var rawContent []byte
	if endIdx >= len(lineStarts) {
		rawContent = data[startByte:]
	} else {
		rawContent = data[startByte:endByte]
	}

	// 12. Clamp result to maxOutputBytes, truncate at valid UTF-8 boundary.
	content, truncated, valid := textPrefix(rawContent, r.maxOutputBytes)
	if !valid {
		return domain.SourceRange{}, fmt.Errorf("path %q is not a supported text file", req.Path)
	}
	resultEndByte := startByte + int64(len(content))

	if !r.preserveCRLF {
		content = strings.ReplaceAll(content, "\r\n", "\n")
	}

	eof := overRequested || endIdx >= len(lineStarts)
	nextStartLine := endLine

	if truncated {
		// Truncation means we cut content before consuming all requested lines.
		// Count how many complete lines we actually returned.
		actualLines := strings.Count(content, "\n")
		if content != "" && !strings.HasSuffix(content, "\n") {
			// A line number cannot identify the unread suffix of a partial line.
			// Callers continue from NextOffsetBytes through the immutable result.
			nextStartLine = 0
		} else {
			nextStartLine = startLine + actualLines
		}
		if nextStartLine != 0 && nextStartLine < startLine {
			nextStartLine = startLine
		}
		if nextStartLine > len(lineStarts) {
			nextStartLine = 0
		}
		eof = false // Content was truncated — more data exists beyond what we returned.
	}

	if eof {
		nextStartLine = 0
	}
	returnedEndLine := startLine + strings.Count(content, "\n")
	if strings.HasSuffix(content, "\n") {
		returnedEndLine--
	}
	if content == "" {
		returnedEndLine = startLine - 1
	}
	location := domain.CodeLocation{Project: req.Project, Path: cleanPath, StartByte: startByte, EndByte: resultEndByte,
		StartLine: startLine, EndLine: returnedEndLine, FileSHA256: fileSHA256}

	resultRef := ""
	if r.resultStore != nil {
		stored, storeErr := r.resultStore.Put(ctx, port.PutResultRequest{Actor: req.Actor, ConversationKey: req.ConversationKey, Kind: "source_snapshot", Content: string(data)})
		if storeErr != nil {
			return domain.SourceRange{}, errors.New("persist immutable source snapshot")
		}
		resultRef = stored.Ref
	}
	nextOffsetBytes := int64(0)
	if truncated {
		nextOffsetBytes = resultEndByte
	}
	return domain.SourceRange{
		Location:        location,
		Content:         content,
		NextStartLine:   nextStartLine,
		NextOffsetBytes: nextOffsetBytes,
		EOF:             eof,
		Truncated:       truncated,
		ResultRef:       resultRef,
	}, nil
}

// buildLineStarts returns the byte offset of the start of each line.
// Lines are terminated by \n (LF). \r\n (CRLF) is treated as a single line
// ending. A final line without a trailing newline is still counted as a line.
// Line numbers are 1-based, so lineStarts[0] is the start of line 1.
func buildLineStarts(data []byte) []int64 {
	if len(data) == 0 {
		return nil
	}
	starts := []int64{0}
	for i := 0; i < len(data); i++ {
		if data[i] == '\n' {
			starts = append(starts, int64(i+1))
		}
	}
	// Remove the final entry if it points past the last byte (trailing \n).
	if len(starts) > 1 && starts[len(starts)-1] == int64(len(data)) {
		starts = starts[:len(starts)-1]
	}
	return starts
}

// CodeLocationForFile constructs a CodeLocation from file metadata.
// startLine and endLine are 1-based, inclusive.
func CodeLocationForFile(project, path, fileSHA256 string, data []byte, lineStarts []int64) domain.CodeLocation {
	loc := domain.CodeLocation{
		Project:    project,
		Path:       path,
		FileSHA256: fileSHA256,
		StartByte:  0,
		EndByte:    int64(len(data)),
		StartLine:  1,
		EndLine:    len(lineStarts),
	}
	if len(lineStarts) == 0 {
		loc.EndLine = 0
	}
	return loc
}

// textPrefix returns a prefix of data truncated to maxBytes at a valid UTF-8 boundary.
// Returns (truncated data, whether truncated, whether valid UTF-8).
func textPrefix(data []byte, maxBytes int) (string, bool, bool) {
	if len(data) == 0 {
		return "", false, true
	}
	if maxBytes <= 0 {
		return "", len(data) > 0, true
	}
	limit := len(data)
	if limit > maxBytes {
		limit = maxBytes
	}
	// Walk runes up to maxBytes, stopping at the last valid rune boundary.
	lastValid := 0
	for offset := 0; offset < limit; {
		r, size := utf8.DecodeRune(data[offset:])
		if r == utf8.RuneError && size == 1 {
			if bytes.Equal(data[offset:offset+1], []byte{0}) {
				return "", false, false // NUL byte
			}
			return "", false, false // invalid UTF-8
		}
		lastValid = offset + size
		offset += size
	}
	truncated := len(data) > lastValid
	content := string(data[:lastValid])
	if lastValid > maxBytes {
		// We wrote more than maxBytes because a multibyte rune straddled the boundary.
		// Walk back to the last valid rune boundary before maxBytes.
		lastBeforeBoundary := 0
		for offset := 0; offset < maxBytes; {
			r, size := utf8.DecodeRune(data[offset:])
			if r == utf8.RuneError && size == 1 {
				return "", false, false
			}
			if offset+size > maxBytes {
				break
			}
			lastBeforeBoundary = offset + size
			offset += size
		}
		truncated = true
		content = string(data[:lastBeforeBoundary])
	}
	return content, truncated, true
}
