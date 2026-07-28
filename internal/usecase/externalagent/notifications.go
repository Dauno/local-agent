package externalagent

import (
	"context"
	"errors"
	"time"

	"github.com/Dauno/slack-local-agent/internal/port"
)

type NotificationConfig struct {
	PollInterval time.Duration
	LeaseTTL     time.Duration
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
			_ = w.store.MarkNotificationUnknown(context.WithoutCancel(ctx), notification, "notification_reconcile_failed")
			return nil
		}
		if found {
			return w.store.MarkNotificationPublished(context.WithoutCancel(ctx), notification, ts, time.Now().UTC())
		}
	}
	response, publishErr := w.publisher.Publish(ctx, *notification)
	if publishErr != nil || response.LastMessageTS == "" {
		_ = w.store.MarkNotificationUnknown(context.WithoutCancel(ctx), notification, "notification_publish_ambiguous")
		return nil
	}
	return w.store.MarkNotificationPublished(context.WithoutCancel(ctx), notification, response.LastMessageTS, time.Now().UTC())
}
