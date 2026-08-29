package blockkit

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewMinimalVariantAppliesInputDefaults(t *testing.T) {
	body := makeDocument(
		`{"inputs":{"req":{"type":"text","required":true},"opt":{"type":"text","default":"FALLBACK_DEFAULT"}},"actions":{}}`,
		`[{"type":"section","text":{"type":"mrkdwn","text":"{{req}}"}},{"type":"section","text":{"type":"mrkdwn","text":"{{opt}}"}}]`,
	)
	if _, err := newSingleTemplateEngine(t, body); err != nil {
		t.Fatalf("New() rejected a defaulted optional input: %v", err)
	}
}

func TestNewRejectsOptionalInputWithoutDefaultInMinimalVariant(t *testing.T) {
	body := makeDocument(
		`{"inputs":{"req":{"type":"text","required":true},"opt":{"type":"text"}},"actions":{}}`,
		`[{"type":"section","text":{"type":"mrkdwn","text":"{{req}}"}},{"type":"section","text":{"type":"mrkdwn","text":"{{opt}}"}}]`,
	)
	_, err := newSingleTemplateEngine(t, body)
	if err == nil || !strings.Contains(err.Error(), "minimal variant") || !strings.Contains(err.Error(), "bad.json") {
		t.Fatalf("New() error = %v, want minimal variant error with source path", err)
	}
}

func TestNewRejectsOverlongModalMetadata(t *testing.T) {
	body := `{"schema_version":2,"surface":"modal","title":{"type":"plain_text","text":"1234567890123456789012345"},"callback_id":"settings","contract":{"inputs":{},"actions":{}},"layout":[{"type":"divider"}]}`
	_, err := newSingleTemplateEngine(t, body)
	if err == nil || !strings.Contains(err.Error(), "title") {
		t.Fatalf("New() error = %v, want title length error", err)
	}
}

func TestNewRejectsLayoutAndSlackValidationErrors(t *testing.T) {
	manyBlocks := make([]string, 51)
	for index := range manyBlocks {
		manyBlocks[index] = `{"type":"divider"}`
	}
	manyChildren := make([]string, 11)
	for index := range manyChildren {
		manyChildren[index] = `{"type":"divider"}`
	}
	manyFields := make([]string, 11)
	for index := range manyFields {
		manyFields[index] = `{"type":"plain_text","text":"x"}`
	}
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			"unknown document field",
			`{"schema_version":2,"surface":"message","contract":{"inputs":{},"actions":{}},"layout":[{"type":"divider"}],"extra":true}`,
			"unknown field",
		},
		{
			"duplicate JSON key",
			`{"schema_version":2,"schema_version":2,"surface":"message","contract":{"inputs":{},"actions":{}},"layout":[{"type":"divider"}]}`,
			"duplicate JSON key",
		},
		{
			"blocked expression",
			baseDocument(`"layout": [{"type": "section", "text": {"type": "plain_text", "text": "${value}"}}]`),
			"blocked expression",
		},
		{
			"unknown modifier",
			baseDocument(
				`"contract": {"inputs": {"value": {"type": "text"}}, "actions": {}}`,
				`"layout": [{"type": "section", "text": {"type": "plain_text", "text": "{{value:unknown}}"}}]`,
			),
			"unknown placeholder modifier",
		},
		{
			"unknown block",
			baseDocument(`"layout": [{"type": "future_block"}]`),
			"unknown block type",
		},
		{
			"unknown element",
			baseDocument(`"layout": [{"type": "actions", "elements": [{"type": "future_element"}]}]`),
			"unsupported block element type",
		},
		{
			"duplicate block IDs",
			baseDocument(`"layout": [{"type":"divider","block_id":"same"},{"type":"divider","block_id":"same"}]`),
			"duplicate block_id",
		},
		{
			"unknown action ID",
			baseDocument(`"layout": [{"type":"section","text":{"type":"plain_text","text":"x"},"accessory":{"type":"button","text":{"type":"plain_text","text":"Go"},"action_id":"unknown"}}]`),
			"not declared",
		},
		{
			"nested message input",
			makeDocument(
				`{"inputs":{},"actions":{"go":{"id":"go","text":"Go"}}}`,
				`[{"type":"container","title":{"type":"plain_text","text":"x"},"child_blocks":[{"type":"input","block_id":"input","label":{"type":"plain_text","text":"x"},"element":{"type":"plain_text_input","action_id":"go"}}]}]`,
			),
			"input blocks are not valid",
		},
		{
			"block count",
			makeDocument(`{"inputs":{},"actions":{}}`, `[`+strings.Join(manyBlocks, ",")+`]`),
			"maximum is 50",
		},
		{
			"container child count",
			makeDocument(`{"inputs":{},"actions":{}}`, `[{"type":"container","title":{"type":"plain_text","text":"x"},"child_blocks":`+`[`+strings.Join(manyChildren, ",")+`]}`+`]`),
			"more than 10",
		},
		{
			"section field count",
			makeDocument(`{"inputs":{},"actions":{}}`, `[{"type":"section","fields":`+`[`+strings.Join(manyFields, ",")+`]}`+`]`),
			"fields exceeds 10",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := newSingleTemplateEngine(t, test.body)
			if err == nil || !strings.Contains(err.Error(), test.want) || !strings.Contains(err.Error(), "bad.json") {
				t.Fatalf("New() error = %v, want %q and source path", err, test.want)
			}
		})
	}
}

func newSingleTemplateEngine(t *testing.T, body string) (*Engine, error) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "templates"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "templates", "bad.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return New(os.DirFS(root))
}

func makeDocument(contract, layout string) string {
	return fmt.Sprintf(`{"schema_version":2,"surface":"message","contract":%s,"layout":%s}`, contract, layout)
}
