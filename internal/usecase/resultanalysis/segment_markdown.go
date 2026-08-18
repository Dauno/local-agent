package resultanalysis

import "bytes"

// segmentMarkdownBounds computes the base (non-overlapping) chunk end
// offsets for markdown_v1: heading boundaries first, then fenced-block
// boundaries, then paragraph boundaries, with a rune-boundary window as the
// final fallback. A fenced block that fits entirely inside one nominal
// segment is never split. A fenced block larger than one segment is split
// on rune boundaries across strictly contiguous, coherently overlapping
// segments (each starts exactly where the fence content resumes), so every
// part of one oversized block is recoverable as a maximal run of adjacent
// segments even though domain.AnalysisSegment carries no explicit
// enclosing-block field: checkpoint 1 keeps the segment record exactly as
// specified, and contiguity already lets a caller reconstruct the block.
//
// An unterminated fence (opened but never closed before EOF) never causes
// this function to stop making progress or to emit an oversized segment: it
// falls back to bounded rune-window cuts for the remainder of the source.
func segmentMarkdownBounds(source []byte, nominal int64) []int {
	n := len(source)
	var bounds []int
	start := 0
	inFence := false
	var fenceMarker byte
	var fenceMinLen int
	for start < n {
		limit := start + int(nominal)
		if limit >= n {
			bounds = append(bounds, n)
			break
		}
		if inFence {
			cut, closed := fenceCut(source, start, limit, fenceMarker, fenceMinLen, nominal)
			bounds = append(bounds, cut)
			start = cut
			if closed {
				inFence = false
			}
			continue
		}
		if openStart, openLineEnd, marker, length, found := nextFenceOpen(source, start); found {
			if openStart > start && openStart < limit {
				bounds = append(bounds, openStart)
				start = openStart
				continue
			}
			if openStart <= start {
				cut, closed := fenceCutFromOpen(source, start, limit, openLineEnd, marker, length, nominal)
				bounds = append(bounds, cut)
				start = cut
				inFence = !closed
				if !closed {
					fenceMarker, fenceMinLen = marker, length
				}
				continue
			}
		}
		if idx := lastHeadingLineStart(source, start, limit); idx > start {
			bounds = append(bounds, idx)
			start = idx
			continue
		}
		if idx := lastParagraphBreak(source, start, limit); idx > start {
			bounds = append(bounds, idx)
			start = idx
			continue
		}
		if idx := lastLineBreak(source, start, limit); idx > start {
			bounds = append(bounds, idx)
			start = idx
			continue
		}
		cut := safeRuneCut(source, start, limit)
		bounds = append(bounds, cut)
		start = cut
	}
	return bounds
}

// fenceCut is called while already inside an open fence. It looks for the
// matching close from start onward: if found and the whole remainder up to
// the close still fits within one nominal segment measured from start, the
// segment ends exactly at the close and the fence stops being open.
// Otherwise it takes a bounded rune-window cut and the fence stays open.
func fenceCut(source []byte, start, limit int, marker byte, minLen int, nominal int64) (cut int, closed bool) {
	if closeEnd, found := findFenceClose(source, start, marker, minLen); found && int64(closeEnd-start) <= nominal {
		return closeEnd, true
	}
	return safeRuneCut(source, start, limit), false
}

// fenceCutFromOpen is called when a fence begins at or before start. It
// behaves like fenceCut but measures the fence from its opening line.
func fenceCutFromOpen(source []byte, start, limit, searchFrom int, marker byte, minLen int, nominal int64) (cut int, closed bool) {
	if closeEnd, found := findFenceClose(source, searchFrom, marker, minLen); found && int64(closeEnd-start) <= nominal {
		return closeEnd, true
	}
	return safeRuneCut(source, start, limit), false
}

// lastHeadingLineStart returns the offset of the last ATX heading line
// start ("#" through "######" followed by a space or tab) within
// source[start:limit], or start when none exists.
func lastHeadingLineStart(source []byte, start, limit int) int {
	best := start
	for i := start; i < limit && i < len(source); {
		nl := bytes.IndexByte(source[i:limit], '\n')
		if nl < 0 {
			break
		}
		lineStart := i + nl + 1
		if lineStart > limit {
			break
		}
		if lineStart > start && isHeadingLine(source, lineStart) {
			best = lineStart
		}
		i = lineStart
	}
	return best
}

func isHeadingLine(source []byte, pos int) bool {
	n := 0
	for pos+n < len(source) && n < 6 && source[pos+n] == '#' {
		n++
	}
	if n == 0 || pos+n >= len(source) {
		return false
	}
	return source[pos+n] == ' ' || source[pos+n] == '\t'
}

// nextFenceOpen finds the first fenced-code-block opening line at or after
// from: a line whose entire trimmed content is a run of three or more
// identical '`' or '~' characters (optionally followed by an info string,
// which is ignored). It returns the line's start offset, the offset right
// after its trailing newline (or EOF), the fence character, and its run
// length.
func nextFenceOpen(source []byte, from int) (lineStart, lineEnd int, marker byte, length int, found bool) {
	pos := from
	for pos < len(source) {
		nl := bytes.IndexByte(source[pos:], '\n')
		var line []byte
		var next int
		if nl < 0 {
			line = source[pos:]
			next = len(source)
		} else {
			line = source[pos : pos+nl]
			next = pos + nl + 1
		}
		if m, l, ok := isFenceLine(line); ok {
			return pos, next, m, l, true
		}
		pos = next
	}
	return 0, 0, 0, 0, false
}

// findFenceClose finds the first closing fence line at or after from that
// uses the same marker character and a run length at least minLen, per the
// CommonMark closing-fence rule. It returns the offset right after that
// line's trailing newline (or EOF).
func findFenceClose(source []byte, from int, marker byte, minLen int) (lineEnd int, found bool) {
	pos := from
	for pos < len(source) {
		nl := bytes.IndexByte(source[pos:], '\n')
		var line []byte
		var next int
		if nl < 0 {
			line = source[pos:]
			next = len(source)
		} else {
			line = source[pos : pos+nl]
			next = pos + nl + 1
		}
		if m, l, ok := isFenceLine(line); ok && m == marker && l >= minLen {
			return next, true
		}
		pos = next
	}
	return 0, false
}

// isFenceLine reports whether line (without its trailing newline) is a
// fenced-code-block delimiter: up to three leading spaces of indentation
// (the CommonMark allowance, most commonly seen for a fence nested inside a
// list item), followed by a run of three or more identical '`' or '~'
// characters, optionally followed by more content (an info string on an
// opening fence, ignored here). A trailing '\r' is tolerated. This is a
// heuristic segmenter, not a full CommonMark parser: a tab, or more than
// three leading spaces, is not treated as fence indentation.
//
// FIND-098: an earlier version required the marker at column 0, so a fence
// indented for a list item (a normal, valid CommonMark document) was never
// recognized, and the segmenter could split inside a block that would
// otherwise have fit one segment.
func isFenceLine(line []byte) (marker byte, length int, ok bool) {
	line = bytes.TrimSuffix(line, []byte("\r"))
	indent := 0
	for indent < len(line) && indent < 3 && line[indent] == ' ' {
		indent++
	}
	rest := line[indent:]
	if len(rest) < 3 {
		return 0, 0, false
	}
	c := rest[0]
	if c != '`' && c != '~' {
		return 0, 0, false
	}
	n := 0
	for n < len(rest) && rest[n] == c {
		n++
	}
	if n < 3 {
		return 0, 0, false
	}
	return c, n, true
}
