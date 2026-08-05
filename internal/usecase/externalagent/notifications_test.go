package externalagent

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

func TestNotificationWorkerReconcilesAmbiguousDeliveryWithoutLoggingContent(t *testing.T) {
	store := &fakeNotificationStore{notification: domain.ExternalAgentJobNotification{
		JobID: "job-1", StatusRevision: 4, Kind: domain.JobNotificationTerminal,
		CanonicalMarkdown: "secret path must not be logged", ContentSHA256: "digest",
		RendererVersion: "markdown_v1", PublishState: domain.NotificationPublishing,
	}}
	publisher := &fakeNotificationPublisher{recoveredTS: "1710000000.000001"}
	worker, err := NewNotificationWorker(NotificationConfig{PollInterval: time.Millisecond, LeaseTTL: time.Second}, NotificationDependencies{Store: store, Publisher: publisher})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.ProcessOne(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !publisher.reconcileCalled || publisher.publishCalled || store.publishedTS != "1710000000.000001" {
		t.Fatalf("reconcile=%v publish=%v ts=%q", publisher.reconcileCalled, publisher.publishCalled, store.publishedTS)
	}
}

func TestNotificationWorkerBacksOffAmbiguousReconciliation(t *testing.T) {
	store := &fakeNotificationStore{notification: domain.ExternalAgentJobNotification{
		JobID: "job-1", StatusRevision: 4, Kind: domain.JobNotificationTerminal,
		CanonicalMarkdown: "safe", ContentSHA256: "digest", RendererVersion: domain.JobNotificationRenderer,
		PublishState: domain.NotificationPublishing,
	}}
	publisher := &fakeNotificationPublisher{reconcileErr: port.NewNotificationPublishError("notification_publish_ambiguous", true, true, errors.New("transient history failure"))}
	worker, err := NewNotificationWorker(NotificationConfig{PollInterval: time.Millisecond, LeaseTTL: time.Second, RetryBase: time.Second, RetryMax: time.Minute}, NotificationDependencies{Store: store, Publisher: publisher})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.ProcessOne(t.Context()); err != nil {
		t.Fatal(err)
	}
	if store.retriedCode != "notification_publish_ambiguous" || store.retriedAt.IsZero() || publisher.publishCalled {
		t.Fatalf("reconcile backoff code=%q next=%v publish=%v", store.retriedCode, store.retriedAt, publisher.publishCalled)
	}
}

type fakeNotificationStore struct {
	notification     domain.ExternalAgentJobNotification
	claimed          bool
	publishedTS      string
	retriedCode      string
	retriedAt        time.Time
	claimErr         error
	markPublishedErr error
	markUnknownErr   error
	markRetryErr     error
	unknownCode      string
}

func (s *fakeNotificationStore) ClaimNextNotification(context.Context, time.Time, string, time.Duration) (*domain.ExternalAgentJobNotification, error) {
	if s.claimErr != nil {
		return nil, s.claimErr
	}
	if s.claimed || s.notification.PublishState == domain.NotificationPublished {
		return nil, nil
	}
	s.claimed = true
	s.notification.NeedsReconciliation = s.notification.NeedsReconciliation || s.notification.PublishState == domain.NotificationPublishing || (s.notification.PublishState == domain.NotificationUnknown && s.notification.DeliveryMode == domain.JobResultDeliveryFile)
	return &s.notification, nil
}
func (s *fakeNotificationStore) MarkNotificationPublished(_ context.Context, n *domain.ExternalAgentJobNotification, ts string, _ time.Time) error {
	if s.markPublishedErr != nil {
		return s.markPublishedErr
	}
	if n == nil || n.JobID != s.notification.JobID {
		return errors.New("wrong notification")
	}
	s.publishedTS = ts
	s.notification.PublishState = domain.NotificationPublished
	return nil
}
func (s *fakeNotificationStore) MarkNotificationUnknown(_ context.Context, _ *domain.ExternalAgentJobNotification, code string) error {
	if s.markUnknownErr != nil {
		return s.markUnknownErr
	}
	s.unknownCode = code
	s.notification.PublishState = domain.NotificationUnknown
	s.notification.LastErrorCode = code
	return nil
}
func (s *fakeNotificationStore) MarkNotificationRetry(_ context.Context, _ *domain.ExternalAgentJobNotification, code string, next, _ time.Time) error {
	if s.markRetryErr != nil {
		return s.markRetryErr
	}
	s.retriedCode = code
	s.retriedAt = next
	return nil
}

type fakeNotificationPublisher struct {
	recoveredTS     string
	reconcileCalled bool
	reconcileErr    error
	publishCalled   bool
	publishResponse port.PublishedResponse
	publishErr      error
}

func (p *fakeNotificationPublisher) Publish(context.Context, domain.ExternalAgentJobNotification) (port.PublishedResponse, error) {
	p.publishCalled = true
	if p.publishErr != nil {
		return port.PublishedResponse{}, p.publishErr
	}
	return p.publishResponse, nil
}
func (p *fakeNotificationPublisher) Reconcile(context.Context, domain.ExternalAgentJobNotification) (string, bool, error) {
	p.reconcileCalled = true
	if p.reconcileErr != nil {
		return "", false, p.reconcileErr
	}
	return p.recoveredTS, p.recoveredTS != "", nil
}

type fakeHostCompleter struct {
	calls int
	turn  port.AgentTurn
}

func (c *fakeHostCompleter) HostCompletionTurn(context.Context, string, string, domain.ConversationKey) (port.AgentTurn, error) {
	c.calls++
	return c.turn, nil
}

func contentSHA256ForTest(value string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(value)))
}

func TestNotificationWorkerRetriesDefinitiveFailureWithPersistedBackoff(t *testing.T) {
	store := &fakeNotificationStore{notification: domain.ExternalAgentJobNotification{
		JobID: "job-1", StatusRevision: 4, Kind: domain.JobNotificationTerminal,
		CanonicalMarkdown: "safe", ContentSHA256: "digest", RendererVersion: domain.JobNotificationRenderer,
		PublishState: domain.NotificationPending,
	}}
	publisher := &fakeNotificationPublisher{publishErr: &port.NotificationPublishError{Code: "result_file_upload_failed", Retryable: true, Err: errors.New("definitive")}}
	worker, err := NewNotificationWorker(NotificationConfig{PollInterval: time.Millisecond, LeaseTTL: time.Second, RetryBase: time.Second, RetryMax: time.Minute}, NotificationDependencies{Store: store, Publisher: publisher})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.ProcessOne(t.Context()); err != nil {
		t.Fatal(err)
	}
	if store.retriedCode != "result_file_upload_failed" || store.retriedAt.IsZero() || !publisher.publishCalled {
		t.Fatalf("retry code=%q next=%v published=%v", store.retriedCode, store.retriedAt, publisher.publishCalled)
	}
}

func TestNotificationWorkerDoesNotReissueAmbiguousFileRequestWithoutID(t *testing.T) {
	store := &fakeNotificationStore{notification: domain.ExternalAgentJobNotification{
		JobID: "job-1", StatusRevision: 4, Kind: domain.JobNotificationTerminal,
		CanonicalMarkdown: "OpenCode job `job-1` completed.", ContentSHA256: "digest",
		RendererVersion: domain.JobNotificationRenderer, DeliveryMode: domain.JobResultDeliveryFile,
		PublishState: domain.NotificationPublishState("unknown"),
	}}
	publisher := &fakeNotificationPublisher{}
	worker, err := NewNotificationWorker(NotificationConfig{PollInterval: time.Millisecond, LeaseTTL: time.Second}, NotificationDependencies{Store: store, Publisher: publisher})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.ProcessOne(t.Context()); err != nil {
		t.Fatal(err)
	}
	if publisher.publishCalled || publisher.reconcileCalled {
		t.Fatalf("ambiguous file was reissued: publish=%v reconcile=%v", publisher.publishCalled, publisher.reconcileCalled)
	}
}

func TestNotificationWorkerUsesHostCompletionBeforePublishingMaterializedResult(t *testing.T) {
	content := "complete host-owned result"
	store := &fakeNotificationStore{notification: domain.ExternalAgentJobNotification{
		JobID: "job-1", StatusRevision: 4, Kind: domain.JobNotificationTerminal,
		Actor: "U12345678", ConversationKey: "slack:T12345678:dm:D12345678",
		CanonicalMarkdown: "OpenCode job `job-1` completed.\n\n" + content,
		ContentSHA256:     contentSHA256ForTest(content), ContentBytes: int64(len(content)),
		NotificationSHA256: contentSHA256ForTest("OpenCode job `job-1` completed.\n\n" + content), NotificationBytes: int64(len([]byte("OpenCode job `job-1` completed.\n\n" + content))),
		ResultSHA256: contentSHA256ForTest(content), ResultBytes: int64(len(content)),
		RendererVersion: domain.JobNotificationRenderer, PublishState: domain.NotificationPending,
		DeliveryMode: domain.JobResultDeliveryMarkdown, PolicyVersion: domain.JobDeliveryPolicyV1,
		MaxMarkdownParts: 1,
	}}
	publisher := &fakeNotificationPublisher{publishResponse: port.PublishedResponse{LastMessageTS: "1710000000.000001"}}
	completer := &fakeHostCompleter{turn: port.AgentTurn{Text: content}}
	worker, err := NewNotificationWorker(NotificationConfig{PollInterval: time.Millisecond, LeaseTTL: time.Second}, NotificationDependencies{Store: store, Publisher: publisher, HostCompleter: completer})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.ProcessOne(t.Context()); err != nil {
		t.Fatal(err)
	}
	if completer.calls != 1 || !publisher.publishCalled || store.publishedTS == "" {
		t.Fatalf("host completion calls=%d publish=%v timestamp=%q", completer.calls, publisher.publishCalled, store.publishedTS)
	}
}

func TestNotificationWorkerRejectsHostCompletionResultIdentityMismatch(t *testing.T) {
	content := "complete host-owned result"
	notification := domain.ExternalAgentJobNotification{
		JobID: "job-1", StatusRevision: 4, Kind: domain.JobNotificationTerminal,
		Actor: "U12345678", ConversationKey: "slack:T12345678:dm:D12345678",
		CanonicalMarkdown: "OpenCode job `job-1` completed.\n\n" + content,
		ResultSHA256:      strings.Repeat("a", 64), ResultBytes: int64(len(content)),
		RendererVersion: domain.JobNotificationRenderer, PublishState: domain.NotificationPending,
		DeliveryMode: domain.JobResultDeliveryMarkdown, PolicyVersion: domain.JobDeliveryPolicyV1,
		MaxMarkdownParts: 1,
	}
	store := &fakeNotificationStore{notification: notification}
	publisher := &fakeNotificationPublisher{publishResponse: port.PublishedResponse{LastMessageTS: "1710000000.000001"}}
	completer := &fakeHostCompleter{turn: port.AgentTurn{Text: content}}
	worker, err := NewNotificationWorker(NotificationConfig{PollInterval: time.Millisecond, LeaseTTL: time.Second}, NotificationDependencies{Store: store, Publisher: publisher, HostCompleter: completer})
	if err != nil {
		t.Fatal(err)
	}
	var classified *port.NotificationPublishError
	if err := worker.verifyHostCompletion(t.Context(), &notification); !errors.As(err, &classified) || classified.Code != "result_delivery_failed" {
		t.Fatalf("result identity mismatch error = %v", err)
	}
	if notification.HostResultText != "" {
		t.Fatal("host result text was accepted on identity mismatch")
	}
	if err := worker.ProcessOne(t.Context()); err != nil {
		t.Fatal(err)
	}
	if publisher.publishCalled || store.unknownCode != "result_delivery_failed" {
		t.Fatalf("mismatched result bypassed verifier: publish=%v unknown_code=%q", publisher.publishCalled, store.unknownCode)
	}
}

func TestNotificationWorkerRejectsDeliveryV1WithoutHostCompleter(t *testing.T) {
	content := "complete host-owned result"
	notification := domain.ExternalAgentJobNotification{
		JobID: "job-1", StatusRevision: 4, Kind: domain.JobNotificationTerminal,
		Actor: "U12345678", ConversationKey: "slack:T12345678:dm:D12345678",
		CanonicalMarkdown: "OpenCode job `job-1` completed.", ContentSHA256: contentSHA256ForTest(content), ContentBytes: int64(len(content)),
		RendererVersion: domain.JobNotificationRenderer, PublishState: domain.NotificationPending,
		DeliveryMode: domain.JobResultDeliveryMarkdown, PolicyVersion: domain.JobDeliveryPolicyV1,
	}
	store := &fakeNotificationStore{notification: notification}
	publisher := &fakeNotificationPublisher{publishResponse: port.PublishedResponse{LastMessageTS: "1710000000.000001"}}
	worker, err := NewNotificationWorker(NotificationConfig{PollInterval: time.Millisecond, LeaseTTL: time.Second}, NotificationDependencies{Store: store, Publisher: publisher})
	if err != nil {
		t.Fatal(err)
	}
	var classified *port.NotificationPublishError
	if err := worker.verifyHostCompletion(t.Context(), &notification); !errors.As(err, &classified) || classified.Code != "result_delivery_failed" {
		t.Fatalf("missing host completer error = %v", err)
	}
	if err := worker.ProcessOne(t.Context()); err != nil {
		t.Fatal(err)
	}
	if publisher.publishCalled || store.unknownCode != "result_delivery_failed" {
		t.Fatalf("delivery-v1 bypassed verifier: publish=%v unknown_code=%q", publisher.publishCalled, store.unknownCode)
	}
}

type recordingNotificationLogger struct {
	warnings []string
	errors   []string
}

func (l *recordingNotificationLogger) Debug(string, ...any) {}
func (l *recordingNotificationLogger) Info(string, ...any)  {}
func (l *recordingNotificationLogger) Warn(message string, args ...any) {
	l.warnings = append(l.warnings, fmt.Sprint(append([]any{message}, args...)...))
}
func (l *recordingNotificationLogger) Error(message string, args ...any) {
	l.errors = append(l.errors, fmt.Sprint(append([]any{message}, args...)...))
}

type recordingNotificationMetrics struct {
	samples []port.MetricSample
}

func (m *recordingNotificationMetrics) AddCounter(name string, delta int64, labels port.MetricLabels) {
	m.samples = append(m.samples, port.MetricSample{Name: name, Kind: port.MetricKindCounter, Value: float64(delta), Labels: port.CloneMetricLabels(labels)})
}
func (m *recordingNotificationMetrics) SetGauge(name string, value int64, labels port.MetricLabels) {
	m.samples = append(m.samples, port.MetricSample{Name: name, Kind: port.MetricKindGauge, Value: float64(value), Labels: port.CloneMetricLabels(labels)})
}
func (m *recordingNotificationMetrics) Observe(name string, value float64, labels port.MetricLabels) {
	m.samples = append(m.samples, port.MetricSample{Name: name, Kind: port.MetricKindObservation, Value: value, Labels: port.CloneMetricLabels(labels)})
}
func (m *recordingNotificationMetrics) Snapshot() []port.MetricSample {
	return append([]port.MetricSample(nil), m.samples...)
}

func TestNotificationWorkerRunLogsProcessErrorsAndKeepsCodesBounded(t *testing.T) {
	secret := "provider response body should not appear"
	logger := &recordingNotificationLogger{}
	store := &fakeNotificationStore{claimErr: errors.New(secret)}
	worker, err := NewNotificationWorker(NotificationConfig{PollInterval: time.Hour, LeaseTTL: time.Second}, NotificationDependencies{
		Store: store, Publisher: &fakeNotificationPublisher{}, Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	worker.Run(ctx)
	if len(logger.errors) != 1 {
		t.Fatalf("logged errors = %d, want 1", len(logger.errors))
	}
	if strings.Contains(logger.errors[0], secret) {
		t.Fatalf("raw error leaked to logger: %s", logger.errors[0])
	}
	if !strings.Contains(logger.errors[0], "notification_publish_ambiguous") {
		t.Fatalf("bounded error code missing: %s", logger.errors[0])
	}
}

func TestNotificationWorkerReturnsFailurePersistenceError(t *testing.T) {
	storeErr := errors.New("retry persistence failed")
	unknownErr := errors.New("unknown persistence failed")
	store := &fakeNotificationStore{notification: domain.ExternalAgentJobNotification{
		JobID: "job-1", StatusRevision: 4, Kind: domain.JobNotificationTerminal,
		PublishState: domain.NotificationPending,
	}, markRetryErr: storeErr, markUnknownErr: unknownErr}
	publisher := &fakeNotificationPublisher{publishErr: port.NewNotificationPublishError("notification_publish_ambiguous", true, true, errors.New("provider body"))}
	worker, err := NewNotificationWorker(NotificationConfig{PollInterval: time.Millisecond, LeaseTTL: time.Second}, NotificationDependencies{Store: store, Publisher: publisher})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.ProcessOne(t.Context()); err == nil || !errors.Is(err, storeErr) || !errors.Is(err, unknownErr) {
		t.Fatalf("ProcessOne error = %v, want both persistence errors", err)
	}
}

func TestNotificationWorkerPersistsProviderFailureLogsWarningAndRecordsMetric(t *testing.T) {
	secret := "provider response body must not be logged"
	logger := &recordingNotificationLogger{}
	metrics := &recordingNotificationMetrics{}
	store := &fakeNotificationStore{notification: domain.ExternalAgentJobNotification{
		JobID: "job-1", StatusRevision: 4, Kind: domain.JobNotificationTerminal,
		CanonicalMarkdown: "safe", ContentSHA256: "digest", RendererVersion: domain.JobNotificationRenderer,
		PublishState: domain.NotificationPending, DeliveryMode: domain.JobResultDeliveryMarkdown, PolicyVersion: "legacy_v1",
	}}
	publisher := &fakeNotificationPublisher{publishErr: port.NewNotificationPublishError("notification_publish_ambiguous", false, false, errors.New(secret))}
	worker, err := NewNotificationWorker(NotificationConfig{PollInterval: time.Millisecond, LeaseTTL: time.Second}, NotificationDependencies{
		Store: store, Publisher: publisher, Logger: logger, Metrics: metrics,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.ProcessOne(t.Context()); err != nil {
		t.Fatal(err)
	}
	if store.unknownCode != "notification_publish_ambiguous" || store.notification.PublishState != domain.NotificationUnknown || store.notification.LastErrorCode != "notification_publish_ambiguous" {
		t.Fatalf("provider failure durable state = %#v code=%q", store.notification, store.unknownCode)
	}
	if len(logger.warnings) != 1 || !strings.Contains(logger.warnings[0], "notification_publish_ambiguous") || strings.Contains(logger.warnings[0], secret) {
		t.Fatalf("provider failure warning = %#v", logger.warnings)
	}
	foundFailureMetric := false
	for _, sample := range metrics.samples {
		if sample.Name == domain.MetricExternalAgentNotificationFailureTotal && sample.Labels["failure_category"] == "provider" && sample.Labels["delivery_mode"] == "markdown" && sample.Value == 1 {
			foundFailureMetric = true
		}
	}
	if !foundFailureMetric {
		t.Fatalf("provider failure metrics = %#v", metrics.samples)
	}
}

func TestNotificationWorkerExposesClaimCASConflict(t *testing.T) {
	metrics := &recordingNotificationMetrics{}
	store := &fakeNotificationStore{claimErr: port.ErrNotificationClaimConflict}
	worker, err := NewNotificationWorker(NotificationConfig{PollInterval: time.Millisecond, LeaseTTL: time.Second}, NotificationDependencies{
		Store: store, Publisher: &fakeNotificationPublisher{}, Metrics: metrics,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.ProcessOne(t.Context()); !errors.Is(err, port.ErrNotificationClaimConflict) {
		t.Fatalf("claim conflict error = %v", err)
	}
	foundConflictMetric := false
	for _, sample := range metrics.samples {
		if sample.Name == domain.MetricExternalAgentNotificationCASConflictTotal && sample.Value == 1 {
			foundConflictMetric = true
		}
	}
	if !foundConflictMetric {
		t.Fatalf("claim conflict metrics = %#v", metrics.samples)
	}
}

func TestNotificationWorkerMetricsUseOnlyBoundedLabels(t *testing.T) {
	metrics := &recordingNotificationMetrics{}
	content := "safe result"
	store := &fakeNotificationStore{notification: domain.ExternalAgentJobNotification{
		JobID: "secret-job-id", StatusRevision: 4, Kind: domain.JobNotificationTerminal,
		Actor: "secret-actor", ConversationKey: "secret-conversation", CanonicalMarkdown: content,
		ContentSHA256: contentSHA256ForTest(content), ContentBytes: int64(len(content)),
		PublishState: domain.NotificationPending, DeliveryMode: domain.JobResultDeliveryMarkdown,
	}}
	publisher := &fakeNotificationPublisher{publishResponse: port.PublishedResponse{LastMessageTS: "1710000000.000001"}}
	worker, err := NewNotificationWorker(NotificationConfig{PollInterval: time.Millisecond, LeaseTTL: time.Second}, NotificationDependencies{Store: store, Publisher: publisher, Metrics: metrics})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.ProcessOne(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, sample := range metrics.samples {
		for key, value := range sample.Labels {
			if key != "result_kind" && key != "delivery_mode" && key != "failure_category" {
				t.Fatalf("unexpected notification metric label %q=%q in %#v", key, value, sample)
			}
			if strings.Contains(value, "secret-") {
				t.Fatalf("sensitive metric label %q=%q", key, value)
			}
		}
	}
}
