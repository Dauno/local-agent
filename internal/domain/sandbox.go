package domain

import "time"

// Capability is a typed sandbox operation that the model may request.
type Capability string

const (
	CapListRepos      Capability = "list_repos"
	CapReadFile       Capability = "read_file"
	CapListWorktrees  Capability = "list_worktrees"
	CapCreateWorktree Capability = "create_worktree"
	CapRemoveWorktree Capability = "remove_worktree"
	CapRunCommand     Capability = "run_command"
)

// IsReadOnly returns true for capabilities that do not mutate state.
func (c Capability) IsReadOnly() bool {
	switch c {
	case CapListRepos, CapReadFile, CapListWorktrees:
		return true
	default:
		return false
	}
}

// ToolAuditRecord is the domain representation of a tool execution audit entry.
type ToolAuditRecord struct {
	OriginalCallID     string
	Capability         Capability
	Actor              string
	AuthorizationResult string
	IdempotencyKey     string
	LifecycleState     ToolLifecycleState
	CreatedAt          time.Time
	CompletedAt        time.Time
}

type ToolLifecycleState string

const (
	ToolStateRequested   ToolLifecycleState = "requested"
	ToolStateAuthorized  ToolLifecycleState = "authorized"
	ToolStateRunning     ToolLifecycleState = "running"
	ToolStateCompleted   ToolLifecycleState = "completed"
	ToolStateFailed      ToolLifecycleState = "failed"
	ToolStateRejected    ToolLifecycleState = "rejected"
)
