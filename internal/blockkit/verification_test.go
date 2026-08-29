package blockkit

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	slackapi "github.com/slack-go/slack"
)

func TestParseInputValueRejectsInvalidDefaults(t *testing.T) {
	tests := []struct {
		name  string
		input Input
		text  string
		want  string
	}{
		{"timestamp", Input{Type: InputTypeTimestamp}, "not-a-timestamp", "cannot parse"},
		{"number", Input{Type: InputTypeNumber}, "not-a-number", "invalid syntax"},
		{"bool", Input{Type: InputTypeBool}, "not-a-bool", "invalid syntax"},
		{"list pair", Input{Type: InputTypeListPair}, "value", "does not support a default"},
		{"unsupported", Input{Type: InputType("future")}, "value", "unsupported input type"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseInputValue(test.input, test.text); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("parseInputValue() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestValidateInputValuesRejectsInvalidRuntimeValues(t *testing.T) {
	tests := []struct {
		name   string
		values renderValues
		want   string
	}{
		{
			"required missing",
			renderValues{"value": {input: Input{Type: InputTypeText, Required: true}}},
			"is required",
		},
		{
			"required empty",
			renderValues{"value": {input: Input{Type: InputTypeText, Required: true}, value: ""}},
			"must not be empty",
		},
		{
			"max",
			renderValues{"value": {input: Input{Type: InputTypeText, Max: 2}, value: "abc"}},
			"exceeds max",
		},
		{
			"enum",
			renderValues{"value": {input: Input{Type: InputTypeEnum, OneOf: []string{"safe"}}, value: "other"}},
			"not in one_of",
		},
		{
			"timestamp zero",
			renderValues{"value": {input: Input{Type: InputTypeTimestamp, Required: true}, value: time.Time{}}},
			"must not be zero",
		},
		{
			"list pair missing",
			renderValues{"value": {input: Input{Type: InputTypeListPair, Required: true}, value: []Pair(nil)}},
			"is required",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateInputValues(test.values); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validateInputValues() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestValidateSlackFieldLimits(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  string
	}{
		{"button value", slackapi.ButtonBlockElement{Value: strings.Repeat("x", 2001)}, "value exceeds"},
		{"option value", slackapi.OptionBlockObject{Value: strings.Repeat("x", 2001)}, "value exceeds"},
		{"image alt text", slackapi.ImageBlock{AltText: strings.Repeat("x", 2001)}, "alt_text exceeds"},
		{"image element alt text", slackapi.ImageBlockElement{AltText: strings.Repeat("x", 2001)}, "alt_text exceeds"},
		{"input max length", slackapi.PlainTextInputBlockElement{MaxLength: 3001}, "max_length exceeds"},
		{"input initial value", slackapi.PlainTextInputBlockElement{MaxLength: 2, InitialValue: "abc"}, "initial_value exceeds"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateSlackFieldLimits(test.value, "field"); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validateSlackFieldLimits() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestEngineRejectsSurfaceAndBindingMismatches(t *testing.T) {
	engine, err := New(os.DirFS("testdata"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Message(settingsView{}); err == nil || !strings.Contains(err.Error(), "not a message") {
		t.Fatalf("Message() error = %v", err)
	}
	if _, err := engine.Modal(confirmationView{}); err == nil || !strings.Contains(err.Error(), "not a modal") {
		t.Fatalf("Modal() error = %v", err)
	}
	if _, err := engine.Message(confirmationView{}); err == nil || !strings.Contains(err.Error(), "no registered view type") {
		t.Fatalf("Message() binding error = %v", err)
	}
	if _, ok := engine.LayoutSHA256("missing"); ok {
		t.Fatal("LayoutSHA256() found a missing template")
	}
}

func TestEngineNilAndTemplateHelperErrors(t *testing.T) {
	var nilEngine *Engine
	if nilEngine.Names() != nil {
		t.Fatal("nil Engine Names() was not nil")
	}
	if _, ok := nilEngine.LayoutSHA256("missing"); ok {
		t.Fatal("nil Engine returned a layout digest")
	}
	if nilEngine.ActionIDs() != nil {
		t.Fatal("nil Engine ActionIDs() was not nil")
	}
	if _, err := nilEngine.Message(confirmationView{}); err == nil {
		t.Fatal("nil Engine Message() did not return an error")
	}
	if _, err := nilEngine.Modal(settingsView{}); err == nil {
		t.Fatal("nil Engine Modal() did not return an error")
	}
	if _, err := templateNameForView((*confirmationView)(nil)); err == nil {
		t.Fatal("templateNameForView() accepted a nil pointer")
	}
	if name, err := templateNameForView(confirmationView{}); err != nil || name != "confirmation.prompt" {
		t.Fatalf("templateNameForView() = %q, error = %v", name, err)
	}
	if got := truncateCodePoints("abc", 2); got != "ab" {
		t.Fatalf("truncateCodePoints() = %q", got)
	}
	if got := truncateCodePoints("abc", 0); got != "abc" {
		t.Fatalf("truncateCodePoints() changed a non-positive limit: %q", got)
	}
}

func TestRenderTextObjectAndReachableErrors(t *testing.T) {
	doc := templateDocument{}
	if text, err := renderTextObject("Title", doc, nil, "title"); err != nil || text.Text != "Title" {
		t.Fatalf("renderTextObject(string) = %#v, error = %v", text, err)
	}
	if _, err := renderTextObject(42, doc, nil, "title"); err == nil {
		t.Fatal("renderTextObject() accepted a number")
	}
	if _, err := renderTextObject(map[string]any{"type": "mrkdwn", "text": "text"}, doc, nil, "title"); err == nil {
		t.Fatal("renderTextObject() accepted mrkdwn metadata")
	}
	if Reachable(Message{}, "value") || Reachable(Message{}, "") {
		t.Fatal("Reachable() found a value in an empty message")
	}
	message := Message{Blocks: []slackapi.Block{
		&slackapi.SectionBlock{Type: slackapi.MBTSection, Text: &slackapi.TextBlockObject{Type: slackapi.PlainTextType, Text: "value"}},
	}}
	if !Reachable(message, "value") {
		t.Fatal("Reachable() missed a value in the message")
	}
}

func TestFieldLabelAndTextValidationErrors(t *testing.T) {
	type tagged struct {
		Empty string `bk:",omitempty"`
		Bad   string `bk:"value,bad"`
	}
	typeOf := reflect.TypeOf(tagged{})
	if _, _, err := fieldLabel(typeOf.Field(0)); err == nil {
		t.Fatal("fieldLabel() accepted an empty name")
	}
	if _, _, err := fieldLabel(typeOf.Field(1)); err == nil {
		t.Fatal("fieldLabel() accepted an invalid tag")
	}
	if err := validateTextLength(slackapi.TextBlockObject{Text: "abcd"}, 3, "text"); err == nil {
		t.Fatal("validateTextLength() accepted oversized text")
	}
	if leadingFenceIndent("     ```") != -1 || leadingFenceIndent("") != 0 {
		t.Fatal("leadingFenceIndent() returned an unexpected result")
	}
}

func TestRenderNodeSupportsTemplateJSONValues(t *testing.T) {
	doc := templateDocument{Inputs: map[string]Input{"value": {Type: InputTypeText}}}
	values := renderValues{"value": {input: doc.Inputs["value"], value: "rendered"}}
	for _, node := range []any{nil, true, float64(2), "{{value}}"} {
		if _, err := renderNode(node, doc, values, renderContext{}); err != nil {
			t.Fatalf("renderNode(%#v) error = %v", node, err)
		}
	}
	if _, err := renderNode(struct{}{}, doc, values, renderContext{}); err == nil {
		t.Fatal("renderNode accepted an unsupported Go value")
	}
	if _, err := renderNode(map[string]any{"type": "plain_text", "text": "{{value}}"}, doc, values, renderContext{}); err != nil {
		t.Fatalf("renderNode object error = %v", err)
	}
}

func TestRawValueStringSupportsActionValueTypes(t *testing.T) {
	timestamp := time.Date(2030, time.January, 2, 15, 4, 5, 0, time.UTC)
	tests := []struct {
		name  string
		value any
		type_ InputType
		want  string
	}{
		{"text", "value", InputTypeText, "value"},
		{"id", "id-1", InputTypeID, "id-1"},
		{"code", "code", InputTypeCode, "code"},
		{"longtext", "long", InputTypeLongText, "long"},
		{"enum", "safe", InputTypeEnum, "safe"},
		{"number", int64(42), InputTypeNumber, "42"},
		{"timestamp", timestamp, InputTypeTimestamp, "2030-01-02T15:04:05Z"},
		{"bool", true, InputTypeBool, "true"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := rawValueString(test.value, test.type_)
			if err != nil || got != test.want {
				t.Fatalf("rawValueString() = %q, error = %v, want %q", got, err, test.want)
			}
		})
	}
	if _, err := rawValueString([]Pair{{Label: "label", Value: "value"}}, InputTypeListPair); err == nil {
		t.Fatal("rawValueString() accepted list<pair>")
	}
}

func TestValidateGoTypeRejectsMismatches(t *testing.T) {
	tests := []struct {
		name      string
		fieldType reflect.Type
		inputType InputType
	}{
		{"timestamp", reflect.TypeOf(""), InputTypeTimestamp},
		{"list pair", reflect.TypeOf([]string{}), InputTypeListPair},
		{"number", reflect.TypeOf(true), InputTypeNumber},
		{"bool", reflect.TypeOf(""), InputTypeBool},
		{"text", reflect.TypeOf(1), InputTypeText},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateGoType(test.fieldType, test.inputType); err == nil {
				t.Fatal("validateGoType() accepted a mismatch")
			}
		})
	}
}

func TestValidationIdentifiersAndNames(t *testing.T) {
	for _, name := range []string{"", "Upper", "has-dash", "é"} {
		if validName(name) {
			t.Fatalf("validName(%q) = true", name)
		}
	}
	if !validName("valid_name_1") {
		t.Fatal("validName rejected a valid name")
	}
	identifierTests := []struct {
		name  string
		value string
		want  string
	}{
		{"empty", "", "must not be empty"},
		{"placeholder", "{{id}}", "placeholder"},
		{"long", strings.Repeat("x", 256), "exceeds"},
		{"unicode", "é", "ASCII"},
		{"space", "has space", "printable"},
	}
	for _, test := range identifierTests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateIdentifier(test.value, "id"); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validateIdentifier() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestTextLimitsForSlackFields(t *testing.T) {
	cases := []struct {
		name   string
		parent any
		field  string
		want   int
	}{
		{"section fields", slackapi.SectionBlock{}, "Fields", maxSectionField},
		{"header text", slackapi.HeaderBlock{}, "Text", maxHeaderText},
		{"container title", slackapi.ContainerBlock{}, "Title", maxHeaderText},
		{"container subtitle", slackapi.ContainerBlock{}, "Subtitle", maxHeaderText},
		{"button text", slackapi.ButtonBlockElement{}, "Text", maxButtonText},
		{"option text", slackapi.OptionBlockObject{}, "Text", maxOptionText},
		{"option description", slackapi.OptionBlockObject{}, "Description", maxOptionText},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := textLimitForField(reflect.ValueOf(test.parent), test.field, maxTextLength); got != test.want {
				t.Fatalf("textLimitForField() = %d, want %d", got, test.want)
			}
		})
	}
}

func TestSlotOfFindsAndRejectsInputsWithoutTemplateMetadata(t *testing.T) {
	message := Message{Blocks: []slackapi.Block{
		&slackapi.SectionBlock{
			Type: slackapi.MBTSection,
			Text: &slackapi.TextBlockObject{Type: slackapi.MarkdownType, Text: "Value: &lt;safe&gt;"},
		},
	}}
	if slot, ok := SlotOf(message, "<safe>"); !ok || slot != slackapi.MarkdownType {
		t.Fatalf("SlotOf() = %q, %t", slot, ok)
	}
	if _, ok := SlotOf(message, "missing"); ok {
		t.Fatal("SlotOf() found a missing input")
	}
}
