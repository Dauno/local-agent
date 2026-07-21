package app

import (
	"fmt"
	"sort"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	opencodeusecase "github.com/Dauno/slack-local-agent/internal/usecase/opencode"
)

func managementProbePath(projects map[string]string) (string, error) {
	if len(projects) == 0 {
		return "", fmt.Errorf("OpenCode management requires a registered project")
	}
	names := make([]string, 0, len(projects))
	for name := range projects {
		names = append(names, name)
	}
	sort.Strings(names)
	return canonicalProjectPath(projects[names[0]])
}

type openCodeManagementToolFactory struct {
	base          port.AgentToolFactory
	runtime       port.ExternalAgentRuntime
	manager       port.OpenCodeManager
	allowedIDs    []string
	primaryPath   string
	configOptions []domain.ACPConfigOption
	coordinator   port.OpenCodeCoordinator
}

func (f *openCodeManagementToolFactory) ToolsForInvocation(actor string, key domain.ConversationKey) ([]any, error) {
	var tools []any
	if f.base != nil {
		base, err := f.base.ToolsForInvocation(actor, key)
		if err != nil {
			return nil, err
		}
		tools = append(tools, base...)
	}
	type args struct {
		Action string `json:"action" jsonschema:"one of status, probe, upgrade, rollback"`
	}
	type result struct {
		Success        bool   `json:"success"`
		PriorVersion   string `json:"prior_version,omitempty"`
		CurrentVersion string `json:"current_version,omitempty"`
		Diagnostic     string `json:"diagnostic"`
	}
	managementTool, err := functiontool.New(functiontool.Config{
		Name:        "manage_opencode",
		Description: "Checks or repairs the trusted OpenCode installation. Upgrade and rollback require operator authorization and confirmation.",
		RequireConfirmationProvider: func(input args) bool {
			return input.Action == "upgrade" || input.Action == "rollback"
		},
	}, func(ctx agent.Context, input args) (result, error) {
		deps := opencodeusecase.Dependencies{
			Runtime: f.runtime, Manager: f.manager, ActorID: actor,
			AllowedIDs: f.allowedIDs, PrimaryPath: f.primaryPath, ConfigOptions: f.configOptions, Coordinator: f.coordinator,
		}
		var output opencodeusecase.Result
		var callErr error
		switch input.Action {
		case "status":
			output, callErr = opencodeusecase.Status(ctx, deps)
		case "probe":
			output, callErr = opencodeusecase.Probe(ctx, deps, f.primaryPath, f.configOptions)
		case "upgrade":
			output, callErr = opencodeusecase.Upgrade(ctx, deps)
		case "rollback":
			output, callErr = opencodeusecase.Rollback(ctx, deps)
		default:
			return result{}, fmt.Errorf("unsupported OpenCode management action %q", input.Action)
		}
		if callErr != nil {
			return result{}, callErr
		}
		return result{Success: output.Success, PriorVersion: output.PriorVersion, CurrentVersion: output.CurrentVersion, Diagnostic: output.Diagnostic}, nil
	})
	if err != nil {
		return nil, err
	}
	return append(tools, managementTool), nil
}
