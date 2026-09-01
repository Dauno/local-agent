package blockkit

import (
	"reflect"
	"testing"
	"time"
)

func TestEscapeSlotMatrix(t *testing.T) {
	probe := `<@U123> & <`
	plainControl := `&lt;@U123> & <`
	mrkdwnControl := `&lt;@U123> &amp; &lt;`
	mrkdwnEscaped := `&lt;@U123&gt; &amp; &lt;`
	tests := []struct {
		name      string
		inputType InputType
		slot      string
		value     string
		want      string
		wantErr   string
	}{
		{"text plain_text", InputTypeText, "plain_text", probe, plainControl, ""},
		{"text mrkdwn", InputTypeText, "mrkdwn", probe, mrkdwnControl, ""},
		{"id plain_text", InputTypeID, "plain_text", probe, plainControl, ""},
		{"id mrkdwn", InputTypeID, "mrkdwn", probe, mrkdwnEscaped, ""},
		{"code plain_text", InputTypeCode, "plain_text", probe, plainControl, ""},
		{"code mrkdwn", InputTypeCode, "mrkdwn", probe, "```" + plainControl + "```", ""},
		{"longtext plain_text", InputTypeLongText, "plain_text", probe, plainControl, ""},
		{"longtext mrkdwn", InputTypeLongText, "mrkdwn", probe, mrkdwnControl, ""},
		{"timestamp plain_text", InputTypeTimestamp, "plain_text", probe, probe, ""},
		{"timestamp mrkdwn", InputTypeTimestamp, "mrkdwn", probe, probe, ""},
		{"number plain_text", InputTypeNumber, "plain_text", probe, probe, ""},
		{"number mrkdwn", InputTypeNumber, "mrkdwn", probe, probe, ""},
		{"enum plain_text", InputTypeEnum, "plain_text", probe, probe, ""},
		{"enum mrkdwn", InputTypeEnum, "mrkdwn", probe, probe, ""},
		{"bool plain_text", InputTypeBool, "plain_text", "true", "", "bool input is not renderable in text"},
		{"bool mrkdwn", InputTypeBool, "mrkdwn", "true", "", "bool input is not renderable in text"},
		{"list pair plain_text", InputTypeListPair, "plain_text", "item", "", "list<pair> input is not renderable in text"},
		{"list pair mrkdwn", InputTypeListPair, "mrkdwn", "item", "", "list<pair> input is not renderable in text"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.wantErr != "" {
				var value any
				if test.inputType == InputTypeBool {
					value = true
				} else {
					value = []Pair{{Label: "label", Value: "value"}}
				}
				_, err := renderTypedValue(value, test.inputType, "", test.slot)
				if err == nil || err.Error() != test.wantErr {
					t.Fatalf("renderTypedValue() error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if got := escapeSlot(test.value, test.inputType, test.slot, ""); got != test.want {
				t.Fatalf("escapeSlot() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestPlaceholderModifierMatrix(t *testing.T) {
	timestamp := time.Date(2030, time.January, 2, 15, 4, 5, 0, time.UTC)
	tests := []struct {
		name      string
		value     any
		inputType InputType
		modifier  string
		slot      string
		want      string
	}{
		{"code", "value", InputTypeText, "code", "mrkdwn", "`value`"},
		{"bold", "value", InputTypeText, "bold", "mrkdwn", "*value*"},
		{"hhmm", timestamp, InputTypeTimestamp, "hhmm", "plain_text", "15:04"},
		{"rfc3339", timestamp, InputTypeTimestamp, "rfc3339", "plain_text", "2030-01-02T15:04:05Z"},
		{"upper", "Ada", InputTypeText, "upper", "plain_text", "ADA"},
		{"lower", "ADA", InputTypeText, "lower", "plain_text", "ada"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := renderTypedValue(test.value, test.inputType, test.modifier, test.slot)
			if err != nil || got != test.want {
				t.Fatalf("renderTypedValue() = %q, error = %v, want %q", got, err, test.want)
			}
		})
	}
}

func TestLongTextChunkingAndFenceNeutralization(t *testing.T) {
	if got, want := splitLongText("ab\nc\ndef", 4), []string{"ab\nc", "\ndef"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("splitLongText() = %#v, want %#v", got, want)
	}
	fenced := "outside <@U123>\n```\ninside <@U123>\n```\noutside <@U123>"
	wantFenced := "outside &lt;@U123>\n```\ninside <@U123>\n```\noutside &lt;@U123>"
	if got := neutralizeUnsafeControls(fenced); got != wantFenced {
		t.Fatalf("neutralizeUnsafeControls(fenced) = %q, want %q", got, wantFenced)
	}
	tilde := "~~~\ninside <@U123>\n~~~"
	if got, want := neutralizeUnsafeControls(tilde), tilde; got != want {
		t.Fatalf("neutralizeUnsafeControls(tilde) = %q, want %q", got, want)
	}
	inline := "`inside <@U123>` and <@U123>"
	if got, want := neutralizeUnsafeControls(inline), "`inside <@U123>` and &lt;@U123>"; got != want {
		t.Fatalf("neutralizeUnsafeControls(inline) = %q, want %q", got, want)
	}
}
