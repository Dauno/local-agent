package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
)

func newTemplatesCommand(backend Backend, streams Streams) *cobra.Command {
	command := &cobra.Command{
		Use:   "templates",
		Short: "Inspect embedded Block Kit templates",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return command.Help()
		},
	}
	command.AddCommand(
		newTemplatesLintCommand(backend, streams),
		newTemplatesPreviewCommand(backend, streams),
	)
	return command
}

func newTemplatesLintCommand(backend Backend, streams Streams) *cobra.Command {
	return &cobra.Command{
		Use:   "lint",
		Short: "Validate all embedded template bindings",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			provider, ok := backend.(TemplateBackend)
			if !ok {
				return &ExitError{Code: 1, Cause: errors.New("templates lint is unavailable")}
			}
			lines, err := provider.LintTemplates(command.Context())
			if err != nil {
				return &ExitError{Code: 1, Cause: err}
			}
			writeLines(streams.Out, lines)
			return nil
		},
	}
}

func newTemplatesPreviewCommand(backend Backend, streams Streams) *cobra.Command {
	var minimal bool
	command := &cobra.Command{
		Use:   "preview <name>",
		Short: "Render one embedded template with representative values",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			provider, ok := backend.(TemplateBackend)
			if !ok {
				return &ExitError{Code: 1, Cause: errors.New("templates preview is unavailable")}
			}
			content, err := provider.PreviewTemplate(command.Context(), args[0], !minimal)
			if err != nil {
				return &ExitError{Code: 1, Cause: err}
			}
			_, _ = fmt.Fprint(streams.Out, content)
			if !strings.HasSuffix(content, "\n") {
				_, _ = fmt.Fprintln(streams.Out)
			}
			return nil
		},
	}
	command.Flags().BoolVar(&minimal, "minimal", false, "omit optional template inputs")
	return command
}

func writeLines(out io.Writer, lines []string) {
	for _, line := range lines {
		_, _ = fmt.Fprintln(out, line)
	}
}
