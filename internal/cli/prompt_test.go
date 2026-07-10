package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestPrompterDefaultsValidationAndCSV(t *testing.T) {
	input := strings.NewReader("\nmaybe\ny\nU12345678, U99999999,U12345678\n")
	var output bytes.Buffer
	prompter := NewPrompter(input, &output)
	text, err := prompter.Text("Name", "Dev Agent", true)
	if err != nil || text != "Dev Agent" {
		t.Fatalf("text=%q err=%v", text, err)
	}
	confirmed, err := prompter.Confirm("Continue", false)
	if err != nil || !confirmed {
		t.Fatalf("confirmed=%v err=%v", confirmed, err)
	}
	ids, err := prompter.CSV("Users", nil)
	if err != nil || len(ids) != 2 || ids[0] != "U12345678" || ids[1] != "U99999999" {
		t.Fatalf("ids=%#v err=%v", ids, err)
	}
}

func TestSecretIsNeverEchoed(t *testing.T) {
	const secret = "xoxb-123456789-secret"
	var output bytes.Buffer
	prompter := NewPrompter(strings.NewReader(secret+"\n"), &output)
	got, err := prompter.Secret("Bot token", "", "xoxb-")
	if err != nil || got != secret {
		t.Fatalf("secret=%q err=%v", got, err)
	}
	if strings.Contains(output.String(), secret) {
		t.Fatalf("secret echoed: %s", output.String())
	}
}
