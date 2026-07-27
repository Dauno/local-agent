package domain

import (
	"strings"
	"testing"
)

var benchmarkLimitedMessages []Message

func BenchmarkLimitMessages(b *testing.B) {
	for _, test := range []struct {
		name  string
		count int
	}{
		{name: "10_messages", count: 10},
		{name: "100_messages", count: 100},
		{name: "1000_messages", count: 1000},
	} {
		b.Run(test.name, func(b *testing.B) {
			messages := make([]Message, test.count)
			for i := range messages {
				messages[i] = Message{
					Role:    RoleUser,
					Content: strings.Repeat("message content ", 13)[:200],
				}
			}
			limits := ContextLimits{MaxMessages: 1000, MaxChars: 100_000}

			b.ResetTimer()
			for range b.N {
				benchmarkLimitedMessages = LimitMessages(messages, limits)
			}
		})
	}
}
