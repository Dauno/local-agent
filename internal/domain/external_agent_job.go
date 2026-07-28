package domain

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const MaxExternalAgentTaskRunes = 200_000

const (
	JobNotificationTerminal = "terminal"
	JobNotificationRenderer = "markdown_v1"
)

type NotificationPublishState string

const (
	NotificationPending    NotificationPublishState = "pending"
	NotificationPublishing NotificationPublishState = "publishing"
	NotificationPublished  NotificationPublishState = "published"
	NotificationUnknown    NotificationPublishState = "unknown"
)

type ExternalAgentJobNotification struct {
	JobID               string
	StatusRevision      int
	Kind                string
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
		markdown += "\n\nSummary: " + sanitizeNotificationText(job.ResultSummary, 2000)
	}
	if job.Status == JobCompletionUnknown {
		markdown = fmt.Sprintf("OpenCode job `%s` was interrupted after external actions may have occurred. It was not retried; reconciliation is required.", job.ID)
	}
	if job.Status == JobFailed {
		markdown += " The operation failed with a host-owned error code."
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
		CanonicalMarkdown: markdown, ContentSHA256: fmt.Sprintf("%x", digest),
		RendererVersion: JobNotificationRenderer, Target: target,
		PublishState: NotificationPending,
	}, nil
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
