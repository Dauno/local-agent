package externalagent

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
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
	s.notification.NeedsReconciliation = s.notification.NeedsReconciliation || s.notification.PublishState == domain.NotificationPublishing || (s.notification.PublishState == domain.NotificationUnknown && s.notification.DeliveryMode == domain.JobResultDeliveryFile)
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
