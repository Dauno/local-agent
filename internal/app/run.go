package app

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/Dauno/slack-local-agent/internal/secure"
)

func (a *Application) Run(ctx context.Context) error {
	setup, err := a.loadRuntimeSetup()
	if err != nil {
		return err
	}
	models, err := a.prepareRuntimeModels(ctx, setup)
	if err != nil {
		return err
	}
	infra, err := a.openRuntimeInfrastructure(ctx, setup, models)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := infra.store.Close(); closeErr != nil {
			models.logger.Error("database close failed", "error", closeErr)
		}
	}()
	composition, err := a.composeRuntime(ctx, setup, models, infra)
	if err != nil {
		return err
	}
	return a.startSlackRuntime(ctx, setup, models, infra, composition)
}

func requiredSlackTokens(botToken, appToken string) error {
	if strings.TrimSpace(botToken) == "" {
		return errors.New("SLACK_BOT_TOKEN is not configured. Run: local-agent init")
	}
	if !startsWithValue(botToken, "xoxb-") {
		return errors.New("SLACK_BOT_TOKEN must start with xoxb-. Run: local-agent doctor")
	}
	if strings.TrimSpace(appToken) == "" {
		return errors.New("SLACK_APP_TOKEN is not configured. Run: local-agent init")
	}
	if !startsWithValue(appToken, "xapp-") {
		return errors.New("SLACK_APP_TOKEN must start with xapp-. Run: local-agent doctor")
	}
	return nil
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
		return 0, errors.New("redactingWriter: target writer is nil")
	}
	redacted := w.redactor.String(string(data))
	n, err := io.WriteString(w.target, redacted)
	if err != nil {
		return 0, err
	}
	if n < len(redacted) {
		return 0, io.ErrShortWrite
	}
	return len(data), nil
}
