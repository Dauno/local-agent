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
	if p.MaxMarkdownParts < 1 || p.MaxMarkdownParts > 8 {
		return errors.New("result delivery max Markdown parts must be between 1 and 8")
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
	HostResultText      string
	CanonicalMarkdown   string
	ContentSHA256       string
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

	// Job-prefixed aliases mirror the existing external-agent job constants.
	JobActivationPending           = ActivationPending
	JobActivationProcessing        = ActivationProcessing
	JobActivationModelStarted      = ActivationModelStarted
	JobActivationResponsePrepared  = ActivationResponsePrepared
	JobActivationCompleted         = ActivationCompleted
	JobActivationCompletionUnknown = ActivationCompletionUnknown
	JobActivationFailed            = ActivationFailed
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
	if len(a.NotificationSHA256) != sha256.Size*2 {
		return errors.New("external-agent activation notification digest is invalid")
	}
	if _, err := hex.DecodeString(a.NotificationSHA256); err != nil {
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

// NotificationHealthSnapshot is kept as a descriptive alias for callers that
// expose the aggregate as a health snapshot.
type NotificationHealthSnapshot = ExternalAgentJobNotificationHealth

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
type ExternalAgentJobInspection struct {
	JobID          string                               `json:"job_id"`
	Status         ExternalAgentJobStatus               `json:"status"`
	StatusRevision int                                  `json:"status_revision"`
	FinishedAt     time.Time                            `json:"finished_at"`
	Deliveries     []ExternalAgentJobDeliveryInspection `json:"deliveries"`
}

// ExternalAgentJobAdminView is an explicit name for the same safe view used by
// the local jobs inspect command.
type ExternalAgentJobAdminView = ExternalAgentJobInspection

// ExternalAgentJobDelivery is the durable, immutable delivery identity. The
// notification name remains as the compatibility-facing store type.
type ExternalAgentJobDelivery = ExternalAgentJobNotification

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
	digest := sha256.Sum256([]byte(markdown))
	target.CorrelationID = fmt.Sprintf("job:%s:%d:%s", job.ID, job.StatusRevision, JobNotificationTerminal)
	return ExternalAgentJobNotification{
		JobID: job.ID, StatusRevision: job.StatusRevision, Kind: JobNotificationTerminal,
		TerminalStatus: job.Status,
		Actor:          job.Actor, ConversationKey: job.ConversationKey,
		CanonicalMarkdown: markdown, ContentSHA256: fmt.Sprintf("%x", digest),
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
	if maxParts > 8 {
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
	if _, err := hex.DecodeString(contentDigest); err != nil || len(contentDigest) != sha256.Size*2 {
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
	target.CorrelationID = fmt.Sprintf("job:%s:%d:%s", job.ID, job.StatusRevision, JobNotificationTerminal)
	return ExternalAgentJobNotification{
		JobID: job.ID, StatusRevision: job.StatusRevision, Kind: JobNotificationTerminal,
		TerminalStatus: job.Status,
		Actor:          job.Actor, ConversationKey: job.ConversationKey,
		CanonicalMarkdown: markdown, ContentSHA256: contentDigest, RendererVersion: JobNotificationRenderer,
		Target: target, PublishState: NotificationPending, DeliveryMode: mode, PolicyVersion: policyVersion,
		ArtifactRef: artifactRef, ResultBytes: contentBytes, MaxMarkdownParts: maxParts,
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

func sanitizeNotificationText(value string, maxRunes int) string {
	var builder strings.Builder
	for _, r := range value {
		if r == '\x00' || (r < ' ' && r != '\n' && r != '\r' && r != '\t') {
			continue
		}
		if r == '<' {
			builder.WriteString("&lt;")
		} else {
			builder.WriteRune(r)
		}
	}
	result := builder.String()
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
	for index := 4; index+1 < len(parts); index++ {
		if parts[index] == "thread" && parts[index+1] != "" {
			target.ThreadTS = parts[index+1]
			break
		}
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
	Provider             string
	Profile              string
	PrimaryProject       string
	AdditionalProjects   []string
	RegistryRevision     string
	Task                 string
	Mode                 ExternalAgentJobMode
	PermissionOptionKind string
	Timeout              time.Duration
	PrimaryPath          string
	AdditionalPaths      []string
	WrapperCallID        string
	OriginalCallID       string
	Actor                string
	TeamID               string
	ConversationKey      ConversationKey
}

type ExternalAgentJob struct {
	ID                  string
	Mode                ExternalAgentJobMode
	Provider            string
	Profile             string
	PrimaryProject      string
	AdditionalProjects  []string
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
	ResultAvailable bool
	ResultSHA256    string
	ResultBytes     int64
	DeliveryMode    JobResultDeliveryMode
	ErrorCode       string
	FinishedAt      time.Time
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
	return ExternalAgentJobStatusView{
		JobID: j.ID, Status: j.Status, StatusRevision: j.StatusRevision,
		ResultAvailable: j.Status == JobCompleted && (j.ResultSummary != "" || j.ResultArtifact != ""),
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
	legal := map[ExternalAgentJobStatus]map[ExternalAgentJobStatus]bool{
		JobQueued:            {JobRunning: true, JobCancelled: true},
		JobRunning:           {JobCompleted: true, JobFailed: true, JobCancelRequested: true, JobInterruptedSafe: true, JobCompletionUnknown: true},
		JobCancelRequested:   {JobCancelled: true, JobCompletionUnknown: true},
		JobInterruptedSafe:   {JobQueued: true, JobCancelled: true},
		JobCompletionUnknown: {JobReconciling: true, JobAbandoned: true},
		JobReconciling:       {JobCompleted: true, JobFailed: true, JobCompletionUnknown: true, JobCancelRequested: true},
	}
	return legal[from][to]
}

func ExternalAgentJobRequestDigest(request ExternalAgentJobRequest) string {
	canonical, _ := json.Marshal(struct {
		Provider             string               `json:"provider"`
		Profile              string               `json:"profile"`
		PrimaryProject       string               `json:"primary_project"`
		AdditionalProjects   []string             `json:"additional_projects"`
		RegistryRevision     string               `json:"registry_revision"`
		Task                 string               `json:"task"`
		Mode                 ExternalAgentJobMode `json:"mode"`
		PermissionOptionKind string               `json:"permission_option_kind"`
		TimeoutNanos         int64                `json:"timeout_nanos"`
		WrapperCallID        string               `json:"wrapper_call_id"`
		OriginalCallID       string               `json:"original_call_id"`
		Actor                string               `json:"actor"`
		TeamID               string               `json:"team_id"`
		ConversationKey      ConversationKey      `json:"conversation_key"`
	}{
		Provider: request.Provider, Profile: request.Profile, PrimaryProject: request.PrimaryProject,
		AdditionalProjects: request.AdditionalProjects, RegistryRevision: request.RegistryRevision,
		Task: request.Task, Mode: request.Mode, PermissionOptionKind: request.PermissionOptionKind,
		TimeoutNanos: request.Timeout.Nanoseconds(), WrapperCallID: request.WrapperCallID,
		OriginalCallID: request.OriginalCallID, Actor: request.Actor, TeamID: request.TeamID,
		ConversationKey: request.ConversationKey,
	})
	digest := sha256.Sum256(canonical)
	return fmt.Sprintf("%x", digest)
}
