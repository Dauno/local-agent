package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/usecase/bootstrap"
	"github.com/Dauno/slack-local-agent/internal/usecase/doctor"
)

type Backend interface {
	PrepareSetup(ctx context.Context) (bootstrap.Snapshot, bootstrap.Secrets, error)
	ApplySetup(ctx context.Context, snapshot bootstrap.Snapshot, identity bootstrap.Identity, access bootstrap.AccessControl, secrets bootstrap.Secrets) error
	Doctor(ctx context.Context, live bool) (doctor.Report, error)
	Run(ctx context.Context) error
	Manifest(ctx context.Context, write bool) (content, path string, err error)
	ResetState(ctx context.Context) error
	Version() string
}

// JobInspectionBackend is optional so existing embedders of the CLI backend
// remain valid while the concrete application exposes local jobs inspect.
type JobInspectionBackend interface {
	InspectJob(ctx context.Context, jobID string) (*domain.ExternalAgentJobInspection, error)
}

type JobReconciliationBackend interface {
	ReconcileJob(ctx context.Context, jobID string, expectedRevision int) (domain.ExternalAgentJobStatusView, error)
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
		newJobsCommand(backend, streams),
		newShimCommand(streams),
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
	var resetState bool
	command := &cobra.Command{
		Use:   "init",
		Short: "Create and configure local-agent artifacts",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if resetState {
				confirmed, err := NewPrompter(streams.In, streams.Out).Confirm("Eliminar permanentemente todo el estado local", false)
				if err != nil {
					return &ExitError{Code: 1, Cause: err}
				}
				if !confirmed {
					fmt.Fprintln(streams.Out, "Restablecimiento cancelado.")
					return nil
				}
				if err := backend.ResetState(command.Context()); err != nil {
					return &ExitError{Code: 1, Cause: err}
				}
				return nil
			}
			if err := runWizard(command.Context(), backend, NewPrompter(streams.In, streams.Out), streams.Out); err != nil {
				return &ExitError{Code: 1, Cause: err}
			}
			return nil
		},
	}
	command.Flags().BoolVar(&resetState, "reset-state", false, "destructively delete all local conversation, memory, session, and confirmation data")
	return command
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

func newJobsCommand(backend Backend, streams Streams) *cobra.Command {
	command := &cobra.Command{
		Use:   "jobs",
		Short: "Inspect durable external-agent jobs",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return command.Help()
		},
	}
	command.AddCommand(newJobsInspectCommand(backend, streams), newJobsReconcileCommand(backend, streams))
	return command
}

func newJobsReconcileCommand(backend Backend, streams Streams) *cobra.Command {
	var expectedRevision int
	var confirm bool
	command := &cobra.Command{
		Use:   "reconcile <job_id>",
		Short: "Reconcile a completion-unknown external-agent job",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			if !confirm {
				return &ExitError{Code: 1, Cause: errors.New("--confirm is required")}
			}
			reconciler, ok := backend.(JobReconciliationBackend)
			if !ok {
				return &ExitError{Code: 1, Cause: errors.New("jobs reconcile is unavailable")}
			}
			view, err := reconciler.ReconcileJob(command.Context(), args[0], expectedRevision)
			if err != nil {
				return &ExitError{Code: 1, Cause: err}
			}
			fmt.Fprintf(streams.Out, "job_id: %s\nstatus: %s\nstatus_revision: %d\nresult_available: %t\n", view.JobID, view.Status, view.StatusRevision, view.ResultAvailable)
			fmt.Fprintf(streams.Out, "acp_session_id: %s\n", inspectionSessionID(view.ACPSessionID))
			if view.ErrorCode != "" {
				fmt.Fprintf(streams.Out, "error_code: %s\n", view.ErrorCode)
			}
			fmt.Fprintln(streams.Out, "next_action: inspect the durable notification or Slack thread")
			return nil
		},
	}
	command.Flags().IntVar(&expectedRevision, "expect-revision", -1, "exact durable job status revision required for reconciliation")
	command.Flags().BoolVar(&confirm, "confirm", false, "confirm reconciliation of existing provider state")
	_ = command.MarkFlagRequired("expect-revision")
	return command
}

func newJobsInspectCommand(backend Backend, streams Streams) *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <job_id>",
		Short: "Inspect a durable external-agent job",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			inspector, ok := backend.(JobInspectionBackend)
			if !ok {
				return &ExitError{Code: 1, Cause: errors.New("jobs inspect is unavailable")}
			}
			view, err := inspector.InspectJob(command.Context(), args[0])
			if err != nil {
				return &ExitError{Code: 1, Cause: errors.New("could not inspect durable job")}
			}
			if view == nil {
				fmt.Fprintln(streams.Out, "job: not found")
				return nil
			}
			writeJobInspection(streams.Out, *view)
			return nil
		},
	}
}

func writeJobInspection(out io.Writer, view domain.ExternalAgentJobInspection) {
	fmt.Fprintf(out, "job_id: %s\n", view.JobID)
	fmt.Fprintf(out, "status: %s\n", view.Status)
	fmt.Fprintf(out, "status_revision: %d\n", view.StatusRevision)
	fmt.Fprintf(out, "acp_session_id: %s\n", inspectionSessionID(view.ACPSessionID))
	if view.Phase != "" || view.Health != "" || view.ProcessAlive != nil {
		fmt.Fprintf(out, "phase: %s\n", view.Phase)
		fmt.Fprintf(out, "health: %s\n", view.Health)
		fmt.Fprintf(out, "last_event: %s\n", view.LastEventKind)
		fmt.Fprintf(out, "last_acp_activity: %s\n", inspectionAge(view.LastTransportActivityAt))
		fmt.Fprintf(out, "prompt_elapsed: %s\n", inspectionDuration(view.PromptElapsedSeconds))
		fmt.Fprintf(out, "active_tools: %d\n", view.ActiveToolCount)
		fmt.Fprintf(out, "pending_permission: %t\n", view.PendingPermission)
		fmt.Fprintf(out, "process: %s\n", inspectionProcess(view.ProcessAlive))
		if view.StopReason != "" {
			fmt.Fprintf(out, "stop_reason: %s\n", view.StopReason)
		}
	}
	fmt.Fprintf(out, "finished_at: %s\n", inspectionTime(view.FinishedAt))
	if len(view.Deliveries) == 0 {
		fmt.Fprintln(out, "delivery_mode:")
		fmt.Fprintln(out, "notification_kind:")
		fmt.Fprintln(out, "publish_state:")
		fmt.Fprintln(out, "attempts: 0")
		fmt.Fprintln(out, "lease_owner:")
		fmt.Fprintln(out, "lease_owner_present: false")
		fmt.Fprintln(out, "lease_expiry:")
		fmt.Fprintln(out, "last_error_code:")
		fmt.Fprintln(out, "next_attempt_at:")
		fmt.Fprintln(out, "recovered_slack_ts:")
		return
	}
	for index, delivery := range view.Deliveries {
		if len(view.Deliveries) > 1 {
			fmt.Fprintf(out, "delivery_%d:\n", index+1)
		}
		fmt.Fprintf(out, "delivery_revision: %d\n", delivery.StatusRevision)
		fmt.Fprintf(out, "delivery_mode: %s\n", delivery.DeliveryMode)
		fmt.Fprintf(out, "notification_kind: %s\n", delivery.NotificationKind)
		fmt.Fprintf(out, "publish_state: %s\n", delivery.PublishState)
		fmt.Fprintf(out, "attempts: %d\n", delivery.Attempts)
		fmt.Fprintf(out, "lease_owner: %s\n", delivery.LeaseOwner)
		fmt.Fprintf(out, "lease_owner_present: %t\n", delivery.LeaseOwnerPresent)
		fmt.Fprintf(out, "lease_expiry: %s\n", inspectionTime(delivery.LeaseExpiry))
		fmt.Fprintf(out, "last_error_code: %s\n", delivery.LastErrorCode)
		fmt.Fprintf(out, "next_attempt_at: %s\n", inspectionTime(delivery.NextAttemptAt))
		fmt.Fprintf(out, "recovered_slack_ts: %s\n", delivery.RecoveredSlackTS)
		if delivery.DeliveryMode == domain.JobResultDeliveryFile {
			fmt.Fprintf(out, "upload_state: %s\n", delivery.UploadState)
			fmt.Fprintf(out, "slack_file_id_present: %t\n", delivery.SlackFileIDPresent)
		}
	}
}

func inspectionTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

// inspectionSessionID renders the complete session ID without truncation.
// An empty session renders as pending, never as an empty line.
func inspectionSessionID(sessionID string) string {
	if sessionID == "" {
		return "pending"
	}
	return sessionID
}

// inspectionProcess renders nullable process liveness. A missing runtime
// handle renders as unknown, never as dead.
func inspectionProcess(alive *bool) string {
	if alive == nil {
		return "unknown"
	}
	if *alive {
		return "alive"
	}
	return "dead"
}

func inspectionAge(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	elapsed := time.Since(value)
	if elapsed < 0 {
		elapsed = 0
	}
	return formatHumanDuration(elapsed)
}

func inspectionDuration(seconds int64) string {
	if seconds <= 0 {
		return ""
	}
	return formatHumanDuration(time.Duration(seconds) * time.Second)
}

func formatHumanDuration(value time.Duration) string {
	if value < time.Minute {
		return fmt.Sprintf("%ds", int(value/time.Second))
	}
	if value < time.Hour {
		return fmt.Sprintf("%dm %02ds", int(value/time.Minute), int(value/time.Second)%60)
	}
	return fmt.Sprintf("%dh %02dm", int(value/time.Hour), int(value/time.Minute)%60)
}
