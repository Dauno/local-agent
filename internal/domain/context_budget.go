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

func ValidateRequestBudget(budget RequestBudget) error {
	if budget.HardTokens <= 0 {
		return fmt.Errorf("request budget: hard tokens must be positive, got %d", budget.HardTokens)
	}
	if budget.TriggerTokens < 0 {
		return fmt.Errorf("request budget: trigger tokens must not be negative, got %d", budget.TriggerTokens)
	}
	if budget.TargetTokens < 0 {
		return fmt.Errorf("request budget: target tokens must not be negative, got %d", budget.TargetTokens)
	}
	if budget.TriggerTokens > 0 && budget.TriggerTokens > budget.HardTokens {
		return fmt.Errorf("request budget: trigger tokens %d exceed hard tokens %d", budget.TriggerTokens, budget.HardTokens)
	}
	if budget.TargetTokens > 0 && budget.TargetTokens > budget.HardTokens {
		return fmt.Errorf("request budget: target tokens %d exceed hard tokens %d", budget.TargetTokens, budget.HardTokens)
	}
	if budget.TargetTokens > 0 && budget.TriggerTokens > 0 && budget.TargetTokens > budget.TriggerTokens {
		return fmt.Errorf("request budget: target tokens %d exceed trigger tokens %d", budget.TargetTokens, budget.TriggerTokens)
	}
	return nil
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
