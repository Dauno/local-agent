package blockkit

import (
	"os"
	"strings"
	"testing"
	"time"
)

type wrongBindingView struct {
	Summary string `bk:"summary"`
	CallID  int    `bk:"call_id"`
}

func (wrongBindingView) Template() string { return "confirmation.prompt" }

type missingBindingView struct {
	Summary string `bk:"summary"`
}

func (missingBindingView) Template() string { return "confirmation.prompt" }

type optionalBindingView struct {
	Summary    string    `bk:"summary"`
	CallID     string    `bk:"call_id"`
	ExpiresAt  time.Time `bk:"expires_at"`
	Project    string    `bk:"project"`
	Details    []Pair    `bk:"details,omitempty"`
	Payload    string    `bk:"payload,omitempty"`
	Advanced   bool      `bk:"advanced,omitempty"`
	SourceCode string    `bk:"source_code,omitempty"`
	Count      int64     `bk:"count,omitempty"`
	Kind       string    `bk:"kind,omitempty"`
}

func (optionalBindingView) Template() string { return "confirmation.prompt" }

type unknownBindingView struct {
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
	Extra      string    `bk:"extra"`
}

func (unknownBindingView) Template() string { return "confirmation.prompt" }

func TestRegisterRejectsContractBindingMismatches(t *testing.T) {
	tests := []struct {
		name string
		view View
		want string
	}{
		{"wrong Go type", wrongBindingView{}, "does not match id"},
		{"missing field", missingBindingView{}, "has no field"},
		{"optional tag", optionalBindingView{}, "omitempty does not match"},
		{"unknown field", unknownBindingView{}, "unknown input"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			engine, err := New(os.DirFS("testdata"))
			if err != nil {
				t.Fatal(err)
			}
			err = engine.Register(test.view)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Register() error = %v, want %q", err, test.want)
			}
		})
	}
}
