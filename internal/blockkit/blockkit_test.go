package blockkit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	slackapi "github.com/slack-go/slack"
)

type confirmationView struct {
	Summary    string    `bk:"summary"`
	CallID     string    `bk:"call_id"`
	ExpiresAt  time.Time `bk:"expires_at"`
	Project    string    `bk:"project,omitempty"`
	Details    []Pair    `bk:"details,omitempty"`
	Payload    string    `bk:"payload,omitempty"`
	Advanced   bool      `bk:"advanced,omitempty"`
	SourceCode string    `bk:"source_code,omitempty"`
	Count      int64     `bk:"count,omitempty"`
	Kind       string    `bk:"kind,omitempty"`
}

func (confirmationView) Template() string { return "confirmation.prompt" }

type settingsView struct{}

func (settingsView) Template() string { return "modal.settings" }

type explicitFallbackView struct {
	Summary string `bk:"summary"`
}

func (explicitFallbackView) Template() string { return "explicit.fallback" }

type fallbackProbeView struct {
	Text     string `bk:"text"`
	CallID   string `bk:"call_id"`
	Asterisk string `bk:"asterisk"`
}

func (fallbackProbeView) Template() string { return "bad" }

func TestEngineLoadsAndRendersTemplates(t *testing.T) {
	engine, err := New(os.DirFS("testdata"))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := engine.Register(confirmationView{}, settingsView{}, explicitFallbackView{}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if got, want := engine.Names(), []string{"confirmation.prompt", "explicit.fallback", "modal.settings"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Names() = %#v, want %#v", got, want)
	}
	view := confirmationView{
		Summary:    "Write <@U123> & report",
		CallID:     "call-1",
		ExpiresAt:  time.Date(2030, time.January, 2, 15, 4, 5, 0, time.UTC),
		Project:    "repo",
		Details:    []Pair{{Label: "Owner", Value: "Ada"}},
		Payload:    strings.Repeat("payload ", 8),
		Advanced:   true,
		SourceCode: "fmt.Println(\"quoted\")",
		Count:      7,
		Kind:       "urgent",
	}
	message, err := engine.Message(view)
	if err != nil {
		t.Fatalf("Message() error = %v", err)
	}
	if len(message.Blocks) < 8 {
		t.Fatalf("rendered %d blocks, want regions and container children", len(message.Blocks))
	}
	if !Reachable(message, view.Summary) {
		t.Fatal("summary did not reach the compiled tree")
	}
	section, ok := message.Blocks[0].(*slackapi.SectionBlock)
	if !ok || section.Text == nil || section.Text.Text != "Write &lt;@U123> &amp; report" {
		t.Fatalf("escaped summary block = %#v", message.Blocks[0])
	}
	if slot, ok := SlotOf(message, "summary"); !ok || slot != "mrkdwn" {
		t.Fatalf("SlotOf(summary) = %q, %t", slot, ok)
	}
	if message.FallbackText == "" || strings.Contains(message.FallbackText, "*") {
		t.Fatalf("derived fallback = %q", message.FallbackText)
	}
	encoded, err := json.Marshal(message.Blocks)
	if err != nil || !strings.Contains(string(encoded), `fmt.Println(\"quoted\")`) {
		t.Fatalf("JSON-safe rendered blocks = %s, error = %v", encoded, err)
	}
	if digest, ok := engine.LayoutSHA256("confirmation.prompt"); !ok || digest != message.LayoutSHA256 || len(digest) != 64 {
		t.Fatalf("layout digest = %q, %t", digest, ok)
	}
	minimal, err := engine.Message(confirmationView{
		Summary: "Summary", CallID: "call-2",
		ExpiresAt: time.Date(2030, time.January, 2, 15, 4, 5, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("minimal message error = %v", err)
	}
	container, ok := minimal.Blocks[2].(*slackapi.ContainerBlock)
	if !ok || len(container.ChildBlocks.BlockSet) != 1 {
		t.Fatalf("minimal container = %#v", minimal.Blocks[2])
	}
	if got := engine.ActionIDs(); len(got) != 2 || got[0] != "local_agent.confirm.approve" || got[1] != "local_agent.confirm.reject" {
		t.Fatalf("ActionIDs() = %#v", got)
	}

	explicit, err := engine.Message(explicitFallbackView{Summary: "A & B"})
	if err != nil || explicit.FallbackText != "Fallback: A & B" {
		t.Fatalf("explicit fallback = %q, error = %v", explicit.FallbackText, err)
	}

	modal, err := engine.Modal(settingsView{})
	if err != nil {
		t.Fatalf("Modal() error = %v", err)
	}
	if modal.Type != slackapi.VTModal || modal.Title == nil || modal.Title.Text != "Settings" {
		t.Fatalf("modal = %#v", modal)
	}
}

func TestDerivedFallbackPreservesValuesAndCleansOnlyLayoutMarkup(t *testing.T) {
	engine, err := newSingleTemplateEngine(t, makeDocument(
		`{"inputs":{"text":{"type":"text","required":true},"call_id":{"type":"id","required":true},"asterisk":{"type":"text","required":true}},"actions":{}}`,
		`[{"type":"section","text":{"type":"mrkdwn","text":"*Bold label:* {{text}}"}},{"type":"section","text":{"type":"plain_text","text":"Call ID: {{call_id}}"}},{"type":"section","text":{"type":"plain_text","text":"Literal: {{asterisk}}"}},{"type":"section","text":{"type":"plain_text","text":"# Header\n- Bullet"}}]`,
	))
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Register(fallbackProbeView{}); err != nil {
		t.Fatal(err)
	}
	message, err := engine.Message(fallbackProbeView{
		Text: "snake_case_value", CallID: "call_00_ABC_def", Asterisk: "a *literal* asterisk",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "Bold label: snake_case_value\nCall ID: call_00_ABC_def\nLiteral: a *literal* asterisk\nHeader\nBullet"
	if message.FallbackText != want {
		t.Fatalf("derived fallback = %q, want %q", message.FallbackText, want)
	}
}

func TestRenderTreatsInputAsInertData(t *testing.T) {
	engine, err := New(os.DirFS("testdata"))
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Register(confirmationView{}); err != nil {
		t.Fatal(err)
	}
	base := confirmationView{
		Summary: "safe", CallID: "call-1",
		ExpiresAt: time.Date(2030, time.January, 2, 15, 4, 5, 0, time.UTC),
	}
	baseline, err := engine.Message(base)
	if err != nil {
		t.Fatalf("baseline Message() error = %v", err)
	}
	base.Summary = `x", {"type":"actions","block_id":"pwn"}, {"type":"section","text":{"type":"mrkdwn","text":"y`
	injected, err := engine.Message(base)
	if err != nil {
		t.Fatalf("injected Message() error = %v", err)
	}
	if len(injected.Blocks) != len(baseline.Blocks) {
		t.Fatalf("injected block count = %d, baseline = %d", len(injected.Blocks), len(baseline.Blocks))
	}
	encoded, err := json.Marshal(injected.Blocks)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"block_id":"pwn"`) {
		t.Fatalf("injected structure appeared in JSON: %s", encoded)
	}
}

func TestNewRejectsModalTemplatesWithoutRequiredMetadata(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			"title",
			`{"schema_version":2,"surface":"modal","contract":{"inputs":{},"actions":{}},"layout":[{"type":"divider"}]}`,
			"title is required",
		},
		{
			"callback id",
			`{"schema_version":2,"surface":"modal","title":{"type":"plain_text","text":"Title"},"contract":{"inputs":{},"actions":{}},"layout":[{"type":"divider"}]}`,
			"callback_id is required",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := newSingleTemplateEngine(t, test.body)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("New() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestNewRejectsEmptyConditionalContainerInMinimalVariant(t *testing.T) {
	_, err := New(os.DirFS("testdata-invalid"))
	if err == nil || !strings.Contains(err.Error(), "conditional_container.json") ||
		!strings.Contains(err.Error(), "minimal variant") || !strings.Contains(err.Error(), "child_blocks") {
		t.Fatalf("New() error = %v, want minimal variant and child_blocks", err)
	}
}

func TestEngineRejectsInvalidTemplatesWithSourcePath(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{"schema", baseDocument(`"schema_version": 1`), "schema_version"},
		{"surface", baseDocument(`"surface": "home"`), "surface"},
		{"chunk", baseDocument(`"contract": {"inputs": {"value": {"type": "text", "chunk": 2}}, "actions": {}}`), "chunk"},
		{"one_of", baseDocument(`"contract": {"inputs": {"value": {"type": "text", "one_of": ["a"]}}, "actions": {}}`), "one_of"},
		{"unknown placeholder", baseDocument(`"layout": [{"type": "section", "text": {"type": "plain_text", "text": "{{missing}}"}}]`), "missing"},
		{"unused input", baseDocument(`"contract": {"inputs": {"value": {"type": "text"}}, "actions": {}}`), "unused"},
		{
			"unbalanced",
			baseDocument(
				`"contract": {"inputs": {"value": {"type": "text"}}, "actions": {}}`,
				`"layout": [{"type": "section", "text": {"type": "plain_text", "text": "{{value"}}]`,
			),
			"unbalanced",
		},
		{
			"fixed placeholder",
			baseDocument(
				`"contract": {"inputs": {"value": {"type": "text"}}, "actions": {}}`,
				`"layout": [{"type": "section", "block_id": "{{value}}", "text": {"type": "plain_text", "text": "static"}}]`,
			),
			"placeholder",
		},
		{
			"wrong modifier slot",
			baseDocument(
				`"contract": {"inputs": {"value": {"type": "text"}}, "actions": {}}`,
				`"layout": [{"type": "section", "text": {"type": "plain_text", "text": "{{value:bold}}"}}]`,
			),
			"requires an mrkdwn",
		},
		{"bad action carries", baseDocument(`"contract": {"inputs": {}, "actions": {"go": {"id": "go", "text": "Go", "carries": "missing"}}}`, `"layout": [{"actions": ["go"]}]`), "carries"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, "templates"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "templates", "bad.json"), []byte(test.body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := New(os.DirFS(root))
			if err == nil || !strings.Contains(err.Error(), "bad.json") || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("New() error = %v, want %q and source path", err, test.wantErr)
			}
		})
	}
}

func baseDocument(overrides ...string) string {
	body := `{
		"schema_version": 2,
		"surface": "message",
		"contract": {"inputs": {}, "actions": {}},
		"layout": [{"type": "section", "text": {"type": "plain_text", "text": "fixed"}}]
	}`
	for _, override := range overrides {
		switch {
		case strings.HasPrefix(override, `"schema_version"`):
			body = strings.Replace(body, `"schema_version": 2`, override, 1)
		case strings.HasPrefix(override, `"surface"`):
			body = strings.Replace(body, `"surface": "message"`, override, 1)
		case strings.HasPrefix(override, `"contract"`):
			body = strings.Replace(body, `"contract": {"inputs": {}, "actions": {}}`, override, 1)
		case strings.HasPrefix(override, `"layout"`):
			body = strings.Replace(body, `"layout": [{"type": "section", "text": {"type": "plain_text", "text": "fixed"}}]`, override, 1)
		}
	}
	return body
}
