package domain_test

import (
	"testing"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

func BenchmarkDerivePercentages(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		domain.DerivePercentages(60)
	}
}

func BenchmarkValidateContextWindow(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = domain.ValidateContextWindow(128_000)
	}
}

func BenchmarkValidatePercent(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = domain.ValidatePercent(60)
	}
}

func BenchmarkContentCost(b *testing.B) {
	text := domain.Content{Role: domain.ContentRoleUser, Parts: []domain.ContentPart{{Text: "hello world this is a test message with enough text to measure cost computation overhead across many iterations"}}}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = domain.ContentCost([]domain.Content{text})
	}
}
