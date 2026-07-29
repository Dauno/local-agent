package port

// AgentDefinitionWriter writes agent definition YAML files atomically.
type AgentDefinitionWriter interface {
	// Write writes definition bytes as <name>.yaml. Fails atomically if a different file exists.
	Write(name string, definition []byte) error
}
