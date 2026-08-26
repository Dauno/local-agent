package domain_test

import (
	"testing"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

func BenchmarkNewRequestBudget(b *testing.B) {
	policy := domain.RequestBudgetPolicy{
		MaxRequestPercent: 60,
	}
	for b.Loop() {
		_, _ = domain.NewRequestBudget(128_000, policy)
	}
}
