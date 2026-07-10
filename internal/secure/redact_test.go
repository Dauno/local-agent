package secure

import (
	"errors"
	"strings"
	"testing"
)

func TestMask(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		secret string
		want   string
	}{
		{name: "unset", secret: "", want: "<not set>"},
		{name: "short generic", secret: "tiny", want: "****"},
		{name: "generic", secret: "deepseek-secret-value", want: "****alue"},
		{name: "Slack bot token", secret: "xoxb-1234567890-secret", want: "xoxb-****cret"},
		{name: "Slack app token", secret: "xapp-1-1234567890-secret", want: "xapp-****cret"},
		{name: "OpenAI-shaped token", secret: "sk-1234567890abcdef", want: "sk-****cdef"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := Mask(tt.secret); got != tt.want {
				t.Fatalf("Mask(%q) = %q, want %q", tt.secret, got, tt.want)
			}
			if tt.secret != "" && Mask(tt.secret) == tt.secret {
				t.Fatalf("Mask(%q) exposed the original value", tt.secret)
			}
		})
	}
}

func TestRedactorString(t *testing.T) {
	t.Parallel()

	registered := "provider-key-that-must-not-leak"
	unregisteredSlack := "xoxb-1234567890-abcdef"
	redactor := NewRedactor(registered, registered, "")

	got := redactor.String("provider=" + registered + " slack=" + unregisteredSlack)
	if strings.Contains(got, registered) {
		t.Fatal("registered credential was not redacted")
	}
	if strings.Contains(got, unregisteredSlack) {
		t.Fatal("recognizable Slack token was not redacted")
	}
	if !strings.Contains(got, "provider=****leak") {
		t.Fatalf("registered credential mask not useful: %q", got)
	}
	if !strings.Contains(got, "slack=xoxb-****cdef") {
		t.Fatalf("Slack token mask not useful: %q", got)
	}
}

func TestRedactorErrorPreservesCause(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("request failed with provider-secret")
	got := NewRedactor("provider-secret").Error(sentinel)
	if got == nil {
		t.Fatal("Error(nil) semantics changed unexpectedly")
	}
	if !errors.Is(got, sentinel) {
		t.Fatal("redacted error does not unwrap to its cause")
	}
	if strings.Contains(got.Error(), "provider-secret") {
		t.Fatal("redacted error exposed the credential")
	}

	if NewRedactor().Error(nil) != nil {
		t.Fatal("Error(nil) must return nil")
	}
}
