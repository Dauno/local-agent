package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/Dauno/slack-local-agent/internal/adapter/memorycurator"
	"github.com/Dauno/slack-local-agent/internal/adapter/openaillm"
	"github.com/Dauno/slack-local-agent/internal/agentdef"
	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/usecase/bootstrap"
)

// memoryCuratorLLM adapts ADK model.LLM to memorycurator.LLM.
type memoryCuratorLLM struct {
	llm                   model.LLM
	generateContentConfig *agentdef.GenerateContentConfig
	logger                port.Logger
	sanitize              func(string) string
}

var _ memorycurator.LLM = (*memoryCuratorLLM)(nil)

var errCuratorResponseIncomplete = errors.New("curator model response incomplete")

func (m *memoryCuratorLLM) GenerateText(ctx context.Context, prompt string) (string, error) {
	request := &model.LLMRequest{
		Contents: []*genai.Content{
			genai.NewContentFromText(prompt, genai.RoleUser),
		},
	}
	if m.generateContentConfig != nil {
		request.Config = buildGenaiConfig(m.generateContentConfig)
	}
	var response string
	var finishReason genai.FinishReason
	for resp, err := range m.llm.GenerateContent(ctx, request, false) {
		if err != nil {
			return "", err
		}
		if resp != nil && resp.Content != nil {
			finishReason = resp.FinishReason
			for _, part := range resp.Content.Parts {
				if part != nil && part.Text != "" {
					response += part.Text
				}
			}
		}
	}
	if m.logger != nil {
		loggedResponse := response
		if m.sanitize != nil {
			loggedResponse = m.sanitize(loggedResponse)
		}
		m.logger.Debug("memory curator model response", "finish_reason", finishReason, "response_chars", len([]rune(response)), "response", loggedResponse)
	}
	if finishReason != "" && finishReason != genai.FinishReasonStop {
		return "", fmt.Errorf("%w: finish_reason=%s response_chars=%d", errCuratorResponseIncomplete, finishReason, len([]rune(response)))
	}
	return response, nil
}

func buildGenaiConfig(cfg *agentdef.GenerateContentConfig) *genai.GenerateContentConfig {
	if cfg == nil {
		return nil
	}
	c := &genai.GenerateContentConfig{}
	if cfg.Temperature != nil {
		temp := float32(*cfg.Temperature)
		c.Temperature = &temp
	}
	if cfg.MaxOutputTokens > 0 {
		tokens := int32(cfg.MaxOutputTokens)
		c.MaxOutputTokens = tokens
	}
	if cfg.TopP != nil {
		topP := float32(*cfg.TopP)
		c.TopP = &topP
	}
	if cfg.TopK != nil {
		topK := float32(*cfg.TopK)
		c.TopK = &topK
	}
	if len(cfg.StopSequences) > 0 {
		c.StopSequences = cfg.StopSequences
	}
	return c
}

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
	return httpModel, apiKey, nil
}

func newModel(cfg config.ModelConfig, apiKey string) (*openaillm.OpenAICompatibleLLM, error) {
	return openaillm.New(
		openaillm.WithAPIKey(apiKey),
		openaillm.WithBaseURL(cfg.BaseURL),
		openaillm.WithHeaders(cfg.Headers),
		openaillm.WithModel(cfg.Name),
		openaillm.WithReasoningEffort(cfg.ReasoningEffort),
		openaillm.WithExtraBody(cfg.ExtraBody),
	)
}

func requiredSecrets(cfg config.Config, values map[string]string) (apiKey, botToken, appToken string, err error) {
	apiKey = values[cfg.Model.APIKeyEnv]
	botToken = values[bootstrap.SlackBotTokenEnv]
	appToken = values[bootstrap.SlackAppTokenEnv]
	if strings.TrimSpace(apiKey) == "" {
		return "", "", "", fmt.Errorf("%s is not configured. Run: local-agent init", cfg.Model.APIKeyEnv)
	}
	if err := requiredSlackTokens(botToken, appToken); err != nil {
		return "", "", "", err
	}
	return apiKey, botToken, appToken, nil
}
