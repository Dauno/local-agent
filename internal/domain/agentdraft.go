package domain

// AgentDraft is the user-provided input for creating a new agent definition.
type AgentDraft struct {
	Name        string
	Description string
	Instruction string
	Model       string // optional, in provider/profile format
}
