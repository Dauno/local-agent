package opencodeshim

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// ParsedRun is the normalized outcome of one `opencode run --format json`
// invocation. Tool payloads and reasoning never appear here.
type ParsedRun struct {
	Text          string
	SessionFailed bool
}

// openCodeEvent decodes only the fields the mapper needs. Native tool input,
// output, and metadata are intentionally absent so an oversized payload is
// never materialized as an unbounded object.
type openCodeEvent struct {
	Type string `json:"type"`
	Part *struct {
		Type  string `json:"type"`
		ID    string `json:"id"`
		Text  string `json:"text"`
		Tool  string `json:"tool"`
		State *struct {
			Status string `json:"status"`
		} `json:"state"`
	} `json:"part"`
	Error json.RawMessage `json:"error"`
}

// ParseRunEvents drains OpenCode's JSON event stream to EOF under the mapper's
// raw line and aggregate stdout bounds. Completed text parts accumulate in
// event order (deduplicated by part id so streamed updates never double).
// Tool parts invoke onToolActivity. Reasoning and step events are ignored.
func ParseRunEvents(reader io.Reader, bounds Bounds, onToolActivity func(name, status string)) (ParsedRun, error) {
	bounds = bounds.withDefaults()
	buffered := bufio.NewReaderSize(reader, bounds.MaxRawLineBytes)

	var (
		parsed         ParsedRun
		partOrder      []string
		partText       = make(map[string]string)
		syntheticIndex int
		totalBytes     int64
	)

	for {
		line, consumed, truncated, err := readBoundedLine(buffered, bounds.MaxRawLineBytes)
		if err == io.EOF {
			break
		}
		if err != nil {
			return ParsedRun{}, fmt.Errorf("read opencode stdout: %w", err)
		}
		totalBytes += int64(consumed)
		if totalBytes > int64(bounds.MaxRawStdoutBytes) {
			return ParsedRun{}, fmt.Errorf("opencode stdout exceeded %d bytes", bounds.MaxRawStdoutBytes)
		}
		if truncated {
			// Reject before unmarshalling. Native tool payloads are opaque and
			// must not bypass the mapper's raw line bound.
			return ParsedRun{}, fmt.Errorf("opencode emitted an event longer than %d bytes", bounds.MaxRawLineBytes)
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}

		var event openCodeEvent
		if err := json.Unmarshal(line, &event); err != nil {
			// Stray non-JSON diagnostics are ignored.
			continue
		}

		switch event.Type {
		case "text":
			if event.Part == nil || event.Part.Type != "text" {
				continue
			}
			id := event.Part.ID
			if id == "" {
				id = "synthetic-" + strconv.Itoa(syntheticIndex)
				syntheticIndex++
			}
			if _, seen := partText[id]; !seen {
				partOrder = append(partOrder, id)
			}
			partText[id] = event.Part.Text
		case "tool_use":
			if event.Part == nil || event.Part.Type != "tool" || event.Part.State == nil {
				continue
			}
			status := event.Part.State.Status
			if status != "completed" && status != "error" {
				continue
			}
			if onToolActivity != nil {
				onToolActivity(event.Part.Tool, status)
			}
		case "error":
			if len(event.Error) == 0 || bytes.Equal(bytes.TrimSpace(event.Error), []byte("null")) {
				continue
			}
			parsed.SessionFailed = true
		default:
			// step_start, step_finish, reasoning, and unknown events are
			// ignored. Reasoning must never become final ADK text.
		}
	}

	segments := make([]string, 0, len(partOrder))
	for _, id := range partOrder {
		if text := partText[id]; strings.TrimSpace(text) != "" {
			segments = append(segments, text)
		}
	}
	parsed.Text = strings.Join(segments, "\n")
	return parsed, nil
}

// readBoundedLine returns one bounded prefix and the exact number of consumed
// bytes, including discarded oversized content and a trailing newline.
func readBoundedLine(reader *bufio.Reader, maxLineBytes int) ([]byte, int, bool, error) {
	var (
		prefix       []byte
		consumed     int
		contentBytes int
	)
	for {
		fragment, err := reader.ReadSlice('\n')
		consumed += len(fragment)
		fragmentContent := len(fragment)
		if fragmentContent > 0 && fragment[fragmentContent-1] == '\n' {
			fragmentContent--
		}
		contentBytes += fragmentContent
		if remaining := maxLineBytes - len(prefix); remaining > 0 {
			if remaining > len(fragment) {
				remaining = len(fragment)
			}
			prefix = append(prefix, fragment[:remaining]...)
		}

		switch err {
		case nil:
			return bytes.TrimRight(prefix, "\r\n"), consumed, contentBytes > maxLineBytes, nil
		case bufio.ErrBufferFull:
			continue
		case io.EOF:
			if consumed == 0 {
				return nil, 0, false, io.EOF
			}
			return bytes.TrimRight(prefix, "\r\n"), consumed, contentBytes > maxLineBytes, nil
		default:
			return bytes.TrimRight(prefix, "\r\n"), consumed, contentBytes > maxLineBytes, err
		}
	}
}
