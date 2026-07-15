package opencodeshim

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/Dauno/slack-local-agent/internal/cliprotocol"
)

// BuildRunArgs produces the deterministic `opencode run` argument vector. User
// text is never included; the prompt is delivered on stdin.
func BuildRunArgs(request cliprotocol.Request) []string {
	args := []string{"run", "--format", "json"}
	if request.Workspace != nil && request.Workspace.WorkingDirectory != "" {
		args = append(args, "--dir", request.Workspace.WorkingDirectory)
	}
	if request.Profile != nil {
		if request.Profile.Model != "" {
			args = append(args, "--model", request.Profile.Model)
		}
		if request.Profile.Agent != "" {
			args = append(args, "--agent", request.Profile.Agent)
		}
		if request.Profile.Variant != "" {
			args = append(args, "--variant", request.Profile.Variant)
		}
		if request.Profile.Approval == cliprotocol.ApprovalAuto {
			args = append(args, "--auto")
		}
	}
	return args
}

// BuildPrompt flattens the trusted instructions, workspace registry, and
// bounded transcript into one OpenCode user message in deterministic order.
func BuildPrompt(request cliprotocol.Request) string {
	var builder strings.Builder

	instruction := strings.TrimSpace(request.SystemInstruction)
	if instruction != "" {
		builder.WriteString("<<AGENT INSTRUCTIONS (trusted)>>\n")
		builder.WriteString(instruction)
		builder.WriteString("\n\n")
	}

	if request.Workspace != nil && len(request.Workspace.Projects) > 0 {
		projects := append([]cliprotocol.Project(nil), request.Workspace.Projects...)
		sort.Slice(projects, func(i, j int) bool { return projects[i].Name < projects[j].Name })
		registry, _ := json.Marshal(struct {
			WorkingDirectory string                `json:"working_directory"`
			Projects         []cliprotocol.Project `json:"projects"`
		}{
			WorkingDirectory: request.Workspace.WorkingDirectory,
			Projects:         projects,
		})
		builder.WriteString("<<WORKSPACE REGISTRY (trusted)>>\n")
		builder.Write(registry)
		builder.WriteString("\n\n")
	}

	builder.WriteString("<<CONVERSATION TRANSCRIPT>>\n")
	for _, message := range request.Messages {
		label := "user"
		if message.Role == cliprotocol.RoleAssistant {
			label = "assistant"
		}
		builder.WriteString("[")
		builder.WriteString(label)
		builder.WriteString("]\n")
		builder.WriteString(message.Text)
		builder.WriteString("\n\n")
	}
	builder.WriteString("The final transcript item above is the current request. Respond to it.")

	return builder.String()
}
