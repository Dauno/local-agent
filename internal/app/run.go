package app

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/Dauno/slack-local-agent/internal/secure"
)

const shutdownStatsTimeout = time.Second

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
	runtimeCtx, runtimeCancel := context.WithCancel(context.WithoutCancel(ctx))
	defer runtimeCancel()
	composition, err := a.composeRuntime(runtimeCtx, setup, models, infra)
	if err != nil {
		return err
	}
	intakeCtx, stopIntake := context.WithCancel(context.Background())
	defer stopIntake()
	slackDone := make(chan error, 1)
	go func() {
		slackDone <- a.startSlackRuntime(intakeCtx, runtimeCtx, setup, models, infra, composition)
	}()
	interrupted := false
	var runErr error
	select {
	case runErr = <-slackDone:
	case <-ctx.Done():
		interrupted = true
	}
	if composition != nil {
		composition.StopExternalAdmission()
		stopIntake()
		beforeStatsCtx, cancelBeforeStats := context.WithTimeout(context.Background(), shutdownStatsTimeout)
		beforeStats, beforeStatsErr := composition.ExternalShutdownStats(beforeStatsCtx)
		cancelBeforeStats()
		grace := time.Duration(setup.cfg.Runtime.ShutdownGraceSeconds) * time.Second
		waitDone := make(chan error, 1)
		drainCtx, cancelDrain := context.WithTimeout(context.Background(), grace)
		go func() {
			waitDone <- waitExternalAndNotification(drainCtx, composition)
		}()
		select {
		case waitErr := <-waitDone:
			if waitErr != nil {
				models.logger.Warn("external-agent shutdown drain failed", "error", waitErr)
			}
		case <-drainCtx.Done():
			models.logger.Warn("external-agent shutdown grace expired")
		case <-a.forceShutdown:
			models.logger.Warn("external-agent shutdown drain bypassed")
		}
		cancelDrain()
		runtimeCancel()
		waitCtx, cancelWait := context.WithTimeout(context.Background(), 5*time.Second)
		if waitErr := waitExternalAndNotification(waitCtx, composition); waitErr != nil {
			models.logger.Warn("external-agent shutdown did not settle", "error", waitErr)
		}
		cancelWait()
		afterStatsCtx, cancelAfterStats := context.WithTimeout(context.Background(), shutdownStatsTimeout)
		afterStats, afterStatsErr := composition.ExternalShutdownStats(afterStatsCtx)
		cancelAfterStats()
		if beforeStatsErr == nil && afterStatsErr == nil {
			drained := beforeStats.Running - afterStats.Running
			if drained < 0 {
				drained = 0
			}
			ambiguous := afterStats.CompletionUnknown - beforeStats.CompletionUnknown
			if ambiguous < 0 {
				ambiguous = 0
			}
			models.logger.Info("external-agent shutdown", "queued", afterStats.Queued, "running", afterStats.Running, "drained", drained, "ambiguous", ambiguous)
		}
		healthCtx, cancelHealth := context.WithTimeout(context.Background(), shutdownStatsTimeout)
		activationHealth, healthErr := composition.ActivationHealth(healthCtx)
		cancelHealth()
		if healthErr == nil {
			models.logger.Info("external-agent activation shutdown", "pending", activationHealth.Pending, "processing", activationHealth.Processing, "model_started", activationHealth.ModelStarted, "response_prepared", activationHealth.ResponsePrepared, "processed", activationHealth.Processed, "completion_unknown", activationHealth.CompletionUnknown, "stuck", activationHealth.Stuck)
		}
	}
	if composition == nil {
		stopIntake()
		runtimeCancel()
	}
	if interrupted {
		select {
		case runErr = <-slackDone:
		case <-time.After(5 * time.Second):
			models.logger.Warn("Slack shutdown did not settle")
		}
	}
	return runErr
}

func waitExternalAndNotification(ctx context.Context, composition *runtimeComposition) error {
	if composition == nil {
		return nil
	}
	return waitInParallel(ctx, composition.WaitExternal, composition.WaitNotification, composition.WaitKnowledge, composition.WaitEmbedding)
}

func waitInParallel(ctx context.Context, waiters ...func(context.Context) error) error {
	if len(waiters) == 0 {
		return nil
	}
	done := make(chan error, len(waiters))
	for _, waiter := range waiters {
		if waiter == nil {
			done <- nil
			continue
		}
		go func(waiter func(context.Context) error) {
			done <- waiter(ctx)
		}(waiter)
	}
	var waitErrs []error
	for range waiters {
		select {
		case err := <-done:
			if err != nil {
				waitErrs = append(waitErrs, err)
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return errors.Join(waitErrs...)
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
