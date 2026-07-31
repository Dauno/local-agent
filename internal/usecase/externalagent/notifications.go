package externalagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

type NotificationConfig struct {
	PollInterval         time.Duration
	LeaseTTL             time.Duration
	RetryBase            time.Duration
	RetryMax             time.Duration
	ReconcileMaxAttempts int
}

type NotificationDependencies struct {
	Store         port.ExternalAgentJobNotificationStore
	Publisher     port.JobNotificationPublisher
	HostCompleter port.ExternalAgentJobHostCompleter
}

type NotificationWorker struct {
	cfg       NotificationConfig
	store     port.ExternalAgentJobNotificationStore
	publisher port.JobNotificationPublisher
	completer port.ExternalAgentJobHostCompleter
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
	if cfg.ReconcileMaxAttempts <= 0 {
		cfg.ReconcileMaxAttempts = 5
	}
	return &NotificationWorker{cfg: cfg, store: deps.Store, publisher: deps.Publisher, completer: deps.HostCompleter, owner: "notification-worker"}, nil
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
		if notification.DeliveryMode == "file" && notification.SlackFileID == "" {
			// An ambiguous URL request did not leave a durable Slack file ID.
			// Reissuing it could create a duplicate file that cannot be
			// reconciled by identity, so fail closed instead.
			return w.recordFailure(ctx, notification, port.NewNotificationPublishError("result_file_upload_unknown", false, false, errors.New("Slack file identity is unavailable")))
		}
		ts, found, reconcileErr := w.publisher.Reconcile(ctx, *notification)
		if reconcileErr != nil {
			return w.recordFailure(ctx, notification, reconcileErr)
		}
		if found {
			return w.store.MarkNotificationPublished(context.WithoutCancel(ctx), notification, ts, time.Now().UTC())
		}
	}
	if err := w.verifyHostCompletion(ctx, notification); err != nil {
		return w.recordFailure(ctx, notification, err)
	}
	response, publishErr := w.publisher.Publish(ctx, *notification)
	if publishErr != nil || response.LastMessageTS == "" {
		if publishErr == nil {
			publishErr = port.NewNotificationPublishError("notification_publish_ambiguous", true, true, errors.New("publisher returned no Slack timestamp"))
		}
		return w.recordFailure(ctx, notification, publishErr)
	}
	return w.store.MarkNotificationPublished(context.WithoutCancel(ctx), notification, response.LastMessageTS, time.Now().UTC())
}

// verifyHostCompletion makes the materialized result enter the host-owned
// delivery path. It never invokes the model runtime and never returns result
// bytes to an ADK event.
func (w *NotificationWorker) verifyHostCompletion(ctx context.Context, notification *domain.ExternalAgentJobNotification) error {
	if w == nil || w.completer == nil || notification == nil || notification.PolicyVersion != domain.JobDeliveryPolicyV1 {
		return nil
	}
	if notification.Actor == "" || notification.ConversationKey == "" {
		return port.NewNotificationPublishError("result_destination_mismatch", false, false, errors.New("durable notification actor binding is unavailable"))
	}
	turn, err := w.completer.HostCompletionTurn(ctx, notification.JobID, notification.Actor, notification.ConversationKey)
	if err != nil {
		code := notificationErrorCode(err)
		if code == "notification_publish_ambiguous" {
			code = "result_artifact_invalid"
		}
		return port.NewNotificationPublishError(code, false, false, errors.New(code))
	}
	if turn.PendingConfirmation != nil || strings.TrimSpace(turn.Text) == "" {
		return port.NewNotificationPublishError("result_delivery_failed", false, false, errors.New("host completion did not return a result"))
	}
	digest := sha256.Sum256([]byte(turn.Text))
	if notification.ContentBytes != int64(len([]byte(turn.Text))) || !strings.EqualFold(notification.ContentSHA256, hex.EncodeToString(digest[:])) {
		return port.NewNotificationPublishError("result_delivery_failed", false, false, errors.New("host completion result identity does not match durable delivery"))
	}
	notification.HostResultText = turn.Text
	return nil
}

func (w *NotificationWorker) recordFailure(ctx context.Context, notification *domain.ExternalAgentJobNotification, failure error) error {
	if notification == nil {
		return nil
	}
	code := notificationErrorCode(failure)
	var classified *port.NotificationPublishError
	if !errors.As(failure, &classified) && !permanentNotificationCode(code) {
		classified = &port.NotificationPublishError{Code: code, Ambiguous: true, Retryable: true}
	}
	if classified != nil && classified.Retryable && w.canRetry(notification, classified) {
		if retryStore, ok := w.store.(port.ExternalAgentJobNotificationRetryStore); ok {
			now := time.Now().UTC()
			if err := retryStore.MarkNotificationRetry(context.WithoutCancel(ctx), notification, code, now.Add(w.retryDelay(notification.Attempts)), now); err == nil {
				return nil
			}
		}
	}
	if classified != nil && classified.Ambiguous && notification.Attempts >= w.cfg.ReconcileMaxAttempts && code == "notification_publish_ambiguous" {
		code = "notification_delivery_invalid"
	}
	_ = w.store.MarkNotificationUnknown(context.WithoutCancel(ctx), notification, code)
	return nil
}

func permanentNotificationCode(code string) bool {
	switch code {
	case "result_artifact_invalid", "result_delivery_failed", "result_destination_mismatch", "notification_delivery_invalid":
		return true
	default:
		return false
	}
}

func (w *NotificationWorker) canRetry(notification *domain.ExternalAgentJobNotification, failure *port.NotificationPublishError) bool {
	if failure == nil || !failure.Retryable {
		return false
	}
	if !failure.Ambiguous {
		return true
	}
	return notification != nil && notification.Attempts < w.cfg.ReconcileMaxAttempts
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
