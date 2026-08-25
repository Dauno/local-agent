package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Dauno/slack-local-agent/internal/agentdef"
	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"google.golang.org/adk/v2/model"
)

// newModelForResolved is the provider-neutral model factory. It returns the
// constructed model and, for providers that require one, the resolved API key
// so the caller can register it for redaction. agent_cli providers require no
// API key.
func newModelForResolved(
	ctx context.Context,
	resolved *agentdef.ResolvedModel,
	values map[string]string,
	cfg config.Config,
	paths config.Paths,
	logger port.Logger,
	sanitize func(string) string,
	requestPercentOverride ...int,
) (model.LLM, string, error) {
	if resolved == nil {
		return nil, "", errors.New("resolved model is required")
	}
	if resolved.IsAgentCLI() {
		cliModel, err := buildAgentCLIModel(ctx, resolved, cfg, paths, logger, sanitize)
		if err != nil {
			return nil, "", err
		}
		return cliModel, "", nil
	}
	apiKey := values[resolved.APIKeyEnv]
	if strings.TrimSpace(apiKey) == "" {
		return nil, "", fmt.Errorf("%s is not configured. Run: local-agent init", resolved.APIKeyEnv)
	}
	httpModel, err := newModelFromResolved(resolved, apiKey)
	if err != nil {
		return nil, "", err
	}
	counter, err := composeRootTokenCounter(resolved)
	if err != nil {
		return nil, "", err
	}
	requestPercent := cfg.Context.ModelBudget.MaxRequestPercent
	if len(requestPercentOverride) > 0 && requestPercentOverride[0] > 0 {
		requestPercent = requestPercentOverride[0]
	}
	budget, err := domain.NewRequestBudget(resolved.ContextWindowTokens, domain.RequestBudgetPolicy{MaxRequestPercent: requestPercent})
	if err != nil {
		return nil, "", fmt.Errorf("compose model request budget: %w", err)
	}
	if err := httpModel.ConfigureRequestGuard(counter, budget, resolved.Provider.Name+"/"+resolved.Model); err != nil {
		return nil, "", err
	}
	if err := httpModel.ConfigureDefaultMaxOutputTokens(resolved.MaxOutputTokens); err != nil {
		return nil, "", err
	}
	return httpModel, apiKey, nil
}
