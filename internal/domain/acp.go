package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	ProviderTypeACP        = "acp"
	ProviderFamilyACP      = "acp"
	ACPProtocolVersion     = "1"
	ACPClientIdentity      = "slack-local-agent"
	ACPClientVersion       = "v1"
	ACPStopReasonEndTurn   = "end_turn"
	ACPStopReasonCancelled = "cancelled"
	ACPStopReasonMaxTokens = "max_tokens"
	ACPStopReasonRefusal   = "refusal"

	ACPPermissionRejectOnce = "reject_once"
	ACPPermissionAllowOnce  = "allow_once"

	ACPFallbackExternalDirectoryMode = "external_directory"
)

type ACPConfigOption struct {
	ID    string
	Value any
}

type ACPPermissionOption struct {
	Kind string
	ID   string
}

type ACPPermissionRequest struct {
	SessionID string
	RequestID string
	Options   []ACPPermissionOption
	Path      string
}

type ACPAgentInfo struct {
	Name    string
	Version string
}

type ACPSessionCapabilities struct {
	AdditionalDirectories bool
	Close                 bool
}

type ACPInitResult struct {
	ProtocolVersion     string
	AgentInfo           ACPAgentInfo
	ClientCapabilities  map[string]any
	ServerCapabilities  map[string]any
	SessionCapabilities ACPSessionCapabilities
}

type ACPConfigState struct {
	Options []ACPConfigOption
}

type ACPPromptResult struct {
	StopReason string
	Text       string
	Usage      ACPUsage
}

type ACPUsage struct {
	InputTokens  int
	OutputTokens int
}

type ACPToolActivity struct {
	Kind   string
	Status string
	Name   string
}

type AcpInvocationRequest struct {
	PrimaryProject       string
	PrimaryPath          string
	AdditionalProjects   []string
	AdditionalPaths      []string
	ProfileName          string
	ConfigOptions        []ACPConfigOption
	PermissionOptionKind string
	GlobalInstruction    string
	AgentInstruction     string
	Task                 string
	Timeout              time.Duration
}

type AcpInvocationResult struct {
	Text  string
	Usage ACPUsage
	Error string
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
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var result GitDeliveryResult
	if err := decoder.Decode(&result); err != nil {
		return GitDeliveryResult{}, fmt.Errorf("decode git delivery result: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return GitDeliveryResult{}, err
	}
	if err := result.Validate(targetProject, worktreeRoot); err != nil {
		return GitDeliveryResult{}, err
	}
	return result, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("git delivery result must contain exactly one JSON object")
		}
		return fmt.Errorf("decode trailing git delivery data: %w", err)
	}
	return nil
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
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return fmt.Errorf("git delivery worktree %q is outside worktree root %q", canonical, canonicalRoot)
		}
	}
	fields := []string{r.Status, r.Repository, r.PRURL, r.Branch, r.BaseBranch, r.Remote, r.Commit, r.Title, r.FilePath, r.Worktree, r.Error}
	for _, field := range fields {
		if !utf8.ValidString(field) || len([]rune(field)) > maxGitDeliveryFieldRunes {
			return fmt.Errorf("git delivery field is invalid or exceeds %d characters", maxGitDeliveryFieldRunes)
		}
		for _, c := range field {
			if unicode.IsControl(c) {
				return fmt.Errorf("git delivery field contains control characters")
			}
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
		if filepath.IsAbs(r.FilePath) {
			return fmt.Errorf("file_path must be repository-relative")
		}
		cleanFile := filepath.Clean(r.FilePath)
		if cleanFile == ".." || strings.HasPrefix(cleanFile, ".."+string(filepath.Separator)) {
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

type OpenCodeManagementOperation string

const (
	OpCodeManageStatus   OpenCodeManagementOperation = "status"
	OpCodeManageProbe    OpenCodeManagementOperation = "probe"
	OpCodeManageUpgrade  OpenCodeManagementOperation = "upgrade"
	OpCodeManageRollback OpenCodeManagementOperation = "rollback"
)

type OpenCodeManagementRequest struct {
	Operation  OpenCodeManagementOperation
	ActorID    string
	OperatorID string
}

type OpenCodeManagementResult struct {
	Success        bool
	PriorVersion   string
	CurrentVersion string
	Diagnostic     string
}
