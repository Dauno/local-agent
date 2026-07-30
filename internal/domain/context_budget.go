package domain

import (
	"fmt"
)

const MaxSafeContextWindow = 10_000_000

var (
	ErrContextWindowNegative = fmt.Errorf("context window must be positive")
	ErrContextWindowTooLarge = fmt.Errorf("context window exceeds implementation-safe maximum of %d", MaxSafeContextWindow)
)

type ModelContextCapability struct {
	ProfileID           string
	ContextWindowTokens int
	MaxOutputTokens     int
	CounterStrategy     string
	CounterID           string
}

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
	wt := int64(windowTokens)
	pct := int64(policy.MaxRequestPercent)
	hard := wt * pct / 100
	triggerTokens := wt * int64(trigger) / 100
	targetTokens := wt * int64(target) / 100
	return RequestBudget{
		WindowTokens:  windowTokens,
		HardTokens:    int(hard),
		TriggerTokens: int(triggerTokens),
		TargetTokens:  int(targetTokens),
	}, nil
}

func DerivePercentages(maxRequestPercent int) (triggerPercent, targetPercent int) {
	trigger := maxRequestPercent - 5
	if trigger < 20 {
		trigger = 20
	}
	target := maxRequestPercent - 10
	if target < 20 {
		target = 20
	}
	return trigger, target
}
