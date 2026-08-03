package port

import (
	"context"
	"errors"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

var ErrNotificationStateConflict = errors.New("external-agent notification state conflict")
var ErrNotificationClaimConflict = ErrNotificationStateConflict
var ErrExternalAgentJobRevisionConflict = errors.New("external-agent job status revision conflict")
var ErrActivationStateConflict = errors.New("external-agent activation state conflict")
var ErrActivationClaimConflict = ErrActivationStateConflict

// ActivationProcessError classifies a host-completion failure without making
// provider details part of the durable activation error code.
type ActivationProcessError struct {
	Code      string
	Retryable bool
	Err       error
}

func (e *ActivationProcessError) Error() string {
	if e == nil {
		return ""
	}
	if e.Code != "" {
		return e.Code
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return "external-agent activation processing failed"
}

func (e *ActivationProcessError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func NewActivationProcessError(code string, retryable bool, err error) error {
	if err == nil {
		err = errors.New("external-agent activation processing failed")
	}
	return &ActivationProcessError{Code: code, Retryable: retryable, Err: err}
}

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

// ExternalAgentJobActivationClaimStore owns durable activation claims. Claims
// are ordered by conversation and compare-and-set against lease state.
type ExternalAgentJobActivationClaimStore interface {
	ClaimNextActivation(ctx context.Context, now time.Time, owner string, leaseTTL time.Duration) (*domain.ExternalAgentJobActivation, error)
}

type ExternalAgentJobActivationRetryStore interface {
	RetryActivation(ctx context.Context, activation *domain.ExternalAgentJobActivation, errorCode string, nextAttemptAt, now time.Time) error
}

type ExternalAgentJobActivationCompletionStore interface {
	MarkActivationModelStarted(ctx context.Context, activation *domain.ExternalAgentJobActivation, now time.Time) error
	PrepareActivationResponse(ctx context.Context, activation *domain.ExternalAgentJobActivation, responseBody, responseSHA256, exchangeIntentID, correlationID string, now time.Time) error
	CompleteActivation(ctx context.Context, activation *domain.ExternalAgentJobActivation, responseSlackTS string, now time.Time) error
	FailActivation(ctx context.Context, activation *domain.ExternalAgentJobActivation, errorCode string, now time.Time) error
	MarkActivationCompletionUnknown(ctx context.Context, activation *domain.ExternalAgentJobActivation, errorCode string, now time.Time) error
}

// ExternalAgentJobActivationStore owns the durable root-turn outbox. Every
// mutation is compare-and-set against the claimed activation owner and attempt;
// implementations must never derive identity from an incoming model message.
type ExternalAgentJobActivationStore interface {
	ExternalAgentJobActivationClaimStore
	ExternalAgentJobActivationRetryStore
	ExternalAgentJobActivationCompletionStore
	GetActivation(ctx context.Context, activationID string) (*domain.ExternalAgentJobActivation, error)
}

// ExternalAgentJobActivationExchangeStore atomically stages an assistant
// exchange and advances its activation to response_prepared. Implementations
// must derive the exchange identity from the activation, and must persist the
// exchange as memory-ineligible.
type ExternalAgentJobActivationExchangeStore interface {
	PrepareActivationResponseWithExchange(ctx context.Context, activation *domain.ExternalAgentJobActivation, metadata domain.ConversationMetadata, message domain.Message, retain int, now time.Time) (PreparedAssistantExchange, error)
}

// ExternalAgentJobActivationLeaseStore supports bounded work that must renew
// its claim without changing activation state.
type ExternalAgentJobActivationLeaseStore interface {
	RenewActivationLease(ctx context.Context, activation *domain.ExternalAgentJobActivation, now time.Time, leaseTTL time.Duration) error
}

// ExternalAgentJobActivationReconciler claims a specific durable activation
// only after revalidating its persisted actor/team/conversation binding.
type ExternalAgentJobActivationReconciler interface {
	ReconcileActivation(ctx context.Context, activationID, actor, teamID string, conversationKey domain.ConversationKey, now time.Time, owner string, leaseTTL time.Duration) (*domain.ExternalAgentJobActivation, error)
}

// ExternalAgentJobCompletionHandler is the use-case boundary for a root turn
// started by a published external-agent completion. It is intentionally
// separate from the ACP runtime and from Slack event handling.
type ExternalAgentJobCompletionHandler interface {
	HandleJobCompletion(ctx context.Context, activation domain.ExternalAgentJobActivation) error
	ReconcileJobCompletion(ctx context.Context, activation domain.ExternalAgentJobActivation) error
}

// ExternalAgentJobNotificationRetryStore persists a definitive, retryable
// publication failure without manufacturing Slack evidence.
type ExternalAgentJobNotificationRetryStore interface {
	MarkNotificationRetry(ctx context.Context, notification *domain.ExternalAgentJobNotification, errorCode string, nextAttemptAt, now time.Time) error
}

// ExternalAgentJobNotificationHealthStore exposes bounded, content-free
// outbox aggregates for health checks and worker gauges.
type ExternalAgentJobNotificationHealthStore interface {
	NotificationHealth(ctx context.Context, now time.Time, stuckThreshold time.Duration) (domain.ExternalAgentJobNotificationHealth, error)
}

// ExternalAgentJobActivationHealthStore exposes bounded, content-free health
// for the host-originated root-turn outbox.
type ExternalAgentJobActivationHealthStore interface {
	ActivationHealth(ctx context.Context, now time.Time, stuckThreshold time.Duration) (domain.ExternalAgentJobActivationHealth, error)
}

// ExternalAgentJobAdminStore exposes a read-only, redacted job inspection view.
type ExternalAgentJobAdminStore interface {
	InspectJob(ctx context.Context, jobID string) (*domain.ExternalAgentJobInspection, error)
}

// NotificationPublishError classifies a failed delivery boundary without
// carrying provider response bodies or result content.
type NotificationPublishError struct {
	Code      string
	Ambiguous bool
	Retryable bool
	Err       error
}

func (e *NotificationPublishError) Error() string {
	if e == nil {
		return ""
	}
	if e.Code != "" {
		return e.Code
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return "notification publication failed"
}

func (e *NotificationPublishError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func NewNotificationPublishError(code string, ambiguous, retryable bool, err error) error {
	if err == nil {
		err = errors.New("notification publication failed")
	}
	return &NotificationPublishError{Code: code, Ambiguous: ambiguous, Retryable: retryable, Err: err}
}

type ExternalAgentJobDeliveryStore interface {
	MarkNotificationFileID(ctx context.Context, notification *domain.ExternalAgentJobNotification, fileID string, now time.Time) error
	MarkNotificationUploadState(ctx context.Context, notification *domain.ExternalAgentJobNotification, state domain.JobResultUploadState, now time.Time) error
}

type ExternalAgentJobReconciler interface {
	BeginReconciliation(ctx context.Context, jobID, actor string, conversationKey domain.ConversationKey, now time.Time, owner string, leaseTTL time.Duration) (*domain.ExternalAgentJob, error)
}

// ExternalAgentJobExpectedReconciler is the optional revision-aware form used
// by operator and Slack entry points. Production stores implement the CAS in
// the same transaction as the reconciling transition.
type ExternalAgentJobExpectedReconciler interface {
	BeginReconciliationExpected(ctx context.Context, jobID, actor string, conversationKey domain.ConversationKey, expectedRevision int, now time.Time, owner string, leaseTTL time.Duration) (*domain.ExternalAgentJob, error)
}

type ExternalAgentJobShutdownStore interface {
	ShutdownStats(ctx context.Context) (domain.ExternalAgentJobShutdownStats, error)
}

// ExternalAgentJobReconciliationService is the shared confirmed operation for
// CLI and invocation-scoped Slack tools.
type ExternalAgentJobReconciliationService interface {
	ReconcileExpected(ctx context.Context, jobID, actor string, conversationKey domain.ConversationKey, expectedRevision int) (domain.AcpInvocationResult, error)
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

// ExternalAgentJobReader exposes only actor- and conversation-bound job
// inspection to host tools. Implementations must not trust model-supplied
// destination or actor values.
type ExternalAgentJobReader interface {
	Status(ctx context.Context, jobID, actor string, conversationKey domain.ConversationKey) (*domain.ExternalAgentJob, error)
	ReadResult(ctx context.Context, jobID, actor string, conversationKey domain.ConversationKey) (domain.ExternalAgentJobResult, error)
	ReadResultChunk(ctx context.Context, jobID, actor string, conversationKey domain.ConversationKey, offsetBytes, maxBytes int64) (domain.ResultChunk, error)
}

// ExternalAgentJobActivationReader reads a job only when its durable revision
// and terminal status still match the activation that requested the read.
type ExternalAgentJobActivationReader interface {
	StatusAtRevision(ctx context.Context, jobID, actor string, conversationKey domain.ConversationKey, expectedRevision int, expectedStatus domain.ExternalAgentJobStatus) (*domain.ExternalAgentJob, error)
	ReadResultChunkAtRevision(ctx context.Context, jobID, actor string, conversationKey domain.ConversationKey, expectedRevision int, expectedStatus domain.ExternalAgentJobStatus, offsetBytes, maxBytes int64) (domain.ResultChunk, error)
}

// ExternalAgentJobHostCompleter is the deterministic response phase after a
// detached job has completed. It must not create a confirmation or rerun ACP.
type ExternalAgentJobHostCompleter interface {
	HostCompletionTurn(ctx context.Context, jobID, actor string, conversationKey domain.ConversationKey) (AgentTurn, error)
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
