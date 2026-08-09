package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const MaxExternalAgentTaskRunes = 200_000

// MaxExternalAgentResultBytes is a final defensive bound for result reads made
// through the host-completion path. Configured artifact bounds remain stricter
// in normal composition.
const MaxExternalAgentResultBytes = 256 * 1024 * 1024

const (
	JobNotificationTerminal = "terminal"
	JobNotificationFailure  = "delivery_failure"
	JobNotificationRenderer = "markdown_v1"
	JobDeliveryPolicyV1     = "delivery_v1"
	SlackMarkdownChunkRunes = 11900
	maxMarkdownParts        = 8
)

type NotificationPublishState string

const (
	NotificationPending    NotificationPublishState = "pending"
	NotificationPublishing NotificationPublishState = "publishing"
	NotificationPublished  NotificationPublishState = "published"
	NotificationUnknown    NotificationPublishState = "unknown"
)

type JobResultDeliveryMode string

const (
	JobResultDeliveryMarkdown JobResultDeliveryMode = "markdown"
	JobResultDeliveryFile     JobResultDeliveryMode = "file"
)

type JobResultUploadState string

const (
	JobResultUploadNotApplicable JobResultUploadState = "not_applicable"
	JobResultUploadPending       JobResultUploadState = "pending"
	JobResultUploadURLRequested  JobResultUploadState = "url_requested"
	JobResultUploadBytesUploaded JobResultUploadState = "bytes_uploaded"
	JobResultUploadCompleted     JobResultUploadState = "completed"
	JobResultUploadUnknown       JobResultUploadState = "unknown"
)

type ResultDeliveryPolicy struct {
	MaxMarkdownParts       int
	MaxFileBytes           int64
	MaxInlineResultBytes   int64
	MaxResultArtifactBytes int64
}

func (p ResultDeliveryPolicy) Validate() error {
	if p.MaxMarkdownParts < 1 || p.MaxMarkdownParts > maxMarkdownParts {
		return fmt.Errorf("result delivery max Markdown parts must be between 1 and %d", maxMarkdownParts)
	}
	if p.MaxFileBytes <= 0 || p.MaxFileBytes > p.MaxResultArtifactBytes {
		return errors.New("result delivery max file bytes must be positive and within the artifact bound")
	}
	if p.MaxInlineResultBytes <= 0 || p.MaxInlineResultBytes > int64(p.MaxMarkdownParts*SlackMarkdownChunkRunes) {
		return errors.New("ACP inline result bound exceeds the configured Markdown delivery capacity")
	}
	return nil
}

type ExternalAgentJobNotification struct {
	JobID          string
	StatusRevision int
	Kind           string
	// TerminalStatus and PublishedAt are immutable snapshots used to create an
	// internal activation. Legacy rows intentionally leave both unset.
	TerminalStatus ExternalAgentJobStatus
	PublishedAt    time.Time
	// Actor and ConversationKey are loaded from the authoritative job row for
	// host-owned completion. They are not part of the immutable delivery key.
	Actor           string
	ConversationKey ConversationKey
	// HostResultText is ephemeral completion data. It is never written to the
	// notification row or exposed through an ADK function response.
	HostResultText string
	// RootActivationRequired is the explicit, immutable completion disposition.
	// It is set by mode at construction and backfilled for historical detached
	// terminal snapshots; MarkNotificationPublished never infers it.
	RootActivationRequired bool
	CanonicalMarkdown      string
	// NotificationSHA256 and NotificationBytes are the notification identity
	// over the canonical Markdown bytes (SHA-256 lowercase hex).
	NotificationSHA256 string
	NotificationBytes  int64
	// ResultSHA256 is the identity of the complete sanitized result, distinct
	// from the notification identity. Empty together with zero ResultBytes
	// means the terminal status carried no result (fail-closed).
	ResultSHA256        string
	RendererVersion     string
	Target              ReplyTarget
	PublishState        NotificationPublishState
	RecoveredSlackTS    string
	Attempts            int
	LeaseOwner          string
	LeaseExpiry         time.Time
	NextAttemptAt       time.Time
	LastErrorCode       string
	NeedsReconciliation bool
	DeliveryMode        JobResultDeliveryMode
	PolicyVersion       string
	ArtifactRef         string
	ResultBytes         int64
	ContentBytes        int64
	MaxMarkdownParts    int
	UploadState         JobResultUploadState
	SlackFileID         string
}

type ExternalAgentJobActivationState string

const (
	ActivationPending           ExternalAgentJobActivationState = "pending"
	ActivationProcessing        ExternalAgentJobActivationState = "processing"
	ActivationModelStarted      ExternalAgentJobActivationState = "model_started"
	ActivationResponsePrepared  ExternalAgentJobActivationState = "response_prepared"
	ActivationCompleted         ExternalAgentJobActivationState = "completed"
	ActivationCompletionUnknown ExternalAgentJobActivationState = "completion_unknown"
	ActivationFailed            ExternalAgentJobActivationState = "failed"

	// ActivationForegroundRetiredCode is the bounded error code stamped by the
	// v31 repair migration on foreground activations retired without model
	// execution or response publication. It is not a worker classification.
	ActivationForegroundRetiredCode = "foreground_activation_retired"
)

// ExternalAgentJobActivation is the durable host-originated root-turn outbox
// entry. Its identity and binding are copied from SQLite notification/job rows;
// callers must not replace them with values received from Slack or a model.
type ExternalAgentJobActivation struct {
	ActivationID       string
	JobID              string
	StatusRevision     int
	Kind               string
	TerminalStatus     ExternalAgentJobStatus
	NotificationSHA256 string
	Actor              string
	TeamID             string
	ConversationKey    ConversationKey
	OriginalCallID     string
	DeliveryMode       JobResultDeliveryMode
	ContentBytes       int64
	SlackMessageTS     string
	PublishedAt        time.Time
	State              ExternalAgentJobActivationState
	Attempt            int
	LeaseOwner         string
	LeaseExpiry        time.Time
	NextAttemptAt      time.Time
	LastErrorCode      string
	ResponseBody       string
	ResponseSHA256     string
	ExchangeIntentID   string
	CorrelationID      string
	ResponseSlackTS    string
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

func ExternalAgentJobActivationID(jobID string, statusRevision int, kind string) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%s", jobID, statusRevision, kind)))
	return "activation_" + hex.EncodeToString(digest[:])
}

func (a ExternalAgentJobActivation) Validate() error {
	if a.ActivationID == "" || a.JobID == "" || a.Kind == "" {
		return errors.New("external-agent activation identity is incomplete")
	}
	if a.StatusRevision < 0 || a.Attempt < 0 || a.ContentBytes < 0 {
		return errors.New("external-agent activation counters are invalid")
	}
	if !validActivationTerminalStatus(a.TerminalStatus) {
		return fmt.Errorf("invalid external-agent activation terminal status %q", a.TerminalStatus)
	}
	if !validSHA256Hex(a.NotificationSHA256) {
		return errors.New("external-agent activation notification digest is invalid")
	}
	if a.DeliveryMode != JobResultDeliveryMarkdown && a.DeliveryMode != JobResultDeliveryFile {
		return fmt.Errorf("invalid external-agent activation delivery mode %q", a.DeliveryMode)
	}
	if !validActivationState(a.State) {
		return fmt.Errorf("invalid external-agent activation state %q", a.State)
	}
	return nil
}

func (a *ExternalAgentJobActivation) Transition(next ExternalAgentJobActivationState) error {
	if a == nil {
		return errors.New("external-agent activation is nil")
	}
	if !validActivationTransition(a.State, next) {
		return fmt.Errorf("illegal external-agent activation transition %q -> %q", a.State, next)
	}
	a.State = next
	return nil
}

func validActivationState(state ExternalAgentJobActivationState) bool {
	switch state {
	case ActivationPending, ActivationProcessing, ActivationModelStarted,
		ActivationResponsePrepared, ActivationCompleted, ActivationCompletionUnknown,
		ActivationFailed:
		return true
	default:
		return false
	}
}

func validActivationTransition(from, to ExternalAgentJobActivationState) bool {
	if from == to {
		return from == ActivationResponsePrepared
	}
	switch from {
	case ActivationPending:
		return to == ActivationProcessing
	case ActivationProcessing:
		return to == ActivationPending || to == ActivationModelStarted || to == ActivationFailed
	case ActivationModelStarted:
		return to == ActivationResponsePrepared || to == ActivationCompletionUnknown
	case ActivationResponsePrepared:
		return to == ActivationCompleted || to == ActivationFailed
	default:
		return false
	}
}

func validActivationTerminalStatus(status ExternalAgentJobStatus) bool {
	switch status {
	case JobCompleted, JobFailed, JobCancelled, JobCompletionUnknown, JobAbandoned:
		return true
	default:
		return false
	}
}

// ExternalAgentJobNotificationHealth is a read-only aggregate of the durable
// notification outbox. It contains no job identity or delivery content.
type ExternalAgentJobNotificationHealth struct {
	Pending           int
	Publishing        int
	Unknown           int
	Published         int
	PermanentFailures int
	Stuck             int
}

// ExternalAgentJobActivationHealth is a content-free aggregate of the root
// activation outbox. Processed is the terminal completed/failed count; the
// ambiguous completion_unknown count stays separate for operator visibility.
type ExternalAgentJobActivationHealth struct {
	Pending           int
	Processing        int
	ModelStarted      int
	ResponsePrepared  int
	Processed         int
	Completed         int
	CompletionUnknown int
	Failed            int
	Stuck             int
}

// ExternalAgentJobIdentityHealth is a content-free aggregate of durable result
// identity completeness. Every field is a count; no job ID, actor,
// conversation, digest, reference, path, or result content value is ever
// exposed. Retired foreground activations are the bounded v31 repair evidence
// (terminal rows stamped with ActivationForegroundRetiredCode): they are
// expected after an upgrade and must never be treated as a defect.
type ExternalAgentJobIdentityHealth struct {
	// JobsCompletedWithoutResultIdentity counts completed jobs whose result
	// identity (SHA-256 + byte count) is not complete and that were not marked
	// as historical during the v32 upgrade.
	JobsCompletedWithoutResultIdentity int
	// JobsCompletedWithoutResultIdentityLegacy counts historical v32 rows whose
	// unavailable result identity is informational rather than a current defect.
	JobsCompletedWithoutResultIdentityLegacy int
	// NotificationsWithoutIdentity counts notification rows whose
	// notification identity (notification_sha256 + notification_bytes) is not
	// complete. Every post-v32 delivery must carry a complete identity.
	NotificationsWithoutIdentity int
	// ActivationsWithoutContent counts completed activations whose content byte
	// count is not positive. Failed, cancelled, completion_unknown, and
	// abandoned activations legitimately carry no result content.
	ActivationsWithoutContent int
	// ActivationsWithoutIdentity counts activations whose notification digest is
	// not lowercase hexadecimal SHA-256.
	ActivationsWithoutIdentity int
	// ForegroundActivationsActive counts non-terminal activations owned by
	// foreground jobs. This is the P0 contract violation: foreground
	// completions must never produce claimable root activations.
	ForegroundActivationsActive int
	// RetiredForegroundActivations counts terminal activations stamped with
	// the bounded foreground_activation_retired code. Informational only.
	RetiredForegroundActivations int
}

// ExternalAgentJobDeliveryInspection is the redacted administrative view of a
// single delivery. Artifact references, canonical content, actor identity and
// conversation keys are intentionally absent.
type ExternalAgentJobDeliveryInspection struct {
	StatusRevision     int                      `json:"status_revision"`
	NotificationKind   string                   `json:"notification_kind"`
	DeliveryMode       JobResultDeliveryMode    `json:"delivery_mode"`
	PublishState       NotificationPublishState `json:"publish_state"`
	Attempts           int                      `json:"attempts"`
	LeaseOwner         string                   `json:"lease_owner"`
	LeaseOwnerPresent  bool                     `json:"lease_owner_present"`
	LeaseExpiry        time.Time                `json:"lease_expiry"`
	LastErrorCode      string                   `json:"last_error_code"`
	NextAttemptAt      time.Time                `json:"next_attempt_at"`
	RecoveredSlackTS   string                   `json:"recovered_slack_ts"`
	UploadState        JobResultUploadState     `json:"upload_state"`
	SlackFileIDPresent bool                     `json:"slack_file_id_present"`
}

// ExternalAgentJobInspection is the safe local operator view of one job.
// The complete ACP session ID is present because the view is actor-free and
// locally authorized. Task, actor, conversation, paths, and result content
// remain excluded.
type ExternalAgentJobInspection struct {
	JobID          string                               `json:"job_id"`
	Status         ExternalAgentJobStatus               `json:"status"`
	StatusRevision int                                  `json:"status_revision"`
	ACPSessionID   string                               `json:"acp_session_id"`
	FinishedAt     time.Time                            `json:"finished_at"`
	Deliveries     []ExternalAgentJobDeliveryInspection `json:"deliveries"`
	// Live projection fields; empty until the projection row exists.
	Phase                    ACPProgressPhase  `json:"phase"`
	Health                   ACPProgressHealth `json:"health"`
	LastEventKind            ACPEventKind      `json:"last_event_kind"`
	LastTransportActivityAt  time.Time         `json:"last_transport_activity_at"`
	LastSessionUpdateAt      time.Time         `json:"last_session_update_at"`
	LastMeaningfulProgressAt time.Time         `json:"last_meaningful_progress_at"`
	PromptStartedAt          time.Time         `json:"prompt_started_at"`
	ActiveToolCount          int               `json:"active_tool_count"`
	PendingPermission        bool              `json:"pending_permission"`
	PromptElapsedSeconds     int64             `json:"prompt_elapsed_seconds"`
	StopReason               string            `json:"stop_reason"`
	// ProcessAlive is nil when the current process has no trustworthy runtime
	// handle (for example the read-only CLI); it must never be rendered as dead.
	ProcessAlive *bool `json:"process_alive"`
}

func NewExternalAgentJobNotification(job ExternalAgentJob) (ExternalAgentJobNotification, error) {
	target, err := ConversationReplyTarget(job.ConversationKey)
	if err != nil {
		return ExternalAgentJobNotification{}, err
	}
	if job.ID == "" || job.StatusRevision < 0 {
		return ExternalAgentJobNotification{}, errors.New("external-agent notification identity is invalid")
	}
	markdown := fmt.Sprintf("OpenCode job `%s` %s.", job.ID, job.Status)
	if job.Status == JobCompleted && strings.TrimSpace(job.ResultSummary) != "" {
		markdown += "\n\nSummary: " + job.ResultSummary
	}
	if job.Status == JobCompletionUnknown {
		markdown = fmt.Sprintf("OpenCode job `%s` was interrupted after external actions may have occurred. It was not retried; reconciliation is required.", job.ID)
	}
	if job.Status == JobFailed {
		markdown += " The operation failed with a host-owned error code."
		if job.ErrorCode != "" {
			markdown += " Delivery code: `" + sanitizeNotificationText(job.ErrorCode, 128) + "`."
		}
	}
	if job.Status == JobCancelled {
		markdown += " The operation was cancelled before completion."
	}
	if job.Status == JobAbandoned {
		markdown += " The operation was explicitly abandoned; external state was not asserted to be reverted."
	}
	markdown = sanitizeNotificationText(markdown, 8000)
	notificationSHA := NotificationIdentitySHA256(markdown)
	if notificationSHA == "" {
		return ExternalAgentJobNotification{}, errors.New("external-agent notification Markdown is invalid")
	}
	resultSHA, resultBytes := ValidResultIdentity(job.ResultSHA256, job.ResultBytes)
	target.CorrelationID = fmt.Sprintf("job:%s:%d:%s", job.ID, job.StatusRevision, JobNotificationTerminal)
	return ExternalAgentJobNotification{
		JobID: job.ID, StatusRevision: job.StatusRevision, Kind: JobNotificationTerminal,
		TerminalStatus: job.Status,
		Actor:          job.Actor, ConversationKey: job.ConversationKey,
		CanonicalMarkdown:      markdown,
		RootActivationRequired: job.Mode == JobDetached,
		NotificationSHA256:     notificationSHA, NotificationBytes: int64(len([]byte(markdown))),
		ResultSHA256: resultSHA, ResultBytes: resultBytes,
		RendererVersion: JobNotificationRenderer, Target: target,
		PublishState: NotificationPending,
		DeliveryMode: JobResultDeliveryMarkdown, PolicyVersion: "legacy_v1",
		UploadState: JobResultUploadNotApplicable, MaxMarkdownParts: 1,
	}, nil
}

// NewExternalAgentJobDelivery creates a notification from a complete,
// already-sanitized result. It never truncates provider output.
func NewExternalAgentJobDelivery(job ExternalAgentJob, result AcpInvocationResult) (ExternalAgentJobNotification, error) {
	target, err := ConversationReplyTarget(job.ConversationKey)
	if err != nil {
		return ExternalAgentJobNotification{}, err
	}
	if job.ID == "" || job.StatusRevision < 0 {
		return ExternalAgentJobNotification{}, errors.New("external-agent delivery identity is invalid")
	}
	if strings.ContainsAny(job.ID, "/\\\x00\r\n") {
		return ExternalAgentJobNotification{}, errors.New("external-agent delivery job ID is not a safe filename component")
	}
	if job.Status != JobCompleted {
		return NewExternalAgentJobNotification(job)
	}
	mode := result.DeliveryMode
	if mode == "" {
		mode = JobResultDeliveryMarkdown
	}
	policyVersion := result.DeliveryPolicyVersion
	if policyVersion == "" {
		policyVersion = JobDeliveryPolicyV1
	}
	maxParts := result.DeliveryMaxMarkdownParts
	if maxParts <= 0 {
		maxParts = 1
	}
	if maxParts > maxMarkdownParts {
		return ExternalAgentJobNotification{}, errors.New("result delivery Markdown part policy is invalid")
	}
	contentDigest := strings.ToLower(result.DeliveryContentSHA256)
	if contentDigest == "" {
		contentDigest = strings.ToLower(result.ResultSHA256)
	}
	if contentDigest == "" {
		digest := sha256.Sum256([]byte(result.Text))
		contentDigest = fmt.Sprintf("%x", digest)
	}
	contentBytes := result.DeliveryContentBytes
	if contentBytes <= 0 {
		contentBytes = result.ResultBytes
	}
	if contentBytes <= 0 {
		contentBytes = int64(len([]byte(result.Text)))
	}
	if contentBytes <= 0 {
		return ExternalAgentJobNotification{}, errors.New("result delivery content size is invalid")
	}
	if !validSHA256Hex(contentDigest) {
		return ExternalAgentJobNotification{}, errors.New("result delivery digest is invalid")
	}
	if mode == JobResultDeliveryMarkdown && result.Text != "" {
		digest := sha256.Sum256([]byte(result.Text))
		if contentDigest != fmt.Sprintf("%x", digest) || contentBytes != int64(len([]byte(result.Text))) {
			return ExternalAgentJobNotification{}, errors.New("result delivery digest does not match complete Markdown content")
		}
	}
	markdown := fmt.Sprintf("OpenCode job `%s` completed.", job.ID)
	artifactRef := ""
	uploadState := JobResultUploadNotApplicable
	if mode == JobResultDeliveryFile {
		artifactRef = result.DeliveryArtifactRef
		if artifactRef == "" {
			artifactRef = result.ArtifactRef
		}
		if artifactRef == "" {
			return ExternalAgentJobNotification{}, errors.New("file delivery requires a result artifact")
		}
		if strings.ContainsAny(artifactRef, "/\\\x00\r\n") {
			return ExternalAgentJobNotification{}, errors.New("result delivery artifact reference is invalid")
		}
		uploadState = JobResultUploadPending
		markdown += fmt.Sprintf(" The complete result was attached as `opencode-%s.md` (%d bytes, SHA-256 `%s`).", job.ID, contentBytes, contentDigest)
	} else if mode == JobResultDeliveryMarkdown {
		if result.ArtifactRef != "" || result.DeliveryArtifactRef != "" {
			return ExternalAgentJobNotification{}, errors.New("Markdown delivery cannot reference an artifact")
		}
		if result.DeliveryCanonicalMarkdown != "" {
			markdown = result.DeliveryCanonicalMarkdown
		} else if result.Text != "" {
			markdown += "\n\n" + result.Text
		}
	} else {
		return ExternalAgentJobNotification{}, fmt.Errorf("unsupported result delivery mode %q", mode)
	}
	if !utf8.ValidString(markdown) || strings.TrimSpace(markdown) == "" {
		return ExternalAgentJobNotification{}, errors.New("result delivery Markdown is invalid")
	}
	notificationSHA := NotificationIdentitySHA256(markdown)
	if notificationSHA == "" {
		return ExternalAgentJobNotification{}, errors.New("result delivery Markdown is invalid")
	}
	target.CorrelationID = fmt.Sprintf("job:%s:%d:%s", job.ID, job.StatusRevision, JobNotificationTerminal)
	return ExternalAgentJobNotification{
		JobID: job.ID, StatusRevision: job.StatusRevision, Kind: JobNotificationTerminal,
		TerminalStatus: job.Status,
		Actor:          job.Actor, ConversationKey: job.ConversationKey,
		CanonicalMarkdown: markdown, RendererVersion: JobNotificationRenderer,
		RootActivationRequired: job.Mode == JobDetached,
		NotificationSHA256:     notificationSHA, NotificationBytes: int64(len([]byte(markdown))),
		ResultSHA256: contentDigest, ResultBytes: contentBytes,
		Target: target, PublishState: NotificationPending, DeliveryMode: mode, PolicyVersion: policyVersion,
		ArtifactRef: artifactRef, MaxMarkdownParts: maxParts,
		ContentBytes: contentBytes,
		UploadState:  uploadState,
	}, nil
}

// SanitizeResultText applies host-owned control neutralization before digesting
// or storing result bytes. Redaction is applied by the application redactor.
func SanitizeResultText(value string) (string, error) {
	if !utf8.ValidString(value) {
		return "", errors.New("result text is not valid UTF-8")
	}
	var builder strings.Builder
	for _, r := range value {
		if r == '\x00' || (r < ' ' && r != '\n' && r != '\r' && r != '\t') {
			continue
		}
		if r == '<' {
			builder.WriteString("&lt;")
			continue
		}
		builder.WriteRune(r)
	}
	result := builder.String()
	if strings.TrimSpace(result) == "" {
		return "", errors.New("result text is empty after sanitization")
	}
	return result, nil
}

// NotificationIdentitySHA256 returns the lowercase hex SHA-256 of the
// canonical notification Markdown, or empty when the Markdown is not a valid
// notification body (fail-closed).
func NotificationIdentitySHA256(markdown string) string {
	if !utf8.ValidString(markdown) || strings.TrimSpace(markdown) == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(markdown))
	return fmt.Sprintf("%x", digest)
}

func validSHA256Hex(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// ValidResultIdentity returns a lowercase 64-hex digest and positive byte
// count only when both form a complete result identity. Anything else fails
// closed to empty values; no digest is ever fabricated for missing data.
func ValidResultIdentity(sha256Hex string, bytes int64) (string, int64) {
	sha256Hex = strings.ToLower(sha256Hex)
	if !validSHA256Hex(sha256Hex) || bytes <= 0 {
		return "", 0
	}
	return sha256Hex, bytes
}

// ValidInlineResult reports whether a completed job's inline result shape is
// structurally coherent: a non-empty summary whose exact UTF-8 bytes match a
// positive byte count and a valid SHA-256 identity over those same bytes. This
// mirrors the inline branch of the verified result readers; any ambiguity
// fails closed to false and no identity is fabricated for missing data.
func ValidInlineResult(summary, sha256Hex string, bytes int64) bool {
	if summary == "" || !utf8.ValidString(summary) {
		return false
	}
	sha256Hex = strings.ToLower(sha256Hex)
	if !validSHA256Hex(sha256Hex) || bytes <= 0 {
		return false
	}
	if int64(len([]byte(summary))) != bytes {
		return false
	}
	digest := sha256.Sum256([]byte(summary))
	return fmt.Sprintf("%x", digest) == sha256Hex
}

// ValidArtifactResult reports whether a completed job's file-mode result shape
// is structurally coherent: a non-empty artifact reference that is a safe
// filename component with a positive byte count and a valid SHA-256 identity.
// Artifact bytes are verified by the artifact reader at read time; the status
// projection can only check the structural identity. Fail-closed on ambiguity.
func ValidArtifactResult(artifactRef, sha256Hex string, bytes int64) bool {
	if artifactRef == "" || strings.ContainsAny(artifactRef, "/\\\x00\r\n") {
		return false
	}
	sha256Hex = strings.ToLower(sha256Hex)
	return validSHA256Hex(sha256Hex) && bytes > 0
}

// CanonicalArtifactReference returns the exact artifact filename bound to a
// job. Result readers derive the owner from the job and the artifact adapter
// accepts only the canonical owner-derived name, so this is the single name a
// completed job may carry to be readable at all.
func CanonicalArtifactReference(jobID string) string {
	return jobID + "-delivery.result"
}

// ValidArtifactResultForJob reports whether a completed job's file-mode result
// shape is coherent AND the stored artifact reference is the exact canonical
// name bound to this job. The artifact reader derives the owner from the job
// and rejects any other reference, so a foreign job's reference or an
// arbitrary safe filename must never project as available; it would fail the
// read closed with an owner/ref mismatch.
func ValidArtifactResultForJob(jobID, artifactRef, sha256Hex string, bytes int64) bool {
	if artifactRef != CanonicalArtifactReference(jobID) {
		return false
	}
	return ValidArtifactResult(artifactRef, sha256Hex, bytes)
}

// ResultErrorCode is a bounded, host-owned classification for verified result
// reads. A code is safe to expose in diagnostics, tool responses, and logs: it
// never carries digest values, artifact references, paths, owners, actors,
// conversations, or result content. The set is closed; unknown codes fail
// closed to ResultErrorArtifactInvalid.
type ResultErrorCode string

const (
	// ResultErrorIdentityInvalid classifies an inline result row whose stored
	// identity is incoherent: empty or invalid UTF-8 content, a missing or
	// malformed digest, or a byte count or SHA-256 that does not match the
	// summary.
	ResultErrorIdentityInvalid ResultErrorCode = "result_identity_invalid"
	// ResultErrorArtifactMissing classifies a file-mode read that cannot
	// obtain the artifact: store unavailable, file absent, unreadable,
	// replaced, or outside the configured read bound.
	ResultErrorArtifactMissing ResultErrorCode = "result_artifact_missing"
	// ResultErrorArtifactOwnerRefMismatch classifies a file-mode reference
	// that is not bound to the reading owner.
	ResultErrorArtifactOwnerRefMismatch ResultErrorCode = "result_artifact_owner_ref_mismatch"
	// ResultErrorArtifactBytesMismatch classifies a file-mode byte count that
	// does not match the stored identity, the verified file size, or the
	// requested UTF-8 range.
	ResultErrorArtifactBytesMismatch ResultErrorCode = "result_artifact_bytes_mismatch"
	// ResultErrorArtifactDigestMismatch classifies a file-mode SHA-256 that
	// does not match the artifact bytes, is missing, or is not a valid digest
	// identity.
	ResultErrorArtifactDigestMismatch ResultErrorCode = "result_artifact_digest_mismatch"
	// ResultErrorArtifactInvalid is the fail-closed fallback for unmapped or
	// out-of-set errors. It predates the taxonomy and remains recognized by
	// every classification layer.
	ResultErrorArtifactInvalid ResultErrorCode = "result_artifact_invalid"
	// ResultErrorChunkRequestInvalid classifies a client chunk-read request
	// that cannot be served without reading result content: an out-of-bounds
	// or non-UTF-8-aligned offset, or a max_bytes bound smaller than the next
	// UTF-8 character. It is a request error, never durable corruption, and is
	// excluded from the identity-failure counter.
	ResultErrorChunkRequestInvalid ResultErrorCode = "result_chunk_request_invalid"
)

// ResultError carries a bounded classification and optional closed detail.
// Error renders only the bounded code, so no sensitive value can ever reach
// logs, tool responses, or operator diagnostics through this type.
type ResultError struct {
	Code ResultErrorCode
	Err  error
}

func (e *ResultError) Error() string {
	return string(e.Code)
}

func (e *ResultError) Unwrap() error {
	return e.Err
}

// ResultErrorCodeOf extracts the bounded classification from an error. Any
// error without a recognized ResultErrorCode fails closed to the generic
// result_artifact_invalid code, so unmapped details can never leak.
func ResultErrorCodeOf(err error) ResultErrorCode {
	var classified *ResultError
	if errors.As(err, &classified) && classified != nil {
		switch classified.Code {
		case ResultErrorIdentityInvalid, ResultErrorArtifactMissing,
			ResultErrorArtifactOwnerRefMismatch, ResultErrorArtifactBytesMismatch,
			ResultErrorArtifactDigestMismatch, ResultErrorArtifactInvalid,
			ResultErrorChunkRequestInvalid:
			return classified.Code
		}
	}
	return ResultErrorArtifactInvalid
}

func sanitizeNotificationText(value string, maxRunes int) string {
	if !utf8.ValidString(value) {
		value = strings.ToValidUTF8(value, "\uFFFD")
	}
	result, err := SanitizeResultText(value)
	if err != nil {
		return ""
	}
	if maxRunes > 0 && utf8.RuneCountInString(result) > maxRunes {
		runes := []rune(result)
		if maxRunes == 1 {
			return "…"
		}
		result = string(runes[:maxRunes-1]) + "…"
	}
	return result
}

func ConversationReplyTarget(key ConversationKey) (ReplyTarget, error) {
	parts := strings.Split(string(key), ":")
	if len(parts) < 4 || parts[0] != "slack" || parts[1] == "" || parts[3] == "" {
		return ReplyTarget{}, errors.New("job conversation key is malformed")
	}
	if len(parts) == 4 && parts[2] != "dm" {
		return ReplyTarget{}, errors.New("job conversation key is malformed")
	}
	if len(parts) != 4 && !(len(parts) == 6 && (parts[2] == "dm" || parts[2] == "channel" || parts[2] == "group") && parts[4] == "thread" && parts[5] != "") {
		return ReplyTarget{}, errors.New("job conversation key is malformed")
	}
	target := ReplyTarget{ChannelID: parts[3]}
	if len(parts) == 6 {
		target.ThreadTS = parts[5]
	}
	return target, nil
}

type ExternalAgentJobMode string

const (
	JobForeground ExternalAgentJobMode = "foreground"
	JobDetached   ExternalAgentJobMode = "detached"
)

type ExternalAgentJobStatus string

const (
	JobQueued            ExternalAgentJobStatus = "queued"
	JobRunning           ExternalAgentJobStatus = "running"
	JobCancelRequested   ExternalAgentJobStatus = "cancel_requested"
	JobInterruptedSafe   ExternalAgentJobStatus = "interrupted_safe"
	JobCompletionUnknown ExternalAgentJobStatus = "completion_unknown"
	JobReconciling       ExternalAgentJobStatus = "reconciling"
	JobCompleted         ExternalAgentJobStatus = "completed"
	JobFailed            ExternalAgentJobStatus = "failed"
	JobCancelled         ExternalAgentJobStatus = "cancelled"
	JobAbandoned         ExternalAgentJobStatus = "abandoned"
)

type ExternalAgentJobRequest struct {
	Provider             string               `json:"provider"`
	Profile              string               `json:"profile"`
	PrimaryProject       string               `json:"primary_project"`
	RegistryRevision     string               `json:"registry_revision"`
	Task                 string               `json:"task"`
	Mode                 ExternalAgentJobMode `json:"mode"`
	PermissionOptionKind string               `json:"permission_option_kind"`
	Timeout              time.Duration        `json:"timeout_nanos"`
	PrimaryPath          string               `json:"-"`
	WrapperCallID        string               `json:"wrapper_call_id"`
	OriginalCallID       string               `json:"original_call_id"`
	Actor                string               `json:"actor"`
	TeamID               string               `json:"team_id"`
	ConversationKey      ConversationKey      `json:"conversation_key"`
}

type ExternalAgentJob struct {
	ID                  string
	Mode                ExternalAgentJobMode
	Provider            string
	Profile             string
	PrimaryProject      string
	RegistryRevision    string
	Task                string
	RequestSHA256       string
	WrapperCallID       string
	OriginalCallID      string
	Actor               string
	TeamID              string
	ConversationKey     ConversationKey
	Status              ExternalAgentJobStatus
	Attempt             int
	ACPSessionID        string
	SideEffectsPossible bool
	LeaseOwner          string
	LeaseExpiry         time.Time
	HeartbeatAt         time.Time
	TimeoutAt           time.Time
	ResultSummary       string
	ResultArtifact      string
	ResultSHA256        string
	ResultBytes         int64
	ErrorCode           string
	StatusRevision      int
	CreatedAt           time.Time
	StartedAt           time.Time
	FinishedAt          time.Time
	UpdatedAt           time.Time
}

// ExternalAgentJobStatusView is the host-owned, model-facing subset of a job.
// It intentionally omits task text, provider configuration, actor identity,
// and the persisted destination.
type ExternalAgentJobStatusView struct {
	JobID           string
	Status          ExternalAgentJobStatus
	StatusRevision  int
	ACPSessionID    string
	ResultAvailable bool
	ResultSHA256    string
	ResultBytes     int64
	DeliveryMode    JobResultDeliveryMode
	ErrorCode       string
	FinishedAt      time.Time
	// Live ACP projection fields are content-free and may be empty when the
	// durable projection has not started or the provider is not ACP-backed.
	Phase                    ACPProgressPhase
	Health                   ACPProgressHealth
	LastEventKind            ACPEventKind
	LastTransportActivityAt  time.Time
	LastSessionUpdateAt      time.Time
	LastMeaningfulProgressAt time.Time
	ActiveToolCount          int
	PendingPermission        bool
	PromptElapsedSeconds     int64
	StopReason               string
	ProcessAlive             *bool
}

// ExternalAgentJobShutdownStats is content-free lifecycle telemetry.
type ExternalAgentJobShutdownStats struct {
	Queued            int
	Running           int
	Reconciling       int
	CompletionUnknown int
}

// ExternalAgentJobResult is the complete sanitized result returned by the
// host-completion path. Artifact references never cross this boundary.
type ExternalAgentJobResult struct {
	JobID          string
	StatusRevision int
	Text           string
	ContentSHA256  string
	ContentBytes   int64
	DeliveryMode   JobResultDeliveryMode
}

func (j ExternalAgentJob) StatusView() ExternalAgentJobStatusView {
	mode := JobResultDeliveryMode("")
	if j.ResultArtifact != "" {
		mode = JobResultDeliveryFile
	} else if j.ResultSummary != "" {
		mode = JobResultDeliveryMarkdown
	}
	available := false
	if j.Status == JobCompleted {
		// The artifact shape governs whenever an artifact reference exists,
		// matching the readers' precedence; an incoherent artifact never falls
		// back to the inline shape.
		if j.ResultArtifact != "" {
			available = ValidArtifactResultForJob(j.ID, j.ResultArtifact, j.ResultSHA256, j.ResultBytes)
		} else {
			available = ValidInlineResult(j.ResultSummary, j.ResultSHA256, j.ResultBytes)
		}
	}
	return ExternalAgentJobStatusView{
		JobID: j.ID, Status: j.Status, StatusRevision: j.StatusRevision,
		ACPSessionID:    j.ACPSessionID,
		ResultAvailable: available,
		ResultSHA256:    j.ResultSHA256, ResultBytes: j.ResultBytes, DeliveryMode: mode,
		ErrorCode: j.ErrorCode, FinishedAt: j.FinishedAt,
	}
}

func (j ExternalAgentJob) Validate() error {
	if j.ID == "" || j.Provider == "" || j.Profile == "" || j.PrimaryProject == "" || j.Task == "" {
		return errors.New("external-agent job required fields are missing")
	}
	if utf8.RuneCountInString(j.Task) > MaxExternalAgentTaskRunes {
		return errors.New("external-agent task exceeds the configured character budget")
	}
	if j.Mode != JobForeground && j.Mode != JobDetached {
		return fmt.Errorf("unsupported external-agent job mode %q", j.Mode)
	}
	if j.Status == "" {
		return errors.New("external-agent job status is required")
	}
	if j.Attempt < 0 || j.ResultBytes < 0 || j.StatusRevision < 0 {
		return errors.New("external-agent job counters are invalid")
	}
	return nil
}

func (j *ExternalAgentJob) Transition(next ExternalAgentJobStatus) error {
	if j == nil {
		return errors.New("external-agent job is nil")
	}
	if !validJobTransition(j.Status, next) {
		return fmt.Errorf("illegal external-agent job transition %q -> %q", j.Status, next)
	}
	j.Status = next
	return nil
}

func validJobTransition(from, to ExternalAgentJobStatus) bool {
	switch from {
	case JobQueued:
		return to == JobRunning || to == JobCancelled
	case JobRunning:
		return to == JobCompleted || to == JobFailed || to == JobCancelRequested || to == JobInterruptedSafe || to == JobCompletionUnknown
	case JobCancelRequested:
		return to == JobCancelled || to == JobCompletionUnknown
	case JobInterruptedSafe:
		return to == JobQueued || to == JobCancelled
	case JobCompletionUnknown:
		return to == JobReconciling || to == JobAbandoned
	case JobReconciling:
		return to == JobCompleted || to == JobFailed || to == JobCompletionUnknown || to == JobCancelRequested
	default:
		return false
	}
}

func ExternalAgentJobRequestDigest(request ExternalAgentJobRequest) string {
	canonical, _ := json.Marshal(request)
	digest := sha256.Sum256(canonical)
	return fmt.Sprintf("%x", digest)
}
