package domain_test

import (
	"strings"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

func TestNewRequestBudgetAtDefaultSixtyPercent(t *testing.T) {
	t.Parallel()

	b, err := domain.NewRequestBudget(128_000, domain.RequestBudgetPolicy{
		MaxRequestPercent: 60,
	})
	if err != nil {
		t.Fatal(err)
	}

	if b.WindowTokens != 128_000 {
		t.Fatalf("WindowTokens = %d, want 128000", b.WindowTokens)
	}
	if b.HardTokens != 76_800 {
		t.Fatalf("HardTokens = %d, want 76800", b.HardTokens)
	}
	if b.TriggerTokens != 70_400 {
		t.Fatalf("TriggerTokens = %d, want 70400", b.TriggerTokens)
	}
	if b.TargetTokens != 64_000 {
		t.Fatalf("TargetTokens = %d, want 64000", b.TargetTokens)
	}
}

func TestNewRequestBudgetFloorRoundsDown(t *testing.T) {
	t.Parallel()

	b, err := domain.NewRequestBudget(4096, domain.RequestBudgetPolicy{
		MaxRequestPercent: 60,
	})
	if err != nil {
		t.Fatal(err)
	}

	if b.HardTokens != 2457 {
		t.Fatalf("4096 * 60 / 100 = %d, want 2457", b.HardTokens)
	}
	if b.TriggerTokens != 2252 {
		t.Fatalf("4096 * 55 / 100 = %d, want 2252", b.TriggerTokens)
	}
	if b.TargetTokens != 2048 {
		t.Fatalf("4096 * 50 / 100 = %d, want 2048", b.TargetTokens)
	}
}

func TestNewRequestBudgetLargeWindowSafe(t *testing.T) {
	t.Parallel()

	b, err := domain.NewRequestBudget(5_000_000, domain.RequestBudgetPolicy{
		MaxRequestPercent: 80,
	})
	if err != nil {
		t.Fatal(err)
	}

	if b.HardTokens != 4_000_000 {
		t.Fatalf("5M * 80%% = %d, want 4000000", b.HardTokens)
	}
}

func TestNewRequestBudgetAtMaxSafeWindow(t *testing.T) {
	t.Parallel()

	b, err := domain.NewRequestBudget(domain.MaxSafeContextWindow, domain.RequestBudgetPolicy{
		MaxRequestPercent: 80,
	})
	if err != nil {
		t.Fatal(err)
	}

	if b.HardTokens != 8_000_000 {
		t.Fatalf("10M * 80%% = %d, want 8000000", b.HardTokens)
	}
}

func TestNewRequestBudgetRejectsWindowZero(t *testing.T) {
	t.Parallel()

	_, err := domain.NewRequestBudget(0, domain.RequestBudgetPolicy{
		MaxRequestPercent: 60,
	})
	if err == nil {
		t.Fatal("expected error for zero window")
	}
}

func TestNewRequestBudgetRejectsWindowOverMax(t *testing.T) {
	t.Parallel()

	_, err := domain.NewRequestBudget(domain.MaxSafeContextWindow+1, domain.RequestBudgetPolicy{
		MaxRequestPercent: 60,
	})
	if err == nil {
		t.Fatal("expected error for window exceeding max safe")
	}
}

func TestNewRequestBudgetRejectsPercentZero(t *testing.T) {
	t.Parallel()

	_, err := domain.NewRequestBudget(128_000, domain.RequestBudgetPolicy{
		MaxRequestPercent: 0,
	})
	if err == nil || !strings.Contains(err.Error(), "between 20 and 80") {
		t.Fatalf("expected percent validation error, got %v", err)
	}
}

func TestNewRequestBudgetRejectsPercentAbove80(t *testing.T) {
	t.Parallel()

	_, err := domain.NewRequestBudget(128_000, domain.RequestBudgetPolicy{
		MaxRequestPercent: 81,
	})
	if err == nil || !strings.Contains(err.Error(), "between 20 and 80") {
		t.Fatalf("expected percent validation error, got %v", err)
	}
}

func TestNewRequestBudgetRejectsPercent19(t *testing.T) {
	t.Parallel()

	_, err := domain.NewRequestBudget(128_000, domain.RequestBudgetPolicy{
		MaxRequestPercent: 19,
	})
	if err == nil || !strings.Contains(err.Error(), "between 20 and 80") {
		t.Fatalf("expected percent validation error for 19%%, got %v", err)
	}
}

func TestNewRequestBudgetAccepts20And80(t *testing.T) {
	t.Parallel()

	_, err := domain.NewRequestBudget(128_000, domain.RequestBudgetPolicy{
		MaxRequestPercent: 20,
	})
	if err != nil {
		t.Fatalf("20%% should be valid: %v", err)
	}

	_, err = domain.NewRequestBudget(128_000, domain.RequestBudgetPolicy{
		MaxRequestPercent: 80,
	})
	if err != nil {
		t.Fatalf("80%% should be valid: %v", err)
	}
}

func TestNewRequestBudgetGuaranteesTargetLeqTriggerLeqHard(t *testing.T) {
	t.Parallel()

	for _, pct := range []int{20, 30, 50, 60, 80} {
		b, err := domain.NewRequestBudget(128_000, domain.RequestBudgetPolicy{
			MaxRequestPercent: pct,
		})
		if err != nil {
			t.Fatalf("%d%%: %v", pct, err)
		}
		if b.TargetTokens > b.TriggerTokens {
			t.Fatalf("target(%d) > trigger(%d) for %d%%", b.TargetTokens, b.TriggerTokens, pct)
		}
		if b.TriggerTokens > b.HardTokens {
			t.Fatalf("trigger(%d) > hard(%d) for %d%%", b.TriggerTokens, b.HardTokens, pct)
		}
	}
}

func TestValidateRequestBudgetRejectsInvalidLimits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		budget domain.RequestBudget
		want   string
	}{
		{
			name:   "hard zero",
			budget: domain.RequestBudget{},
			want:   "hard tokens must be positive",
		},
		{
			name:   "trigger above hard",
			budget: domain.RequestBudget{HardTokens: 100, TriggerTokens: 101},
			want:   "trigger tokens",
		},
		{
			name:   "target above hard",
			budget: domain.RequestBudget{HardTokens: 100, TargetTokens: 101},
			want:   "target tokens",
		},
		{
			name:   "target above trigger",
			budget: domain.RequestBudget{HardTokens: 100, TriggerTokens: 50, TargetTokens: 51},
			want:   "target tokens 51 exceed trigger tokens 50",
		},
		{
			name:   "negative trigger",
			budget: domain.RequestBudget{HardTokens: 100, TriggerTokens: -1},
			want:   "must not be negative",
		},
		{
			name:   "negative target",
			budget: domain.RequestBudget{HardTokens: 100, TargetTokens: -1},
			want:   "must not be negative",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := domain.ValidateRequestBudget(tc.budget)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ValidateRequestBudget(%#v) = %v, want error containing %q", tc.budget, err, tc.want)
			}
		})
	}
}

func TestValidateRequestBudgetAcceptsOptionalZeroLimits(t *testing.T) {
	t.Parallel()

	for _, budget := range []domain.RequestBudget{
		{HardTokens: 100},
		{HardTokens: 100, TriggerTokens: 50},
		{HardTokens: 100, TargetTokens: 50},
	} {
		if err := domain.ValidateRequestBudget(budget); err != nil {
			t.Fatalf("ValidateRequestBudget(%#v) = %v, want nil", budget, err)
		}
	}
}

func TestDerivePercentagesStandard(t *testing.T) {
	t.Parallel()

	trigger, target := domain.DerivePercentages(60)
	if trigger != 55 {
		t.Fatalf("trigger = %d, want 55", trigger)
	}
	if target != 50 {
		t.Fatalf("target = %d, want 50", target)
	}
}

func TestDerivePercentagesClampsLowerBound(t *testing.T) {
	t.Parallel()

	trigger, target := domain.DerivePercentages(20)
	if trigger != 20 {
		t.Fatalf("trigger for 20%% = %d, want 20", trigger)
	}
	if target != 20 {
		t.Fatalf("target for 20%% = %d, want 20", target)
	}
}

func TestDerivePercentagesLowValues(t *testing.T) {
	t.Parallel()

	trigger, target := domain.DerivePercentages(25)
	if trigger != 20 {
		t.Fatalf("trigger for 25%% = %d, want 20", trigger)
	}
	if target != 20 {
		t.Fatalf("target for 25%% = %d, want 20", target)
	}
}

func TestDerivePercentagesHighValue(t *testing.T) {
	t.Parallel()

	trigger, target := domain.DerivePercentages(80)
	if trigger != 75 {
		t.Fatalf("trigger for 80%% = %d, want 75", trigger)
	}
	if target != 70 {
		t.Fatalf("target for 80%% = %d, want 70", target)
	}
}

func TestDerivePercentagesEnsuresTargetLeqTrigger(t *testing.T) {
	t.Parallel()

	for _, pct := range []int{20, 25, 30, 50, 60, 80} {
		trigger, target := domain.DerivePercentages(pct)
		if target > trigger {
			t.Fatalf("DerivePercentages(%d): target(%d) > trigger(%d)", pct, target, trigger)
		}
	}
}

func TestValidateContextWindowRejectsZero(t *testing.T) {
	t.Parallel()

	if err := domain.ValidateContextWindow(0); err == nil {
		t.Fatal("expected error for zero context window")
	}
}

func TestValidateContextWindowRejectsNegative(t *testing.T) {
	t.Parallel()

	if err := domain.ValidateContextWindow(-1); err == nil {
		t.Fatal("expected error for negative context window")
	}
}

func TestValidateContextWindowAcceptsValid(t *testing.T) {
	t.Parallel()

	if err := domain.ValidateContextWindow(128_000); err != nil {
		t.Fatalf("unexpected error for 128000: %v", err)
	}
}

func TestValidateContextWindowRejectsOverMax(t *testing.T) {
	t.Parallel()

	if err := domain.ValidateContextWindow(domain.MaxSafeContextWindow + 1); err == nil {
		t.Fatal("expected error for window exceeding max safe")
	}
}

func TestValidateContextWindowAcceptsMax(t *testing.T) {
	t.Parallel()

	if err := domain.ValidateContextWindow(domain.MaxSafeContextWindow); err != nil {
		t.Fatalf("max safe window should be valid: %v", err)
	}
}

func TestNewRequestBudgetIdempotentRepro(t *testing.T) {
	t.Parallel()

	policy := domain.RequestBudgetPolicy{
		MaxRequestPercent: 60,
	}
	first, err := domain.NewRequestBudget(128_000, policy)
	if err != nil {
		t.Fatal(err)
	}
	second, err := domain.NewRequestBudget(128_000, policy)
	if err != nil {
		t.Fatal(err)
	}

	if first != second {
		t.Fatalf("budget should be deterministic: %#v != %#v", first, second)
	}
}

func TestValidatePercentBoundaries(t *testing.T) {
	t.Parallel()

	for _, pct := range []int{-1, 0, 19, 81, 100} {
		if err := domain.ValidatePercent(pct); err == nil {
			t.Fatalf("ValidatePercent(%d) should fail", pct)
		}
	}
	for _, pct := range []int{20, 40, 60, 80} {
		if err := domain.ValidatePercent(pct); err != nil {
			t.Fatalf("ValidatePercent(%d) should pass: %v", pct, err)
		}
	}
}
