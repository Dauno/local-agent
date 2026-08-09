// Package sandbox validates and executes model-requested capabilities with
// authorization, locks, idempotency, resource limits, and audit records.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

// Config holds the sandbox use case parameters.
type Config struct {
	AllowedCapabilities []domain.Capability
	CommandTimeout      time.Duration
	MaxOutputBytes      int
}

// Dependencies wires the sandbox service to its adapters.
type Dependencies struct {
	AuditStore SandboxAuditStore
	Executor   SandboxExecutor
}

// SandboxAuditStore persists tool execution audit records and enforces
// idempotency via the original call ID.
type SandboxAuditStore interface {
	InsertAudit(ctx context.Context, record domain.ToolAuditRecord) error
	UpdateAuditState(ctx context.Context, callID string, state domain.ToolLifecycleState, completedAt time.Time) error
	GetAuditByCallID(ctx context.Context, callID string) (*domain.ToolAuditRecord, error)
}

// SandboxExecutor runs a capability against the local environment.
type SandboxExecutor interface {
	Execute(ctx context.Context, op SandboxOperation) (SandboxResult, error)
}

// SandboxOperation describes one capability invocation.
type SandboxOperation struct {
	Capability domain.Capability
	Args       map[string]any
	Actor      string
}

// SandboxResult is the structured output of a sandbox operation.
type SandboxResult struct {
	Output      string
	OutputBytes int
	Truncated   bool
}

// Service validates capabilities, logs audit records, and delegates
// execution to the configured executor.
type Service struct {
	cfg      Config
	audit    SandboxAuditStore
	executor SandboxExecutor
}

// New creates a sandbox service.
func New(cfg Config, deps Dependencies) (*Service, error) {
	if len(cfg.AllowedCapabilities) == 0 {
		return nil, errors.New("at least one allowed capability is required")
	}
	if deps.AuditStore == nil {
		return nil, errors.New("sandbox audit store is required")
	}
	if deps.Executor == nil {
		return nil, errors.New("sandbox executor is required")
	}
	if cfg.CommandTimeout <= 0 {
		cfg.CommandTimeout = 30 * time.Second
	}
	if cfg.MaxOutputBytes <= 0 {
		cfg.MaxOutputBytes = 64 * 1024
	}
	return &Service{
		cfg:      cfg,
		audit:    deps.AuditStore,
		executor: deps.Executor,
	}, nil
}

// Run validates and executes a single sandbox operation.
func (s *Service) Run(ctx context.Context, callID string, capability domain.Capability, args map[string]any, actor string) (SandboxResult, error) {
	if strings.TrimSpace(callID) == "" || strings.TrimSpace(actor) == "" {
		return SandboxResult{}, errors.New("sandbox call ID and actor are required")
	}
	now := time.Now().UTC()

	// Check idempotency — if this call ID has already been processed, return cached result.
	existing, err := s.audit.GetAuditByCallID(ctx, callID)
	if err != nil {
		return SandboxResult{}, fmt.Errorf("read audit record: %w", err)
	}
	if existing != nil {
		if existing.LifecycleState == domain.ToolStateCompleted {
			return SandboxResult{}, fmt.Errorf("operation already completed for call %s", callID)
		}
		if existing.LifecycleState == domain.ToolStateFailed {
			return SandboxResult{}, fmt.Errorf("operation previously failed for call %s", callID)
		}
		if existing.LifecycleState == domain.ToolStateRunning {
			return SandboxResult{}, fmt.Errorf("operation already in progress for call %s", callID)
		}
		return SandboxResult{}, fmt.Errorf("operation already recorded for call %s", callID)
	}

	// Validate capability is allowed.
	if !slices.Contains(s.cfg.AllowedCapabilities, capability) {
		if err := s.audit.InsertAudit(ctx, domain.ToolAuditRecord{
			OriginalCallID:      callID,
			Capability:          capability,
			Actor:               actor,
			AuthorizationResult: "denied",
			LifecycleState:      domain.ToolStateRejected,
			CreatedAt:           now,
		}); err != nil {
			return SandboxResult{}, fmt.Errorf("audit denied operation: %w", err)
		}
		return SandboxResult{}, fmt.Errorf("sandbox capability not authorized: %s", capability)
	}
	if err := s.ValidateArgs(capability, args); err != nil {
		if auditErr := s.audit.InsertAudit(ctx, domain.ToolAuditRecord{
			OriginalCallID: callID, Capability: capability, Actor: actor,
			AuthorizationResult: "invalid_arguments", LifecycleState: domain.ToolStateRejected, CreatedAt: now,
		}); auditErr != nil {
			return SandboxResult{}, fmt.Errorf("audit invalid operation: %w", auditErr)
		}
		return SandboxResult{}, err
	}

	// Record the audit entry as authorized.
	if err := s.audit.InsertAudit(ctx, domain.ToolAuditRecord{
		OriginalCallID:      callID,
		Capability:          capability,
		Actor:               actor,
		AuthorizationResult: "allowed",
		LifecycleState:      domain.ToolStateAuthorized,
		CreatedAt:           now,
	}); err != nil {
		return SandboxResult{}, fmt.Errorf("audit insert: %w", err)
	}

	// Execute the operation.
	if err := s.audit.UpdateAuditState(ctx, callID, domain.ToolStateRunning, now); err != nil {
		return SandboxResult{}, fmt.Errorf("mark audit running: %w", err)
	}

	op := SandboxOperation{Capability: capability, Args: args, Actor: actor}
	execCtx, cancel := context.WithTimeout(ctx, s.cfg.CommandTimeout)
	defer cancel()
	result, execErr := s.executor.Execute(execCtx, op)

	completedAt := time.Now().UTC()
	if execErr != nil {
		if auditErr := s.audit.UpdateAuditState(ctx, callID, domain.ToolStateFailed, completedAt); auditErr != nil {
			return result, fmt.Errorf("sandbox execution failed: %w; mark audit failed: %v", execErr, auditErr)
		}
		return result, execErr
	}
	if len(result.Output) > s.cfg.MaxOutputBytes {
		cut := s.cfg.MaxOutputBytes
		for cut > 0 && !utf8.ValidString(result.Output[:cut]) {
			cut--
		}
		result.Output = result.Output[:cut]
		result.OutputBytes = cut
		result.Truncated = true
	}

	if err := s.audit.UpdateAuditState(ctx, callID, domain.ToolStateCompleted, completedAt); err != nil {
		return SandboxResult{}, fmt.Errorf("mark audit completed: %w", err)
	}
	return result, nil
}

// ValidateArgs checks that required arguments are present and safe.
func (s *Service) ValidateArgs(cap domain.Capability, args map[string]any) error {
	switch cap {
	case domain.CapReadFile, domain.CapListWorktrees:
		project, ok := args["project"].(string)
		if !ok || strings.TrimSpace(project) == "" {
			return errors.New("project is required")
		}
		if cap == domain.CapListWorktrees {
			return nil
		}
		path, ok := args["path"].(string)
		if !ok || strings.TrimSpace(path) == "" {
			return errors.New("path is required for read_file")
		}
		return validateRelativePath(path)
	case domain.CapListDirectory:
		project, ok := args["project"].(string)
		if !ok || strings.TrimSpace(project) == "" {
			return errors.New("project is required")
		}
		path, ok := args["path"].(string)
		if !ok || strings.TrimSpace(path) == "" {
			return nil // defaults to "."
		}
		return validateRelativePath(path)
	}
	return nil
}

func validateRelativePath(path string) error {
	if filepath.IsAbs(path) {
		return errors.New("path traversal not allowed")
	}
	if strings.Contains(path, "~") || strings.ContainsRune(path, '\x00') {
		return errors.New("path traversal not allowed")
	}
	segs := strings.Split(filepath.ToSlash(path), "/")
	for _, seg := range segs {
		if seg == ".." {
			return errors.New("path traversal not allowed")
		}
		if seg == ".env" || seg == ".git" {
			return errors.New("path not allowed")
		}
	}
	return nil
}
