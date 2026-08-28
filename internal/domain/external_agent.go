package domain

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

const (
	ExternalAgentStopReasonEndTurn   = "end_turn"
	ExternalAgentStopReasonCancelled = "cancelled"
	ExternalAgentStopReasonMaxTokens = "max_tokens"
	ExternalAgentStopReasonRefusal   = "refusal"
)

// ExternalAgentErrorCode is a bounded host-owned failure classification. The
// code is safe to expose in diagnostics; frame content is never included.
type ExternalAgentErrorCode string

const (
	ExternalAgentErrorFrameTooLarge              ExternalAgentErrorCode = "acp_frame_too_large"
	ExternalAgentErrorMalformedFrame             ExternalAgentErrorCode = "acp_malformed_frame"
	ExternalAgentErrorProtocolViolation          ExternalAgentErrorCode = "acp_protocol_violation"
	ExternalAgentErrorConfigDrift                ExternalAgentErrorCode = "acp_config_drift"
	ExternalAgentErrorIdleTimeout                ExternalAgentErrorCode = "acp_idle_timeout"
	ExternalAgentErrorJobTimeout                 ExternalAgentErrorCode = "acp_job_timeout"
	ExternalAgentErrorProcessExit                ExternalAgentErrorCode = "acp_process_exit"
	ExternalAgentErrorResultTooLarge             ExternalAgentErrorCode = "acp_result_too_large"
	ExternalAgentErrorResultArtifactInvalid      ExternalAgentErrorCode = "result_artifact_invalid"
	ExternalAgentErrorResultDeliveryFailed       ExternalAgentErrorCode = "result_delivery_failed"
	ExternalAgentErrorCompletedWithoutFinalText  ExternalAgentErrorCode = "acp_completed_without_final_message"
	ExternalAgentErrorPermissionUnavailable      ExternalAgentErrorCode = "acp_permission_unavailable"
	ExternalAgentErrorInvalidInput               ExternalAgentErrorCode = "acp_invalid_input"
	ExternalAgentErrorSessionRecoveryUnsupported ExternalAgentErrorCode = "acp_session_recovery_unsupported"
	ExternalAgentErrorProgressInvalid            ExternalAgentErrorCode = "acp_progress_invalid"
)

type ExternalAgentError struct {
	Code ExternalAgentErrorCode
	Err  error
}

func (e *ExternalAgentError) Error() string {
	if e.Err == nil {
		return string(e.Code)
	}
	return fmt.Sprintf("%s: %v", e.Code, e.Err)
}

func (e *ExternalAgentError) Unwrap() error {
	return e.Err
}

type ExternalAgentInvocationRequest struct {
	JobID                string
	PrimaryProject       string
	PrimaryPath          string
	ProfileName          string
	ProviderName         string
	RegistryRevision     string
	PermissionOptionKind string
	GlobalInstruction    string
	AgentInstruction     string
	Task                 string
	Timeout              time.Duration
	// These fields are trusted host identity and are never included in the prompt.
	Actor           string
	TeamID          string
	ConversationKey ConversationKey
	OriginalCallID  string
	// These host-owned hooks are used by durable jobs and are never serialized
	// into provider or model-visible content.
	OnSessionCreated      func(string) error
	OnSideEffectsPossible func() error
	BeforePermission      func() error
	// OnProgress receives content-free live progress events derived from the
	// provider stream. Monitoring failures must never fail or cancel an
	// otherwise healthy invocation, so the callback returns no error.
	OnProgress func(ExternalAgentProgressEvent)
}

type ExternalAgentInvocationResult struct {
	Text        string
	Inline      bool
	ArtifactRef string
	// NativeResultID is host-only durable catalog identity. It is carried from
	// the runtime to the job transition and never persisted in model-visible
	// result text or provider protocol payloads.
	NativeResultID            string
	NativeJobID               string
	NativeResultHandle        ResultHandle
	ResultSHA256              string
	ResultBytes               int64
	DeliveryMode              JobResultDeliveryMode
	DeliveryCanonicalMarkdown string
	DeliveryPolicyVersion     string
	DeliveryMaxMarkdownParts  int
	DeliveryContentSHA256     string
	DeliveryContentBytes      int64
	DeliveryArtifactRef       string
}

type ResultArtifact struct {
	Reference string
	SHA256    string
	Bytes     int64
}

type GitDeliveryResult struct {
	Status     string `json:"status"`
	Repository string `json:"repository"`
	PRURL      string `json:"pr_url"`
	Branch     string `json:"branch"`
	BaseBranch string `json:"base_branch"`
	Remote     string `json:"remote"`
	Commit     string `json:"commit"`
	Title      string `json:"title"`
	FilePath   string `json:"file_path"`
	Worktree   string `json:"worktree"`
	Error      string `json:"error"`
}

var validGitDeliveryStatuses = map[string]bool{
	"success": true,
	"blocked": true,
	"failed":  true,
}

const maxGitDeliveryFieldRunes = 4096

func ParseGitDeliveryResult(data []byte, targetProject, worktreeRoot string) (GitDeliveryResult, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return GitDeliveryResult{}, fmt.Errorf("decode git delivery result: %w", err)
	}
	requiredFields := []string{"status", "repository", "pr_url", "branch", "base_branch", "remote", "commit", "title", "file_path", "worktree", "error"}
	for _, field := range requiredFields {
		if _, exists := fields[field]; !exists {
			return GitDeliveryResult{}, fmt.Errorf("git delivery result is missing field %q", field)
		}
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var result GitDeliveryResult
	if err := decoder.Decode(&result); err != nil {
		return GitDeliveryResult{}, fmt.Errorf("decode git delivery result: %w", err)
	}
	if err := result.Validate(targetProject, worktreeRoot); err != nil {
		return GitDeliveryResult{}, err
	}
	return result, nil
}

func (r *GitDeliveryResult) Validate(targetProject, worktreeRoot string) error {
	if r == nil {
		return fmt.Errorf("git delivery result is nil")
	}
	if !validGitDeliveryStatuses[r.Status] {
		return fmt.Errorf("git delivery status must be success, blocked, or failed, got %q", r.Status)
	}
	if r.Repository != targetProject {
		return fmt.Errorf("git delivery repository %q does not match target project %q", r.Repository, targetProject)
	}
	if strings.TrimSpace(r.Worktree) != "" {
		canonicalRoot, err := filepath.EvalSymlinks(worktreeRoot)
		if err != nil {
			return fmt.Errorf("git delivery worktree root %q cannot be resolved: %w", worktreeRoot, err)
		}
		canonical, err := filepath.EvalSymlinks(r.Worktree)
		if err != nil {
			return fmt.Errorf("git delivery worktree %q cannot be resolved: %w", r.Worktree, err)
		}
		relative, err := filepath.Rel(canonicalRoot, canonical)
		if err != nil || !filepath.IsLocal(relative) {
			return fmt.Errorf("git delivery worktree %q is outside worktree root %q", canonical, canonicalRoot)
		}
	}
	fields := []string{r.Status, r.Repository, r.PRURL, r.Branch, r.BaseBranch, r.Remote, r.Commit, r.Title, r.FilePath, r.Worktree, r.Error}
	for _, field := range fields {
		if len([]rune(field)) > maxGitDeliveryFieldRunes {
			return fmt.Errorf("git delivery field is invalid or exceeds %d characters", maxGitDeliveryFieldRunes)
		}
		if strings.ContainsFunc(field, unicode.IsControl) {
			return fmt.Errorf("git delivery field contains control characters")
		}
	}
	if r.Status == "success" {
		if strings.TrimSpace(r.PRURL) == "" {
			return fmt.Errorf("pr_url is required for successful delivery")
		}
		parsedURL, err := url.ParseRequestURI(r.PRURL)
		if err != nil || parsedURL.Host == "" || (parsedURL.Scheme != "https" && parsedURL.Scheme != "http") {
			return fmt.Errorf("pr_url must be an absolute HTTP(S) URL")
		}
		if strings.TrimSpace(r.Commit) == "" {
			return fmt.Errorf("commit is required for successful delivery")
		}
		if strings.TrimSpace(r.Branch) == "" {
			return fmt.Errorf("branch is required for successful delivery")
		}
		if strings.TrimSpace(r.FilePath) == "" {
			return fmt.Errorf("file_path is required for successful delivery")
		}
		if !filepath.IsLocal(r.FilePath) {
			return fmt.Errorf("file_path escapes the repository")
		}
		if strings.TrimSpace(r.Title) == "" {
			return fmt.Errorf("title is required for successful delivery")
		}
	}
	if r.Status != "success" && r.PRURL != "" {
		return fmt.Errorf("pr_url must be empty unless delivery succeeded")
	}
	return nil
}
