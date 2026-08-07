package domain

import (
	"fmt"
)

const MaxSafeContextWindow = 10_000_000

var (
	ErrContextWindowNegative = fmt.Errorf("context window must be positive")
	ErrContextWindowTooLarge = fmt.Errorf("context window exceeds implementation-safe maximum of %d", MaxSafeContextWindow)
)

type RequestBudgetPolicy struct {
	MaxRequestPercent int
}

type RequestBudget struct {
	WindowTokens  int
	HardTokens    int
	TriggerTokens int
	TargetTokens  int
}

func ValidateContextWindow(tokens int) error {
	if tokens <= 0 {
		return ErrContextWindowNegative
	}
	if tokens > MaxSafeContextWindow {
		return ErrContextWindowTooLarge
	}
	return nil
}

func ValidatePercent(pct int) error {
	if pct < 20 || pct > 80 {
		return fmt.Errorf("budget percent must be between 20 and 80, got %d", pct)
	}
	return nil
}

func NewRequestBudget(windowTokens int, policy RequestBudgetPolicy) (RequestBudget, error) {
	if err := ValidateContextWindow(windowTokens); err != nil {
		return RequestBudget{}, err
	}
	if err := ValidatePercent(policy.MaxRequestPercent); err != nil {
		return RequestBudget{}, err
	}
	trigger, target := DerivePercentages(policy.MaxRequestPercent)
	return RequestBudget{
		WindowTokens:  windowTokens,
		HardTokens:    windowTokens * policy.MaxRequestPercent / 100,
		TriggerTokens: windowTokens * trigger / 100,
		TargetTokens:  windowTokens * target / 100,
	}, nil
}

func DerivePercentages(maxRequestPercent int) (triggerPercent, targetPercent int) {
	return max(maxRequestPercent-5, 20), max(maxRequestPercent-10, 20)
}
