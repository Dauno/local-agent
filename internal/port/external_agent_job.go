package port

import (
	"context"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

// ExternalAgentJobStore is the durable source of truth for external-agent
// execution. Implementations must bind lease operations to owner and attempt.
type ExternalAgentJobStore interface {
	CreateIfAbsent(ctx context.Context, job domain.ExternalAgentJob) (created bool, existing *domain.ExternalAgentJob, err error)
	GetJob(ctx context.Context, jobID string) (*domain.ExternalAgentJob, error)
	ClaimNext(ctx context.Context, now time.Time, owner string, leaseTTL time.Duration) (*domain.ExternalAgentJob, error)
	RenewLease(ctx context.Context, jobID, owner string, attempt int, now time.Time, leaseTTL time.Duration) error
	AssignACPSession(ctx context.Context, jobID, owner string, attempt int, sessionID string) error
	MarkSideEffectsPossible(ctx context.Context, jobID, owner string, attempt int) error
	RequestCancellation(ctx context.Context, jobID, actor string) (*domain.ExternalAgentJob, error)
	Transition(ctx context.Context, jobID, owner string, attempt int, next domain.ExternalAgentJobStatus, result *domain.AcpInvocationResult, errorCode string, now time.Time) error
	ListExpiredRunning(ctx context.Context, now time.Time) ([]domain.ExternalAgentJob, error)
}

type ExternalAgentJobNotificationStore interface {
	ClaimNextNotification(ctx context.Context, now time.Time, owner string, leaseTTL time.Duration) (*domain.ExternalAgentJobNotification, error)
	MarkNotificationPublished(ctx context.Context, notification *domain.ExternalAgentJobNotification, slackTS string, now time.Time) error
	MarkNotificationUnknown(ctx context.Context, notification *domain.ExternalAgentJobNotification, errorCode string) error
}

type ExternalAgentJobReconciler interface {
	BeginReconciliation(ctx context.Context, jobID, actor string, conversationKey domain.ConversationKey, now time.Time, owner string, leaseTTL time.Duration) (*domain.ExternalAgentJob, error)
}

type JobNotificationPublisher interface {
	Publish(ctx context.Context, notification domain.ExternalAgentJobNotification) (PublishedResponse, error)
	Reconcile(ctx context.Context, notification domain.ExternalAgentJobNotification) (string, bool, error)
}

type ExternalAgentSessionRecoveryRuntime interface {
	Reconcile(ctx context.Context, job domain.ExternalAgentJob) (domain.AcpInvocationResult, error)
}

// ExpiredExternalAgentJobRecovery is implemented by durable stores that can
// recover an expired lease without trusting its expired owner.
type ExpiredExternalAgentJobRecovery interface {
	RecoverExpired(ctx context.Context, jobID string, attempt, statusRevision int, now time.Time, next domain.ExternalAgentJobStatus, errorCode string) error
}

type ExternalAgentJobStarter interface {
	Start(ctx context.Context, request domain.ExternalAgentJobRequest) (*domain.ExternalAgentJob, error)
}

// ExternalAgentJobRuntime executes one already-admitted job. It is deliberately
// provider-neutral so use cases do not import ACP or process types.
type ExternalAgentJobRuntime interface {
	Run(ctx context.Context, job domain.ExternalAgentJob) (domain.AcpInvocationResult, error)
}

// ExternalAgentJobPublisher delivers host-owned terminal status. A nil
// publisher is valid for foreground callers and durable inspection-only mode.
type ExternalAgentJobPublisher interface {
	PublishJobTerminal(ctx context.Context, job domain.ExternalAgentJob) error
}
