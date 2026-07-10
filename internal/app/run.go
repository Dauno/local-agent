package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	slackapi "github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"

	"github.com/Dauno/slack-local-agent/internal/adapter/adkagent"
	"github.com/Dauno/slack-local-agent/internal/adapter/envfile"
	"github.com/Dauno/slack-local-agent/internal/adapter/logging"
	slackadapter "github.com/Dauno/slack-local-agent/internal/adapter/slack"
	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/secure"
	"github.com/Dauno/slack-local-agent/internal/usecase/bootstrap"
	botusecase "github.com/Dauno/slack-local-agent/internal/usecase/bot"
)

func (a *Application) Run(ctx context.Context) error {
	configPath, err := config.ConfigPath(a.root)
	if err != nil {
		return err
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errors.New("Configuration not found. Run: local-agent init")
		}
		return fmt.Errorf("load runtime configuration: %w", err)
	}
	paths, err := cfg.ResolvePaths(a.root)
	if err != nil {
		return err
	}
	info, statErr := os.Stat(paths.StateDir)
	if errors.Is(statErr, os.ErrNotExist) {
		return errors.New("Local state not found. Run: local-agent init")
	}
	if statErr != nil {
		return fmt.Errorf("inspect configured state directory: %w. Run: local-agent doctor", statErr)
	}
	if !info.IsDir() {
		return errors.New("Configured state.dir is not a directory. Run: local-agent doctor")
	}

	values, err := envfile.NewResolver(paths.EnvFile).Resolve(
		cfg.Model.APIKeyEnv, bootstrap.SlackBotTokenEnv, bootstrap.SlackAppTokenEnv,
	)
	if err != nil {
		return fmt.Errorf("load runtime secrets: %w", err)
	}
	apiKey, botToken, appToken, err := requiredSecrets(cfg, values)
	if err != nil {
		return err
	}
	redactor := secure.NewRedactor(apiKey, botToken, appToken)
	logger := logging.New(a.logOutput, cfg.Runtime.LogLevel, redactor)

	store, err := adaptersqlite.OpenExisting(ctx, paths.DatabaseFile)
	if err != nil {
		if errors.Is(err, adaptersqlite.ErrDatabaseNotFound) {
			return errors.New("Local state not found. Run: local-agent init")
		}
		if errors.Is(err, adaptersqlite.ErrFutureSchema) {
			return redactor.Error(fmt.Errorf("%w. Install a local-agent version that supports this database or back up and remove only the configured database file", err))
		}
		return redactor.Error(fmt.Errorf("open runtime database: %w", err))
	}
	defer func() {
		if closeErr := store.Close(); closeErr != nil {
			logger.Error("database close failed", "error", closeErr)
		}
	}()
	if err := store.CleanupDedupe(ctx, time.Now().UTC()); err != nil {
		return redactor.Error(err)
	}

	llm, err := newModel(cfg.Model, apiKey)
	if err != nil {
		return redactor.Error(err)
	}
	agent, err := adkagent.New(cfg.Agent.Name, llm)
	if err != nil {
		return redactor.Error(err)
	}

	sdkLog := log.New(&redactingWriter{target: a.logOutput, redactor: redactor}, "slack: ", log.LstdFlags)
	api := slackapi.New(
		botToken,
		slackapi.OptionAppLevelToken(appToken),
		slackapi.OptionLog(sdkLog),
	)
	authCtx, cancelAuth := optionalTimeout(ctx, time.Duration(cfg.Runtime.SlackAPITimeoutSeconds)*time.Second)
	auth, err := api.AuthTestContext(authCtx)
	cancelAuth()
	if err != nil {
		return redactor.Error(fmt.Errorf("authenticate Slack bot: %w", err))
	}
	if auth == nil || auth.UserID == "" {
		return errors.New("authenticate Slack bot: Slack returned no bot user ID")
	}

	slackTimeout := time.Duration(cfg.Runtime.SlackAPITimeoutSeconds) * time.Second
	publisher := slackadapter.NewPublisher(api, slackTimeout, logger)
	history := slackadapter.NewHistoryReader(api, auth.UserID, slackTimeout, logger)
	service, err := botusecase.New(botusecase.Config{
		AccessPolicy: domain.AccessPolicy{
			AllowAllUsers: cfg.Slack.AllowAllUsers, AllowedUserIDs: cfg.Slack.AllowedUserIDs,
			AllowedTeamIDs: cfg.Slack.AllowedTeamIDs, AllowedChannelIDs: cfg.Slack.AllowedChannelIDs,
		},
		ContextLimits: domain.ContextLimits{
			MaxMessages: cfg.Context.MaxMessages, MaxChars: cfg.Context.MaxChars,
		},
		RetainMessages:      cfg.Context.RetainMessagesPerConversation,
		MaxConcurrentCalls:  cfg.Runtime.MaxConcurrentModelCalls,
		ModelTimeout:        time.Duration(cfg.Runtime.ModelTimeoutSeconds) * time.Second,
		BusyMessage:         cfg.Runtime.BusyMessage,
		ModelErrorMessage:   cfg.Runtime.ModelErrorMessage,
		UnauthorizedMessage: cfg.Slack.UnauthorizedMessage,
	}, botusecase.Dependencies{
		Store: store, Agent: agent, History: history, Publisher: publisher, Logger: logger,
		SanitizeContent: redactor.String,
	})
	if err != nil {
		return err
	}

	socket := socketmode.New(api, socketmode.OptionLog(sdkLog))
	listener := slackadapter.NewListener(socket, slackadapter.NewRouter(auth.UserID), logger)
	logger.Info("local-agent starting",
		"agent", cfg.Agent.Name,
		"model", cfg.Model.Name,
		"model_base_url", cfg.Model.BaseURL,
		"database", paths.DatabaseFile,
		"allowed_users", len(cfg.Slack.AllowedUserIDs),
		"allow_all_users", cfg.Slack.AllowAllUsers,
		"max_concurrent_model_calls", cfg.Runtime.MaxConcurrentModelCalls,
	)
	err = listener.Run(ctx, func(eventCtx context.Context, invocation domain.Invocation) {
		if _, handleErr := service.Handle(eventCtx, invocation); handleErr != nil {
			logger.Error("Slack invocation processing failed", "event_id", invocation.EventID, "error", handleErr)
		}
	})
	if err != nil {
		return redactor.Error(err)
	}
	logger.Info("local-agent stopped")
	return nil
}

func requiredSecrets(cfg config.Config, values map[string]string) (apiKey, botToken, appToken string, err error) {
	apiKey = values[cfg.Model.APIKeyEnv]
	botToken = values[bootstrap.SlackBotTokenEnv]
	appToken = values[bootstrap.SlackAppTokenEnv]
	if strings.TrimSpace(apiKey) == "" {
		return "", "", "", fmt.Errorf("%s is not configured. Run: local-agent init", cfg.Model.APIKeyEnv)
	}
	if strings.TrimSpace(botToken) == "" {
		return "", "", "", errors.New("SLACK_BOT_TOKEN is not configured. Run: local-agent init")
	}
	if !startsWithValue(botToken, "xoxb-") {
		return "", "", "", errors.New("SLACK_BOT_TOKEN must start with xoxb-. Run: local-agent doctor")
	}
	if strings.TrimSpace(appToken) == "" {
		return "", "", "", errors.New("SLACK_APP_TOKEN is not configured. Run: local-agent init")
	}
	if !startsWithValue(appToken, "xapp-") {
		return "", "", "", errors.New("SLACK_APP_TOKEN must start with xapp-. Run: local-agent doctor")
	}
	return apiKey, botToken, appToken, nil
}

func startsWithValue(value, prefix string) bool {
	return len(value) > len(prefix) && value[:len(prefix)] == prefix
}

func optionalTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}

type redactingWriter struct {
	target   io.Writer
	redactor secure.Redactor
}

func (w *redactingWriter) Write(data []byte) (int, error) {
	if w == nil || w.target == nil {
		return len(data), nil
	}
	if _, err := io.WriteString(w.target, w.redactor.String(string(data))); err != nil {
		return 0, err
	}
	return len(data), nil
}
