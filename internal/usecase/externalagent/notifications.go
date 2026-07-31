package externalagent

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/Dauno/slack-local-agent/internal/port"
)

type NotificationConfig struct {
	PollInterval time.Duration
	LeaseTTL     time.Duration
	RetryBase    time.Duration
	RetryMax     time.Duration
}

type NotificationDependencies struct {
	Store     port.ExternalAgentJobNotificationStore
	Publisher port.JobNotificationPublisher
}

type NotificationWorker struct {
	cfg       NotificationConfig
	store     port.ExternalAgentJobNotificationStore
	publisher port.JobNotificationPublisher
	owner     string
}

func NewNotificationWorker(cfg NotificationConfig, deps NotificationDependencies) (*NotificationWorker, error) {
	if cfg.PollInterval <= 0 || cfg.LeaseTTL <= 0 || deps.Store == nil || deps.Publisher == nil {
		return nil, errors.New("notification worker settings and dependencies are required")
	}
	if cfg.RetryBase <= 0 {
		cfg.RetryBase = time.Second
	}
	if cfg.RetryMax <= 0 {
		cfg.RetryMax = time.Minute
	}
	if cfg.RetryMax < cfg.RetryBase {
		return nil, errors.New("notification retry maximum must not be below its base delay")
	}
	return &NotificationWorker{cfg: cfg, store: deps.Store, publisher: deps.Publisher, owner: "notification-worker"}, nil
}

func (w *NotificationWorker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.cfg.PollInterval)
	defer ticker.Stop()
	for {
		_ = w.ProcessOne(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// ProcessOne deliberately returns no provider error after a claim. The durable
// row is marked unknown and the next worker restart/retry performs reconciliation.
func (w *NotificationWorker) ProcessOne(ctx context.Context) error {
	notification, err := w.store.ClaimNextNotification(ctx, time.Now().UTC(), w.owner, w.cfg.LeaseTTL)
	if err != nil || notification == nil {
		return err
	}
	if notification.NeedsReconciliation {
		ts, found, reconcileErr := w.publisher.Reconcile(ctx, *notification)
		if reconcileErr != nil {
			_ = w.store.MarkNotificationUnknown(context.WithoutCancel(ctx), notification, notificationErrorCode(reconcileErr))
			return nil
		}
		if found {
			return w.store.MarkNotificationPublished(context.WithoutCancel(ctx), notification, ts, time.Now().UTC())
		}
	}
	response, publishErr := w.publisher.Publish(ctx, *notification)
	if publishErr != nil || response.LastMessageTS == "" {
		code := "notification_publish_ambiguous"
		if publishErr != nil {
			code = notificationErrorCode(publishErr)
		}
		var classified *port.NotificationPublishError
		if errors.As(publishErr, &classified) && !classified.Ambiguous && classified.Retryable {
			if retryStore, ok := w.store.(port.ExternalAgentJobNotificationRetryStore); ok {
				next := time.Now().UTC().Add(w.retryDelay(notification.Attempts))
				if err := retryStore.MarkNotificationRetry(context.WithoutCancel(ctx), notification, code, next, time.Now().UTC()); err == nil {
					return nil
				}
			}
		}
		_ = w.store.MarkNotificationUnknown(context.WithoutCancel(ctx), notification, code)
		return nil
	}
	return w.store.MarkNotificationPublished(context.WithoutCancel(ctx), notification, response.LastMessageTS, time.Now().UTC())
}

func (w *NotificationWorker) retryDelay(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	shift := attempts - 1
	if shift > 20 {
		shift = 20
	}
	delay := float64(w.cfg.RetryBase) * math.Pow(2, float64(shift))
	if delay >= float64(w.cfg.RetryMax) {
		return w.cfg.RetryMax
	}
	return time.Duration(delay)
}

func notificationErrorCode(err error) string {
	if err == nil {
		return "notification_publish_ambiguous"
	}
	var classified *port.NotificationPublishError
	if errors.As(err, &classified) && classified.Code != "" {
		return classified.Code
	}
	message := strings.ToLower(err.Error())
	for _, code := range []string{"result_artifact_invalid", "result_delivery_failed", "result_destination_mismatch", "notification_delivery_invalid"} {
		if strings.Contains(message, code) {
			return code
		}
	}
	if strings.Contains(message, "job notification") || strings.Contains(message, "delivery identity") {
		return "notification_delivery_invalid"
	}
	return "notification_publish_ambiguous"
}
