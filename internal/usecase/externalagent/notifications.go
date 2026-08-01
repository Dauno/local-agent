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
	StuckThreshold       time.Duration
}

type NotificationDependencies struct {
	Store         port.ExternalAgentJobNotificationStore
	Publisher     port.JobNotificationPublisher
	HostCompleter port.ExternalAgentJobHostCompleter
	Logger        port.Logger
	Metrics       port.MetricRecorder
}

type NotificationWorker struct {
	cfg       NotificationConfig
	store     port.ExternalAgentJobNotificationStore
	publisher port.JobNotificationPublisher
	completer port.ExternalAgentJobHostCompleter
	logger    port.Logger
	metrics   port.MetricRecorder
	owner     string
}

const defaultNotificationStuckThreshold = 5 * time.Minute

type noopLogger struct{}

func (noopLogger) Debug(string, ...any) {}
func (noopLogger) Info(string, ...any)  {}
func (noopLogger) Warn(string, ...any)  {}
func (noopLogger) Error(string, ...any) {}

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
	if cfg.StuckThreshold == 0 {
		cfg.StuckThreshold = defaultNotificationStuckThreshold
	}
	if cfg.StuckThreshold < 0 {
		return nil, errors.New("notification stuck threshold must not be negative")
	}
	logger := deps.Logger
	if logger == nil {
		logger = noopLogger{}
	}
	metrics := deps.Metrics
	if metrics == nil {
		metrics = port.NoopMetricRecorder{}
	}
	return &NotificationWorker{cfg: cfg, store: deps.Store, publisher: deps.Publisher, completer: deps.HostCompleter,
		logger: logger, metrics: metrics, owner: "notification-worker"}, nil
}

func (w *NotificationWorker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.cfg.PollInterval)
	defer ticker.Stop()
	for {
		if err := w.ProcessOne(ctx); err != nil {
			w.logProcessingError(err)
		}
		if _, supported := w.store.(port.ExternalAgentJobNotificationHealthStore); supported {
			if _, err := w.SnapshotHealth(ctx, time.Now().UTC()); err != nil && !errors.Is(err, context.Canceled) {
				w.logger.Error("external-agent notification health snapshot failed", "error_code", notificationErrorCode(err))
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// ProcessOne durably classifies provider failures after a claim. Persistence
// errors remain visible to the caller instead of being converted into success.
func (w *NotificationWorker) ProcessOne(ctx context.Context) error {
	notification, err := w.store.ClaimNextNotification(ctx, time.Now().UTC(), w.owner, w.cfg.LeaseTTL)
	if err != nil {
		w.recordCASConflict(err, nil)
		return err
	}
	if notification == nil {
		return err
	}
	w.metrics.AddCounter(domain.MetricExternalAgentNotificationClaimTotal, 1, port.MetricLabels{
		"result_kind": boundedResultKind(notification.Kind),
	})
	if notification.NeedsReconciliation {
		w.metrics.AddCounter(domain.MetricExternalAgentNotificationReconcileTotal, 1, port.MetricLabels{
			"delivery_mode": boundedDeliveryMode(notification.DeliveryMode),
		})
		if notification.DeliveryMode == "file" && notification.SlackFileID == "" {
			// An ambiguous URL request did not leave a durable Slack file ID.
			// Reissuing it could create a duplicate file that cannot be
			// reconciled by identity, so fail closed instead.
			return wrapNotificationError(notification, w.recordFailure(ctx, notification, port.NewNotificationPublishError("result_file_upload_unknown", false, false, errors.New("Slack file identity is unavailable"))))
		}
		ts, found, reconcileErr := w.publisher.Reconcile(ctx, *notification)
		if reconcileErr != nil {
			return wrapNotificationError(notification, w.recordFailure(ctx, notification, reconcileErr))
		}
		if found {
			if err := w.store.MarkNotificationPublished(context.WithoutCancel(ctx), notification, ts, time.Now().UTC()); err != nil {
				w.recordCASConflict(err, notification)
				return wrapNotificationError(notification, err)
			}
			w.metrics.AddCounter(domain.MetricExternalAgentNotificationPublishTotal, 1, port.MetricLabels{
				"delivery_mode": boundedDeliveryMode(notification.DeliveryMode),
			})
			return nil
		}
	}
	if err := w.verifyHostCompletion(ctx, notification); err != nil {
		return wrapNotificationError(notification, w.recordFailure(ctx, notification, err))
	}
	response, publishErr := w.publisher.Publish(ctx, *notification)
	if publishErr != nil || response.LastMessageTS == "" {
		if publishErr == nil {
			publishErr = port.NewNotificationPublishError("notification_publish_ambiguous", true, true, errors.New("publisher returned no Slack timestamp"))
		}
		return wrapNotificationError(notification, w.recordFailure(ctx, notification, publishErr))
	}
	if err := w.store.MarkNotificationPublished(context.WithoutCancel(ctx), notification, response.LastMessageTS, time.Now().UTC()); err != nil {
		w.recordCASConflict(err, notification)
		return wrapNotificationError(notification, err)
	}
	w.metrics.AddCounter(domain.MetricExternalAgentNotificationPublishTotal, 1, port.MetricLabels{
		"delivery_mode": boundedDeliveryMode(notification.DeliveryMode),
	})
	return nil
}

// SnapshotHealth returns the current content-free outbox health and updates
// the bounded stuck gauge when the concrete store supports the optional view.
func (w *NotificationWorker) SnapshotHealth(ctx context.Context, now time.Time) (domain.ExternalAgentJobNotificationHealth, error) {
	store, ok := w.store.(port.ExternalAgentJobNotificationHealthStore)
	if !ok {
		return domain.ExternalAgentJobNotificationHealth{}, errors.New("notification health store is unavailable")
	}
	health, err := store.NotificationHealth(ctx, now, w.cfg.StuckThreshold)
	if err != nil {
		return domain.ExternalAgentJobNotificationHealth{}, err
	}
	w.metrics.SetGauge(domain.MetricExternalAgentNotificationStuck, int64(health.Stuck), nil)
	return health, nil
}

// verifyHostCompletion makes the materialized result enter the host-owned
// delivery path. It never invokes the model runtime and never returns result
// bytes to an ADK event.
func (w *NotificationWorker) verifyHostCompletion(ctx context.Context, notification *domain.ExternalAgentJobNotification) error {
	if notification == nil {
		return port.NewNotificationPublishError("notification_delivery_invalid", false, false, errors.New("durable notification is unavailable"))
	}
	if notification.PolicyVersion != domain.JobDeliveryPolicyV1 {
		return nil
	}
	if w == nil || w.completer == nil {
		return port.NewNotificationPublishError("result_delivery_failed", false, false, errors.New("durable host completion verifier is unavailable"))
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
	var retryPersistErr error
	if classified != nil && classified.Retryable && w.canRetry(notification, classified) {
		if retryStore, ok := w.store.(port.ExternalAgentJobNotificationRetryStore); ok {
			now := time.Now().UTC()
			if err := retryStore.MarkNotificationRetry(context.WithoutCancel(ctx), notification, code, now.Add(w.retryDelay(notification.Attempts)), now); err == nil {
				w.recordFailureMetric(notification, code)
				w.logPersistedFailure(notification, code)
				return nil
			} else {
				retryPersistErr = err
				w.recordCASConflict(err, notification)
			}
		}
	}
	if classified != nil && classified.Ambiguous && notification.Attempts >= w.cfg.ReconcileMaxAttempts && code == "notification_publish_ambiguous" {
		code = "notification_delivery_invalid"
	}
	if err := w.store.MarkNotificationUnknown(context.WithoutCancel(ctx), notification, code); err != nil {
		w.recordCASConflict(err, notification)
		if retryPersistErr != nil {
			return errors.Join(retryPersistErr, err)
		}
		return err
	}
	w.recordFailureMetric(notification, code)
	w.logPersistedFailure(notification, code)
	// A retry persistence error is surfaced even when the conservative fallback
	// to unknown succeeded, so Run cannot silently hide a durable write failure.
	return retryPersistErr
}

func (w *NotificationWorker) recordFailureMetric(notification *domain.ExternalAgentJobNotification, code string) {
	w.metrics.AddCounter(domain.MetricExternalAgentNotificationFailureTotal, 1, port.MetricLabels{
		"failure_category": notificationFailureCategory(code),
		"delivery_mode":    boundedDeliveryMode(notification.DeliveryMode),
	})
}

func (w *NotificationWorker) recordCASConflict(err error, notification *domain.ExternalAgentJobNotification) {
	if errors.Is(err, port.ErrNotificationStateConflict) || errors.Is(err, port.ErrNotificationClaimConflict) {
		labels := port.MetricLabels(nil)
		if notification != nil {
			labels = port.MetricLabels{"result_kind": boundedResultKind(notification.Kind)}
		}
		w.metrics.AddCounter(domain.MetricExternalAgentNotificationCASConflictTotal, 1, labels)
	}
}

func (w *NotificationWorker) logPersistedFailure(notification *domain.ExternalAgentJobNotification, code string) {
	if w == nil || notification == nil {
		return
	}
	w.logger.Warn("external-agent notification provider failure persisted",
		"error_code", safeNotificationErrorCode(code),
		"job_id", notification.JobID,
		"status_revision", notification.StatusRevision,
		"kind", boundedResultKind(notification.Kind),
		"attempts", notification.Attempts,
	)
}

func (w *NotificationWorker) logProcessingError(err error) {
	args := []any{"error_code", notificationErrorCode(err)}
	var processingErr *notificationProcessingError
	if errors.As(err, &processingErr) && processingErr.notification != nil {
		notification := processingErr.notification
		args = append(args,
			"job_id", notification.JobID,
			"status_revision", notification.StatusRevision,
			"kind", boundedResultKind(notification.Kind),
			"attempts", notification.Attempts,
		)
	}
	w.logger.Error("external-agent notification processing failed", args...)
}

type notificationProcessingError struct {
	notification *domain.ExternalAgentJobNotification
	err          error
}

func (e *notificationProcessingError) Error() string {
	if e == nil || e.err == nil {
		return "external-agent notification processing failed"
	}
	return e.err.Error()
}

func (e *notificationProcessingError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func wrapNotificationError(notification *domain.ExternalAgentJobNotification, err error) error {
	if err == nil {
		return nil
	}
	if notification == nil {
		return err
	}
	return &notificationProcessingError{notification: notification, err: err}
}

func permanentNotificationCode(code string) bool {
	switch code {
	case "result_artifact_invalid", "result_delivery_failed", "result_destination_mismatch", "notification_delivery_invalid", "result_file_upload_unknown":
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
	if errors.Is(err, port.ErrNotificationStateConflict) || errors.Is(err, port.ErrNotificationClaimConflict) {
		return "notification_state_conflict"
	}
	var classified *port.NotificationPublishError
	if errors.As(err, &classified) && classified.Code != "" {
		return safeNotificationErrorCode(classified.Code)
	}
	message := strings.ToLower(err.Error())
	for _, code := range []string{
		"result_artifact_invalid", "result_delivery_failed", "result_destination_mismatch",
		"notification_delivery_invalid", "result_file_upload_failed", "result_file_upload_unknown",
		"result_file_completion_failed",
	} {
		if strings.Contains(message, code) {
			return code
		}
	}
	if strings.Contains(message, "job notification") || strings.Contains(message, "delivery identity") {
		return "notification_delivery_invalid"
	}
	if strings.Contains(message, "mark notification") || strings.Contains(message, "persist notification") || strings.Contains(message, "notification failure update") {
		return "notification_state_persist_failed"
	}
	return "notification_publish_ambiguous"
}

func safeNotificationErrorCode(code string) string {
	switch code {
	case "result_artifact_invalid", "result_delivery_failed", "result_destination_mismatch",
		"notification_delivery_invalid", "notification_publish_ambiguous", "result_file_upload_failed",
		"result_file_upload_unknown", "result_file_completion_failed", "notification_state_conflict",
		"notification_state_persist_failed":
		return code
	default:
		return "notification_publish_ambiguous"
	}
}

func boundedResultKind(kind string) string {
	switch kind {
	case domain.JobNotificationTerminal, domain.JobNotificationFailure:
		return kind
	default:
		return "other"
	}
}

func boundedDeliveryMode(mode domain.JobResultDeliveryMode) string {
	switch mode {
	case domain.JobResultDeliveryMarkdown, domain.JobResultDeliveryFile:
		return string(mode)
	default:
		return "unknown"
	}
}

func notificationFailureCategory(code string) string {
	switch code {
	case "result_artifact_invalid", "result_delivery_failed", "result_destination_mismatch", "notification_delivery_invalid":
		return "validation"
	case "result_file_upload_failed", "result_file_upload_unknown", "result_file_completion_failed":
		return "file_upload"
	case "notification_publish_ambiguous":
		return "provider"
	case "notification_state_conflict", "notification_state_persist_failed":
		return "persistence"
	default:
		return "other"
	}
}
