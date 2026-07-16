package slack

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	SlackMarkdownChunkRunes = 11900
	markdownRenderMode      = "markdown_v1"
)

var (
	slackControlPattern = regexp.MustCompile(
		`<@U[A-Z0-9]+(\|[^>]*)?>|` +
			`<!subteam\^[A-Z0-9]+(\|[^>]*)?>|` +
			`<!here>|` +
			`<!channel>|` +
			`<!everyone>|` +
			`<#C[A-Z0-9]+(\|[^>]*)?>|` +
			`<!date\^[^>]+(\|[^>]*)?>`,
	)
	headingRe    = regexp.MustCompile(`^#{1,6}\s`)
	listItemRe   = regexp.MustCompile(`^(\s*(?:[-*+]|\d+\.)\s+)`)
	blockQuoteRe = regexp.MustCompile(`^(\s*>\s?)`)
)

type fenceSpec struct {
	marker byte
	length int
}

func renderMarkdownV1(text string, showLabels bool) []string {
	return SplitMarkdown(neutralizeUnsafeControls(text), SlackMarkdownChunkRunes, showLabels)
}

func neutralizeUnsafeControls(text string) string {
	var result strings.Builder
	result.Grow(len(text))

	var activeFence *fenceSpec
	inlineTicks := 0
	for _, rawLine := range splitLinesKeepEnd(text) {
		line, ending := splitLineEnding(rawLine)
		if activeFence != nil {
			result.WriteString(rawLine)
			if isFenceClose(line, *activeFence) {
				activeFence = nil
			}
			continue
		}

		if inlineTicks == 0 {
			if spec, ok := parseFenceOpen(line); ok {
				result.WriteString(rawLine)
				activeFence = &spec
				continue
			}
		}

		result.WriteString(neutralizeLine(line, &inlineTicks))
		result.WriteString(ending)
	}
	return result.String()
}

func neutralizeLine(line string, inlineTicks *int) string {
	var result strings.Builder
	result.Grow(len(line))
	for i := 0; i < len(line); {
		if line[i] == '`' {
			run := countByteRun(line, i, '`')
			result.WriteString(line[i : i+run])
			if *inlineTicks == 0 {
				if hasClosingBacktickRun(line, i+run, run) {
					*inlineTicks = run
				}
			} else if run == *inlineTicks {
				*inlineTicks = 0
			}
			i += run
			continue
		}
		if *inlineTicks == 0 {
			if match := slackControlPattern.FindStringIndex(line[i:]); match != nil && match[0] == 0 {
				end := i + match[1]
				result.WriteString("&lt;")
				result.WriteString(line[i+1 : end])
				i = end
				continue
			}
		}
		result.WriteByte(line[i])
		i++
	}
	return result.String()
}

func hasClosingBacktickRun(line string, start, length int) bool {
	for index := start; index < len(line); {
		if line[index] != '`' {
			index++
			continue
		}
		run := countByteRun(line, index, '`')
		if run == length {
			return true
		}
		index += run
	}
	return false
}

func parseFenceOpen(line string) (fenceSpec, bool) {
	start := leadingFenceIndent(line)
	if start < 0 || start >= len(line) || (line[start] != '`' && line[start] != '~') {
		return fenceSpec{}, false
	}
	marker := line[start]
	length := countByteRun(line, start, marker)
	if length < 3 {
		return fenceSpec{}, false
	}
	if marker == '`' && strings.ContainsRune(line[start+length:], '`') {
		return fenceSpec{}, false
	}
	return fenceSpec{marker: marker, length: length}, true
}

func isFenceClose(line string, fence fenceSpec) bool {
	start := leadingFenceIndent(line)
	if start < 0 || start >= len(line) || line[start] != fence.marker {
		return false
	}
	length := countByteRun(line, start, fence.marker)
	return length >= fence.length && strings.TrimSpace(line[start+length:]) == ""
}

func leadingFenceIndent(line string) int {
	index := 0
	for index < len(line) && line[index] == ' ' && index < 4 {
		index++
	}
	if index > 3 {
		return -1
	}
	return index
}

func countByteRun(text string, start int, value byte) int {
	count := 0
	for start+count < len(text) && text[start+count] == value {
		count++
	}
	return count
}

// SplitMarkdown creates deterministic, bounded Slack delivery parts. Multipart
// labels and any repeated Markdown structure are delivery-only artifacts.
func SplitMarkdown(text string, limit int, showLabels bool) []string {
	if text == "" || limit <= 0 {
		return nil
	}
	parts := splitMarkdownContent(text, limit)
	if len(parts) <= 1 || !showLabels {
		return parts
	}

	for {
		labelRunes := utf8.RuneCountInString(fmt.Sprintf("Part %d of %d\n\n", len(parts), len(parts)))
		contentLimit := limit - labelRunes
		if contentLimit <= 0 {
			return splitHardRunes(text, limit)
		}
		adjusted := splitMarkdownContent(text, contentLimit)
		if len(adjusted) == len(parts) {
			parts = adjusted
			break
		}
		parts = adjusted
	}

	total := len(parts)
	for index := range parts {
		parts[index] = fmt.Sprintf("Part %d of %d\n\n%s", index+1, total, parts[index])
	}
	return parts
}

func splitMarkdownContent(text string, limit int) []string {
	lines := splitLinesKeepEnd(text)
	var chunks []string
	var current strings.Builder
	flush := func() {
		if current.Len() != 0 {
			chunks = append(chunks, current.String())
			current.Reset()
		}
	}
	appendSegment := func(segment string) {
		if segment == "" {
			return
		}
		if runeCount(current.String())+runeCount(segment) > limit {
			flush()
		}
		if runeCount(segment) > limit {
			for _, part := range splitPlainMarkdown(segment, limit) {
				if runeCount(part) > limit {
					chunks = append(chunks, splitHardRunes(part, limit)...)
					continue
				}
				chunks = append(chunks, part)
			}
			return
		}
		current.WriteString(segment)
	}

	for index := 0; index < len(lines); {
		line, _ := splitLineEnding(lines[index])
		if fence, ok := parseFenceOpen(line); ok {
			end := index + 1
			for end < len(lines) {
				candidate, _ := splitLineEnding(lines[end])
				end++
				if isFenceClose(candidate, fence) {
					break
				}
			}
			block := lines[index:end]
			fenceParts := splitFenceBlock(block, fence, limit)
			for _, part := range fenceParts {
				appendSegment(part)
				if len(fenceParts) > 1 {
					flush()
				}
			}
			index = end
			continue
		}

		if index+1 < len(lines) && isTableHeader(line) {
			separator, _ := splitLineEnding(lines[index+1])
			if isTableSeparator(separator) {
				end := index + 2
				for end < len(lines) {
					row, _ := splitLineEnding(lines[end])
					if !isTableRow(row) {
						break
					}
					end++
				}
				for _, part := range splitTableBlock(lines[index:end], limit) {
					appendSegment(part)
					flush()
				}
				index = end
				continue
			}
		}

		end := index + 1
		for end < len(lines) {
			candidate, _ := splitLineEnding(lines[end])
			if _, ok := parseFenceOpen(candidate); ok {
				break
			}
			if end+1 < len(lines) && isTableHeader(candidate) {
				separator, _ := splitLineEnding(lines[end+1])
				if isTableSeparator(separator) {
					break
				}
			}
			end++
		}
		plain := strings.Join(lines[index:end], "")
		plainParts := splitPlainMarkdown(plain, limit)
		for partIndex, part := range plainParts {
			appendSegment(part)
			if partIndex < len(plainParts)-1 {
				flush()
			}
		}
		index = end
	}
	flush()
	return chunks
}

func splitFenceBlock(lines []string, fence fenceSpec, limit int) []string {
	block := strings.Join(lines, "")
	if runeCount(block) <= limit {
		return []string{block}
	}
	opening, _ := splitLineEnding(lines[0])
	open := opening + "\n"
	close := strings.Repeat(string(fence.marker), fence.length)
	bodyEnd := len(lines)
	if bodyEnd > 1 {
		last, _ := splitLineEnding(lines[bodyEnd-1])
		if isFenceClose(last, fence) {
			bodyEnd--
		}
	}
	body := strings.Join(lines[1:bodyEnd], "")
	bodyLimit := limit - runeCount(open) - runeCount(close) - 1
	if bodyLimit < 1 {
		return splitPlainMarkdown(block, limit)
	}
	bodyParts := splitPlainMarkdown(body, bodyLimit)
	if len(bodyParts) == 0 {
		bodyParts = []string{""}
	}
	parts := make([]string, 0, len(bodyParts))
	for _, bodyPart := range bodyParts {
		part := open + bodyPart
		if !strings.HasSuffix(part, "\n") {
			part += "\n"
		}
		parts = append(parts, part+close)
	}
	return parts
}

func splitTableBlock(lines []string, limit int) []string {
	block := strings.Join(lines, "")
	if runeCount(block) <= limit {
		return []string{block}
	}
	header := strings.Join(lines[:2], "")
	if runeCount(header) >= limit {
		return splitPlainMarkdown(block, limit)
	}
	var parts []string
	current := header
	rows := 0
	for _, row := range lines[2:] {
		if runeCount(header)+runeCount(row) > limit {
			if rows > 0 {
				parts = append(parts, current)
				current, rows = header, 0
			}
			parts = append(parts, splitPlainMarkdown(row, limit)...)
			continue
		}
		if runeCount(current)+runeCount(row) > limit {
			parts = append(parts, current)
			current, rows = header, 0
		}
		current += row
		rows++
	}
	if rows > 0 {
		parts = append(parts, current)
	}
	return parts
}

func splitPlainMarkdown(text string, limit int) []string {
	if text == "" || limit <= 0 {
		return nil
	}
	if parts, ok := splitMarkedLine(text, limit); ok {
		return parts
	}
	runes := []rune(text)
	var parts []string
	for len(runes) > limit {
		cut := chooseMarkdownCut(runes, limit)
		if cut <= 0 || cut > limit {
			cut = limit
		}
		parts = append(parts, string(runes[:cut]))
		runes = runes[cut:]
	}
	if len(runes) > 0 {
		parts = append(parts, string(runes))
	}
	return parts
}

func splitMarkedLine(text string, limit int) ([]string, bool) {
	line, ending := splitLineEnding(text)
	if runeCount(text) <= limit || strings.ContainsRune(line, '\n') {
		return nil, false
	}
	match := listItemRe.FindStringSubmatch(line)
	if match == nil {
		match = blockQuoteRe.FindStringSubmatch(line)
	}
	if match == nil {
		return nil, false
	}
	marker := match[1]
	contentLimit := limit - runeCount(marker) - runeCount(ending)
	if contentLimit < 1 {
		return nil, false
	}
	body := strings.TrimPrefix(line, marker)
	bodyParts := splitPlainMarkdown(body, contentLimit)
	parts := make([]string, len(bodyParts))
	for index, part := range bodyParts {
		parts[index] = marker + part
	}
	parts[len(parts)-1] += ending
	return parts, true
}

func chooseMarkdownCut(runes []rune, limit int) int {
	safe := markdownSafeCuts(runes[:min(limit+1, len(runes))])
	bestBlank, bestHeading, bestStructured, bestLine, bestSpace := 0, 0, 0, 0, 0
	lineStart := 0
	for index := 0; index < limit && index < len(runes); index++ {
		cut := index + 1
		if !safe[cut] {
			continue
		}
		if unicode.IsSpace(runes[index]) {
			bestSpace = cut
		}
		if runes[index] != '\n' {
			continue
		}
		bestLine = cut
		if index > 0 && runes[index-1] == '\n' {
			bestBlank = cut
		}
		lineStart = cut
		if lineStart < len(runes) {
			line := string(runes[lineStart:min(lineStart+80, len(runes))])
			if headingRe.MatchString(strings.TrimLeft(line, " \t")) {
				bestHeading = lineStart
			} else if listItemRe.MatchString(line) || blockQuoteRe.MatchString(line) {
				bestStructured = lineStart
			}
		}
	}
	for _, candidate := range []int{bestBlank, bestHeading, bestStructured, bestLine, bestSpace} {
		if candidate > 0 {
			return candidate
		}
	}
	return limit
}

func markdownSafeCuts(runes []rune) []bool {
	safe := make([]bool, len(runes)+1)
	for index := range safe {
		safe[index] = true
	}
	inlineTicks := 0
	linkStart, linkDepth := -1, 0
	for index := 0; index < len(runes); {
		if linkDepth > 0 {
			safe[index+1] = false
			if runes[index] == '(' {
				linkDepth++
			} else if runes[index] == ')' {
				linkDepth--
				if linkDepth == 0 {
					safe[index+1] = true
				}
			}
			index++
			continue
		}
		if runes[index] == '`' {
			run := 1
			for index+run < len(runes) && runes[index+run] == '`' {
				run++
			}
			closing := inlineTicks != 0 && run == inlineTicks
			if inlineTicks == 0 {
				inlineTicks = run
			}
			for offset := 1; offset <= run; offset++ {
				safe[index+offset] = false
			}
			index += run
			if closing {
				inlineTicks = 0
				safe[index] = true
			}
			continue
		}
		if inlineTicks != 0 {
			safe[index+1] = false
			index++
			continue
		}
		if runes[index] == '[' {
			linkStart = index
		}
		if runes[index] == ']' && index+1 < len(runes) && runes[index+1] == '(' && linkStart >= 0 {
			for position := linkStart + 1; position <= index+2; position++ {
				safe[position] = false
			}
			linkDepth = 1
			index += 2
			continue
		}
		index++
	}
	return safe
}

func isTableHeader(line string) bool {
	return strings.Contains(line, "|") && strings.TrimSpace(line) != ""
}

func isTableSeparator(line string) bool {
	trimmed := strings.TrimSpace(strings.Trim(strings.TrimSpace(line), "|"))
	if trimmed == "" {
		return false
	}
	for _, cell := range strings.Split(trimmed, "|") {
		cell = strings.TrimSpace(cell)
		cell = strings.TrimPrefix(cell, ":")
		cell = strings.TrimSuffix(cell, ":")
		if len(cell) < 3 || strings.Trim(cell, "-") != "" {
			return false
		}
	}
	return true
}

func isTableRow(line string) bool {
	return strings.Contains(line, "|") && strings.TrimSpace(line) != ""
}

func splitLinesKeepEnd(text string) []string {
	if text == "" {
		return nil
	}
	lines := strings.SplitAfter(text, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func splitLineEnding(line string) (string, string) {
	if strings.HasSuffix(line, "\r\n") {
		return strings.TrimSuffix(line, "\r\n"), "\r\n"
	}
	if strings.HasSuffix(line, "\n") {
		return strings.TrimSuffix(line, "\n"), "\n"
	}
	return line, ""
}

func splitHardRunes(text string, limit int) []string {
	if limit <= 0 {
		return nil
	}
	runes := []rune(text)
	parts := make([]string, 0, (len(runes)+limit-1)/limit)
	for len(runes) > 0 {
		end := min(limit, len(runes))
		parts = append(parts, string(runes[:end]))
		runes = runes[end:]
	}
	return parts
}

func runeCount(text string) int {
	return utf8.RuneCountInString(text)
}

func contentSHA256(content string) string {
	hash := sha256.Sum256([]byte(content))
	return fmt.Sprintf("%x", hash)
}
