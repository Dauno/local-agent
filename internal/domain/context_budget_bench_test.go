package domain_test

import (
	"testing"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

func BenchmarkNewRequestBudget(b *testing.B) {
	policy := domain.RequestBudgetPolicy{
		MaxRequestPercent: 60,
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = domain.NewRequestBudget(128_000, policy)
	}
}
