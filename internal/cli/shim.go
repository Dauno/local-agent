package cli

import (
	"github.com/spf13/cobra"

	"github.com/Dauno/slack-local-agent/internal/adapter/codexshim"
	"github.com/Dauno/slack-local-agent/internal/adapter/opencodeshim"
)

// newShimCommand registers the hidden `shim` command group. Shims are mapper
// processes launched by an agent_cli provider; they never start the Slack
// application, load configuration, or open state.
func newShimCommand(streams Streams) *cobra.Command {
	command := &cobra.Command{
		Use:    "shim",
		Short:  "Run a cli-v1 mapper for an agent CLI (internal use)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return command.Help()
		},
	}
	command.AddCommand(newOpenCodeShimCommand(streams))
	command.AddCommand(newCodexShimCommand(streams))
	return command
}

func newOpenCodeShimCommand(streams Streams) *cobra.Command {
	return &cobra.Command{
		Use:    "opencode",
		Short:  "Map cli-v1 to the OpenCode CLI",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			code := opencodeshim.Run(command.Context(), streams.In, streams.Out, opencodeshim.Config{})
			if code != 0 {
				return &ExitError{Code: code}
			}
			return nil
		},
	}
}

func newCodexShimCommand(streams Streams) *cobra.Command {
	return &cobra.Command{
		Use:    "codex",
		Short:  "Map cli-v1 to the Codex CLI",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			code := codexshim.Run(command.Context(), streams.In, streams.Out, codexshim.Config{})
			if code != 0 {
				return &ExitError{Code: code}
			}
			return nil
		},
	}
}
