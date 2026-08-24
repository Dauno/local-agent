package resultanalysis

// segmentTextBounds computes the base (non-overlapping) chunk end offsets
// for text_v1: paragraph-first, then line, then a rune-boundary window. It
// is the fallback for every media type without a dedicated segmenter,
// including JSON, code, and logs.
func segmentTextBounds(source []byte, nominal int64) []int {
	n := len(source)
	var bounds []int
	start := 0
	for start < n {
		limit := start + int(nominal)
		if limit >= n {
			bounds = append(bounds, n)
			break
		}
		bounds = append(bounds, findTextCut(source, start, limit))
		start = bounds[len(bounds)-1]
	}
	return bounds
}

// findTextCut chooses one cut position in (start, limit]: the last
// paragraph break if one exists, else the last line break, else a
// rune-boundary window cut.
func findTextCut(source []byte, start, limit int) int {
	if idx := lastParagraphBreak(source, start, limit); idx > start {
		return idx
	}
	if idx := lastLineBreak(source, start, limit); idx > start {
		return idx
	}
	return safeRuneCut(source, start, limit)
}
