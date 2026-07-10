package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Dauno/slack-local-agent/internal/usecase/bootstrap"
	"github.com/Dauno/slack-local-agent/internal/usecase/doctor"
)

type Backend interface {
	PrepareSetup(ctx context.Context) (bootstrap.Snapshot, bootstrap.Secrets, error)
	ApplySetup(ctx context.Context, snapshot bootstrap.Snapshot, identity bootstrap.Identity, access bootstrap.AccessControl, secrets bootstrap.Secrets) error
	Doctor(ctx context.Context, live bool) (doctor.Report, error)
	Run(ctx context.Context) error
	Manifest(ctx context.Context, write bool) (content, path string, err error)
	Version() string
}

type Streams struct {
	In  io.Reader
	Out io.Writer
	Err io.Writer
}

type ExitError struct {
	Code  int
	Cause error
}

func (e *ExitError) Error() string {
	if e.Cause != nil {
		return e.Cause.Error()
	}
	return fmt.Sprintf("command exited with status %d", e.Code)
}

func (e *ExitError) Unwrap() error { return e.Cause }

func NewRoot(backend Backend, streams Streams) (*cobra.Command, error) {
	if backend == nil {
		return nil, errors.New("CLI backend is required")
	}
	if streams.In == nil || streams.Out == nil || streams.Err == nil {
		return nil, errors.New("CLI input, output, and error streams are required")
	}

	root := &cobra.Command{
		Use:           "local-agent",
		Short:         "Local-first conversational Slack agent",
		SilenceErrors: true,
		SilenceUsage:  true,
		Args:          cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return command.Help()
		},
	}
	root.CompletionOptions.DisableDefaultCmd = true
	root.SetIn(streams.In)
	root.SetOut(streams.Out)
	root.SetErr(streams.Err)
	root.AddCommand(
		newInitCommand(backend, streams),
		newDoctorCommand(backend, streams),
		newRunCommand(backend),
		newManifestCommand(backend, streams),
		newVersionCommand(backend, streams),
	)
	return root, nil
}

func Execute(ctx context.Context, root *cobra.Command, args []string, stderr io.Writer) int {
	root.SetArgs(args)
	err := root.ExecuteContext(ctx)
	if err == nil {
		return 0
	}
	var exit *ExitError
	if errors.As(err, &exit) {
		if exit.Cause != nil {
			fmt.Fprintln(stderr, exit.Cause)
		}
		return exit.Code
	}
	fmt.Fprintln(stderr, err)
	return 2
}

func newInitCommand(backend Backend, streams Streams) *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Create and configure local-agent artifacts",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if err := runWizard(command.Context(), backend, NewPrompter(streams.In, streams.Out), streams.Out); err != nil {
				return &ExitError{Code: 1, Cause: err}
			}
			return nil
		},
	}
}

func newDoctorCommand(backend Backend, streams Streams) *cobra.Command {
	var live bool
	command := &cobra.Command{
		Use:   "doctor",
		Short: "Validate local configuration and connectivity",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			report, err := backend.Doctor(command.Context(), live)
			if err != nil {
				return &ExitError{Code: 1, Cause: err}
			}
			for _, result := range report.Results {
				label := "PASS"
				if result.Status == doctor.StatusFail {
					label = "FAIL"
				}
				fmt.Fprintf(streams.Out, "%s %-24s %s\n", label, result.Name, result.Detail)
				if result.Remediation != "" {
					fmt.Fprintf(streams.Out, "     Fix: %s\n", result.Remediation)
				}
			}
			if code := report.ExitCode(); code != 0 {
				return &ExitError{Code: code}
			}
			return nil
		},
	}
	command.Flags().BoolVar(&live, "live", false, "include Slack and model endpoint checks")
	return command
}

func newRunCommand(backend Backend) *cobra.Command {
	return &cobra.Command{
		Use:   "run",
		Short: "Run the Slack Socket Mode agent",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if err := backend.Run(command.Context()); err != nil {
				return &ExitError{Code: 1, Cause: err}
			}
			return nil
		},
	}
}

func newManifestCommand(backend Backend, streams Streams) *cobra.Command {
	var write bool
	command := &cobra.Command{
		Use:   "manifest",
		Short: "Render the configured Slack app manifest",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			content, path, err := backend.Manifest(command.Context(), write)
			if err != nil {
				return &ExitError{Code: 1, Cause: err}
			}
			if write {
				fmt.Fprintf(streams.Out, "Slack manifest written to %s\n", path)
				return nil
			}
			fmt.Fprint(streams.Out, content)
			if !strings.HasSuffix(content, "\n") {
				fmt.Fprintln(streams.Out)
			}
			return nil
		},
	}
	command.Flags().BoolVar(&write, "write", false, "write the managed local manifest")
	return command
}

func newVersionCommand(backend Backend, streams Streams) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version and Go runtime details",
		Args:  cobra.NoArgs,
		Run: func(*cobra.Command, []string) {
			fmt.Fprintln(streams.Out, backend.Version())
		},
	}
}
