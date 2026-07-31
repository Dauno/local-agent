package externalagent

import (
	"context"
	"errors"
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

type fakeNotificationStore struct {
	notification domain.ExternalAgentJobNotification
	claimed      bool
	publishedTS  string
	retriedCode  string
	retriedAt    time.Time
}

func (s *fakeNotificationStore) ClaimNextNotification(context.Context, time.Time, string, time.Duration) (*domain.ExternalAgentJobNotification, error) {
	if s.claimed || s.notification.PublishState == domain.NotificationPublished {
		return nil, nil
	}
	s.claimed = true
	s.notification.NeedsReconciliation = true
	return &s.notification, nil
}
func (s *fakeNotificationStore) MarkNotificationPublished(_ context.Context, n *domain.ExternalAgentJobNotification, ts string, _ time.Time) error {
	if n == nil || n.JobID != s.notification.JobID {
		return errors.New("wrong notification")
	}
	s.publishedTS = ts
	s.notification.PublishState = domain.NotificationPublished
	return nil
}
func (s *fakeNotificationStore) MarkNotificationUnknown(context.Context, *domain.ExternalAgentJobNotification, string) error {
	return nil
}
func (s *fakeNotificationStore) MarkNotificationRetry(_ context.Context, _ *domain.ExternalAgentJobNotification, code string, next, _ time.Time) error {
	s.retriedCode = code
	s.retriedAt = next
	return nil
}

type fakeNotificationPublisher struct {
	recoveredTS     string
	reconcileCalled bool
	publishCalled   bool
	publishErr      error
}

func (p *fakeNotificationPublisher) Publish(context.Context, domain.ExternalAgentJobNotification) (port.PublishedResponse, error) {
	p.publishCalled = true
	if p.publishErr != nil {
		return port.PublishedResponse{}, p.publishErr
	}
	return port.PublishedResponse{}, nil
}
func (p *fakeNotificationPublisher) Reconcile(context.Context, domain.ExternalAgentJobNotification) (string, bool, error) {
	p.reconcileCalled = true
	return p.recoveredTS, p.recoveredTS != "", nil
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
