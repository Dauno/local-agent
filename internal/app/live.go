package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	slackapi "github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/Dauno/slack-local-agent/internal/adapter/openaillm"
	"github.com/Dauno/slack-local-agent/internal/config"
)

type liveChecker struct{}

func (liveChecker) CheckSlackBot(ctx context.Context, botToken string) error {
	response, err := slackapi.New(botToken).AuthTestContext(ctx)
	if err != nil {
		return fmt.Errorf("Slack auth.test failed: %w", err)
	}
	if response == nil || response.UserID == "" {
		return errors.New("Slack auth.test returned no bot user ID")
	}
	return nil
}

func (liveChecker) CheckSlackApp(ctx context.Context, botToken, appToken string) error {
	api := slackapi.New(botToken, slackapi.OptionAppLevelToken(appToken))
	_, websocketURL, err := socketmode.New(api).OpenContext(ctx)
	if err != nil {
		return fmt.Errorf("Slack apps.connections.open failed: %w", err)
	}
	if strings.TrimSpace(websocketURL) == "" {
		return errors.New("Slack apps.connections.open returned no WebSocket URL")
	}
	return nil
}

func (liveChecker) CheckModel(ctx context.Context, cfg config.ModelConfig, apiKey string) error {
	llm, err := newModel(cfg, apiKey)
	if err != nil {
		return err
	}
	request := &model.LLMRequest{
		Contents: []*genai.Content{genai.NewContentFromText("Reply with OK.", genai.RoleUser)},
		Config:   &genai.GenerateContentConfig{MaxOutputTokens: 8},
	}
	for response, generateErr := range llm.GenerateContent(ctx, request, false) {
		if generateErr != nil {
			return generateErr
		}
		if response == nil || response.Content == nil {
			return errors.New("model endpoint returned no assistant content")
		}
		return nil
	}
	return errors.New("model endpoint returned no response")
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
