package secure

import (
	"strings"
	"testing"
)

var benchmarkRedactedText string

func BenchmarkRedactorString(b *testing.B) {
	redactor := NewRedactor("sk-fake-key-12345", "xoxb-fake-token-67890")
	longText := strings.Repeat("ordinary request text ", 220) + "sk-fake-key-12345" + strings.Repeat(" trailing text", 40)

	inputs := []struct {
		name  string
		value string
	}{
		{name: "clean", value: "request completed without sensitive data"},
		{name: "single_secret", value: strings.Repeat("ordinary text ", 6) + "sk-fake-key-12345" + " done"},
		{name: "multiple_secrets", value: strings.Repeat("ordinary text ", 10) + "sk-fake-key-12345 and xoxb-fake-token-67890" + strings.Repeat(" done", 5)},
		{name: "long_text", value: longText},
	}

	for _, input := range inputs {
		b.Run(input.name, func(b *testing.B) {
			b.ResetTimer()
			for range b.N {
				benchmarkRedactedText = redactor.String(input.value)
			}
		})
	}
}
