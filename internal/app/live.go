package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"

	slackapi "github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/Dauno/slack-local-agent/internal/adapter/openaillm"
	"github.com/Dauno/slack-local-agent/internal/agentdef"
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

func (liveChecker) CheckSlackContext(ctx context.Context, botToken string) error {
	api := slackapi.New(botToken)
	auth, err := api.AuthTestContext(ctx)
	if err != nil {
		return fmt.Errorf("Slack auth.test for context check failed: %w", err)
	}
	if auth == nil || auth.UserID == "" {
		return errors.New("Slack auth.test for context check returned no bot user ID")
	}
	if _, err := api.GetUserInfoContext(ctx, auth.UserID); err != nil {
		return fmt.Errorf("Slack users.info failed: %w", err)
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

func (liveChecker) CheckResolvedModel(ctx context.Context, resolved *agentdef.ResolvedModel, apiKey string) error {
	llm, err := newModelFromResolved(resolved, apiKey)
	if err != nil {
		return err
	}
	request := &model.LLMRequest{
		Contents: []*genai.Content{genai.NewContentFromText("Reply with OK.", genai.RoleUser)},
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

func newModelFromResolved(resolved *agentdef.ResolvedModel, apiKey string) (*openaillm.OpenAICompatibleLLM, error) {
	opts := []openaillm.Option{
		openaillm.WithAPIKey(apiKey),
		openaillm.WithBaseURL(resolved.BaseURL),
		openaillm.WithModel(resolved.Model),
	}
	if len(resolved.Headers) > 0 {
		opts = append(opts, openaillm.WithHeaders(resolved.Headers))
	}
	if resolved.ReasoningEffort != "" {
		opts = append(opts, openaillm.WithReasoningEffort(resolved.ReasoningEffort))
	}
	if len(resolved.ExtraBody) > 0 {
		opts = append(opts, openaillm.WithExtraBody(resolved.ExtraBody))
	}
	return openaillm.New(opts...)
}

// cliProviderChecker implements doctor.CLIProviderChecker for agent_cli
// providers through the same construction and handshake path used at startup.
type cliProviderChecker struct{}

func (cliProviderChecker) CheckProvider(ctx context.Context, resolved *agentdef.ResolvedModel, cfg config.Config, projectRoot string, describe bool) (string, error) {
	paths, err := cfg.ResolvePaths(projectRoot)
	if err != nil {
		return "", err
	}
	cliModel, err := buildAgentCLIModel(ctx, resolved, cfg, paths, nil, nil)
	if err != nil {
		return "", err
	}
	description, err := handshakeAgentCLI(ctx, cliModel, describe)
	if err != nil {
		return "", err
	}
	if describe {
		return fmt.Sprintf("shim %s (%s) maps CLI version %s; profile validated", description.Name, description.ShimVersion, description.CLIVersion), nil
	}
	return "profile validated", nil
}

func (cliProviderChecker) CheckAuthentication(ctx context.Context, _ *agentdef.ResolvedModel) (string, error) {
	// OpenCode is the only agent CLI supported by this release. The check
	// reports saved-login status without making a model call; output is never
	// echoed because it may reference account identifiers.
	executable, err := exec.LookPath("opencode")
	if err != nil {
		return "", fmt.Errorf("opencode executable not found: %w", err)
	}
	command := exec.CommandContext(ctx, executable, "auth", "list")
	// Authentication output can contain account identifiers. Discard it while
	// still draining both streams so memory use remains bounded.
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("opencode auth list cancelled: %w", ctxErr)
		}
		return "", fmt.Errorf("opencode auth list failed: %w", err)
	}
	return "opencode auth list succeeded; saved credentials are available", nil
}
