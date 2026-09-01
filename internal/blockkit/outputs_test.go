package blockkit

import (
	"errors"
	"strings"
	"testing"

	slackapi "github.com/slack-go/slack"
)

type modalOutputTarget struct {
	Name  string `bk:"name"`
	Kind  string `bk:"kind"`
	Note  string `bk:"note,omitempty"`
	Count int    `bk:"count,omitempty"`
}

func (modalOutputTarget) Template() string { return "bad" }

func TestSubmitReadsTextAndSelectOutputs(t *testing.T) {
	engine, err := newSingleTemplateEngine(t, modalOutputDocument(
		`"name":{"type":"text","required":true},"kind":{"type":"enum","required":true,"one_of":["safe","urgent"]},"note":{"type":"text"},"count":{"type":"number"}`,
		`"name":{"type":"text","required":true},"kind":{"type":"enum","required":true,"one_of":["safe","urgent"]},"note":{"type":"text"},"count":{"type":"number"}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.RegisterSubmit(modalOutputTarget{}); err != nil {
		t.Fatal(err)
	}

	target := modalOutputTarget{}
	state := map[string]map[string]slackapi.BlockAction{
		"name":  {"name": {Value: "Ada"}},
		"kind":  {"kind": {Value: "wrong", SelectedOption: slackapi.OptionBlockObject{Value: "safe"}}},
		"note":  {"note": {Value: "A note"}},
		"count": {"count": {Value: "7"}},
	}
	if err := engine.Submit(&target, state); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if target.Name != "Ada" || target.Kind != "safe" || target.Note != "A note" || target.Count != 7 {
		t.Fatalf("submit target = %#v", target)
	}
}

func TestSubmitRejectsInvalidNumberWithItsBlock(t *testing.T) {
	engine := newModalOutputEngine(t)
	target := modalOutputTarget{Count: 42}
	state := completeModalOutputState()
	state["count"]["count"] = slackapi.BlockAction{Value: "not-a-number"}

	err := engine.Submit(&target, state)
	var submitErr *SubmitError
	if !errors.As(err, &submitErr) || submitErr.BlockID != "count" {
		t.Fatalf("Submit() error = %v, block = %q", err, submitErrBlock(err))
	}
	if target.Count != 0 || !strings.Contains(err.Error(), "integer") {
		t.Fatalf("target = %#v, error = %v", target, err)
	}
}

func TestSubmitRejectsMissingRequiredOutputWithItsBlock(t *testing.T) {
	engine := newModalOutputEngine(t)
	target := modalOutputTarget{Name: "stale"}
	state := completeModalOutputState()
	delete(state, "name")

	err := engine.Submit(&target, state)
	var submitErr *SubmitError
	if !errors.As(err, &submitErr) || submitErr.BlockID != "name" {
		t.Fatalf("Submit() error = %v, block = %q", err, submitErrBlock(err))
	}
	if target.Name != "" {
		t.Fatalf("target retained stale required value: %#v", target)
	}
}

func TestSubmitRejectsEnumValueOutsideOneOf(t *testing.T) {
	engine := newModalOutputEngine(t)
	state := completeModalOutputState()
	state["kind"]["kind"] = slackapi.BlockAction{SelectedOption: slackapi.OptionBlockObject{Value: "unsafe"}}

	err := engine.Submit(&modalOutputTarget{}, state)
	var submitErr *SubmitError
	if !errors.As(err, &submitErr) || submitErr.BlockID != "kind" || !strings.Contains(err.Error(), "one_of") {
		t.Fatalf("Submit() error = %v, block = %q", err, submitErrBlock(err))
	}
}

func TestNewRejectsInputBlockWithoutOutput(t *testing.T) {
	_, err := newSingleTemplateEngine(t, singleInputModalDocument(
		`"name":{"type":"text"}`,
		``,
	))
	if err == nil || !strings.Contains(err.Error(), `input block "name" has no declared output`) {
		t.Fatalf("New() error = %v, want undeclared input block", err)
	}
}

func TestNewRejectsOutputForUnknownBlock(t *testing.T) {
	_, err := newSingleTemplateEngine(t, singleInputModalDocument(
		`"name":{"type":"text"}`,
		`"missing":{"type":"text"}`,
	))
	if err == nil || !strings.Contains(err.Error(), `output "missing" names unknown input block`) {
		t.Fatalf("New() error = %v, want unknown input block", err)
	}
}

func TestNewRejectsOutputElementTypeMismatch(t *testing.T) {
	_, err := newSingleTemplateEngine(t, singleInputModalDocument(
		`"name":{"type":"text"}`,
		`"name":{"type":"enum","one_of":["safe"]}`,
	))
	if err == nil || !strings.Contains(err.Error(), "requires a select") {
		t.Fatalf("New() error = %v, want output element mismatch", err)
	}
}

func newModalOutputEngine(t *testing.T) *Engine {
	t.Helper()
	engine, err := newSingleTemplateEngine(t, modalOutputDocument(
		`"name":{"type":"text","required":true},"kind":{"type":"enum","required":true,"one_of":["safe","urgent"]},"note":{"type":"text"},"count":{"type":"number"}`,
		`"name":{"type":"text","required":true},"kind":{"type":"enum","required":true,"one_of":["safe","urgent"]},"note":{"type":"text"},"count":{"type":"number"}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.RegisterSubmit(modalOutputTarget{}); err != nil {
		t.Fatal(err)
	}
	return engine
}

func completeModalOutputState() map[string]map[string]slackapi.BlockAction {
	return map[string]map[string]slackapi.BlockAction{
		"name":  {"name": {Value: "Ada"}},
		"kind":  {"kind": {SelectedOption: slackapi.OptionBlockObject{Value: "safe"}}},
		"note":  {"note": {Value: "A note"}},
		"count": {"count": {Value: "7"}},
	}
}

func submitErrBlock(err error) string {
	var submitErr *SubmitError
	if errors.As(err, &submitErr) {
		return submitErr.BlockID
	}
	return ""
}

func singleInputModalDocument(inputs, outputs string) string {
	return `{"schema_version":2,"surface":"modal","title":{"type":"plain_text","text":"Submit"},"callback_id":"modal.submit","contract":{"inputs":{` + inputs + `},"outputs":{` + outputs + `},"actions":{}},"layout":[` +
		`{"type":"input","block_id":"name","label":{"type":"plain_text","text":"Name"},"element":{"type":"plain_text_input","action_id":"name"}}]}`
}

func modalOutputDocument(inputs, outputs string) string {
	return `{"schema_version":2,"surface":"modal","title":{"type":"plain_text","text":"Submit"},"callback_id":"modal.submit","contract":{"inputs":{` + inputs + `},"outputs":{` + outputs + `},"actions":{}},"layout":[` +
		`{"type":"input","block_id":"name","label":{"type":"plain_text","text":"Name"},"element":{"type":"plain_text_input","action_id":"name"}},` +
		`{"type":"input","block_id":"kind","label":{"type":"plain_text","text":"Kind"},"element":{"type":"static_select","action_id":"kind","options":[{"text":{"type":"plain_text","text":"Safe"},"value":"safe"},{"text":{"type":"plain_text","text":"Urgent"},"value":"urgent"}]}},` +
		`{"type":"input","block_id":"note","label":{"type":"plain_text","text":"Note"},"element":{"type":"plain_text_input","action_id":"note"}},` +
		`{"type":"input","block_id":"count","label":{"type":"plain_text","text":"Count"},"element":{"type":"plain_text_input","action_id":"count"}}]}`
}
