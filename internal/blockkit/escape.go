package blockkit

import (
	"regexp"
	"strings"
)

var slackControlPattern = regexp.MustCompile(
	`<@U[A-Z0-9]+(\|[^>]*)?>|` +
		`<!subteam\^[A-Z0-9]+(\|[^>]*)?>|` +
		`<!here>|` +
		`<!channel>|` +
		`<!everyone>|` +
		`<#C[A-Z0-9]+(\|[^>]*)?>|` +
		`<!date\^[^>]+(\|[^>]*)?>`,
)

type fenceSpec struct {
	marker byte
	length int
}

func escapeSlot(value string, inputType InputType, slot, modifier string) string {
	var escaped string
	if slot == "mrkdwn" {
		switch inputType {
		case InputTypeText, InputTypeLongText:
			escaped = escapeMrkdwn(value, true)
		case InputTypeCode:
			escaped = "```" + neutralizeUnsafeControls(value) + "```"
		case InputTypeEnum, InputTypeNumber, InputTypeTimestamp:
			escaped = value
		default:
			escaped = escapeMrkdwn(value, false)
		}
	} else {
		switch inputType {
		case InputTypeText, InputTypeID, InputTypeCode, InputTypeLongText:
			escaped = neutralizeUnsafeControls(value)
		default:
			escaped = value
		}
	}
	if modifier == "code" {
		escaped = "`" + escaped + "`"
	}
	if modifier == "bold" {
		escaped = "*" + escaped + "*"
	}
	return escaped
}

func escapeMrkdwn(value string, neutralizeControls bool) string {
	var result strings.Builder
	result.Grow(len(value))
	for index := 0; index < len(value); {
		if neutralizeControls {
			if match := slackControlPattern.FindStringIndex(value[index:]); match != nil && match[0] == 0 {
				end := index + match[1]
				result.WriteString("&lt;")
				result.WriteString(value[index+1 : end])
				index = end
				continue
			}
		}
		switch value[index] {
		case '&':
			result.WriteString("&amp;")
		case '<':
			result.WriteString("&lt;")
		case '>':
			result.WriteString("&gt;")
		default:
			result.WriteByte(value[index])
		}
		index++
	}
	return result.String()
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
	for index := 0; index < len(line); {
		if line[index] == '`' {
			run := countByteRun(line, index, '`')
			result.WriteString(line[index : index+run])
			if *inlineTicks == 0 {
				if hasClosingBacktickRun(line, index+run, run) {
					*inlineTicks = run
				}
			} else if run == *inlineTicks {
				*inlineTicks = 0
			}
			index += run
			continue
		}
		if *inlineTicks == 0 {
			if match := slackControlPattern.FindStringIndex(line[index:]); match != nil && match[0] == 0 {
				end := index + match[1]
				result.WriteString("&lt;")
				result.WriteString(line[index+1 : end])
				index = end
				continue
			}
		}
		result.WriteByte(line[index])
		index++
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

func splitLinesKeepEnd(text string) []string {
	if text == "" {
		return nil
	}
	var lines []string
	start := 0
	for start < len(text) {
		newline := strings.IndexByte(text[start:], '\n')
		if newline < 0 {
			lines = append(lines, text[start:])
			break
		}
		end := start + newline + 1
		lines = append(lines, text[start:end])
		start = end
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
