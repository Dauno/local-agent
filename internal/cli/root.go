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
	"github.com/Dauno/slack-local-agent/internal/usecase/rollout"
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

// JobClosureBackend is optional so existing embedders of the CLI backend
// remain valid while the concrete application exposes local jobs close.
type JobClosureBackend interface {
	CloseJob(ctx context.Context, jobID string, expectedRevision int) (domain.ExternalAgentJobStatusView, error)
}

// KnowledgeIndexRebuildBackend is optional so existing embedders of the CLI
// backend remain valid while the concrete application exposes the bounded
// reconstructible-knowledge-index rebuild command.
type KnowledgeIndexRebuildBackend interface {
	RebuildKnowledgeIndexes(ctx context.Context) (domain.KnowledgeIndexRebuildResult, error)
}

// LegacyIdentityQuarantinePreviewBackend is optional so existing embedders of
// the CLI backend remain valid while the concrete application exposes the
// read-only legacy identity quarantine preview.
type LegacyIdentityQuarantinePreviewBackend interface {
	PreviewLegacyIdentityQuarantine(ctx context.Context) (rollout.LegacyIdentityQuarantinePreview, error)
}

// LegacyIdentityQuarantineApplyBackend is optional for the same reason; it
// carries only the confirmed apply so a preview-only embedder never sees a
// mutating method.
type LegacyIdentityQuarantineApplyBackend interface {
	ApplyLegacyIdentityQuarantine(ctx context.Context, expected rollout.LegacyIdentityQuarantinePreview) (rollout.LegacyIdentityQuarantineReport, error)
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
		newKnowledgeCommand(backend, streams),
		newDBCommand(backend, streams),
	)
	return root, nil
}

func Execute(ctx context.Context, root *cobra.Command, args []string, stderr io.Writer) int {
	root.SetArgs(args)
	err := root.ExecuteContext(ctx)
	if err == nil {
		return 0
	}
	if exit, ok := errors.AsType[*ExitError](err); ok {
		if exit.Cause != nil {
			_, _ = fmt.Fprintln(stderr, exit.Cause)
		}
		return exit.Code
	}
	_, _ = fmt.Fprintln(stderr, err)
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
					_, _ = fmt.Fprintln(streams.Out, "Restablecimiento cancelado.")
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
				switch result.Status {
				case doctor.StatusFail:
					label = "FAIL"
				case doctor.StatusSkipped:
					label = "SKIP"
				}
				_, _ = fmt.Fprintf(streams.Out, "%s %-24s %s\n", label, result.Name, result.Detail)
				if result.Remediation != "" {
					_, _ = fmt.Fprintf(streams.Out, "     Fix: %s\n", result.Remediation)
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
				_, _ = fmt.Fprintf(streams.Out, "Slack manifest written to %s\n", path)
				return nil
			}
			_, _ = fmt.Fprint(streams.Out, content)
			if !strings.HasSuffix(content, "\n") {
				_, _ = fmt.Fprintln(streams.Out)
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
			_, _ = fmt.Fprintln(streams.Out, backend.Version())
		},
	}
}

func newKnowledgeCommand(backend Backend, streams Streams) *cobra.Command {
	command := &cobra.Command{
		Use:   "knowledge",
		Short: "Operate the scoped knowledge catalog",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return command.Help()
		},
	}
	command.AddCommand(newKnowledgeRebuildIndexCommand(backend, streams))
	return command
}

func newKnowledgeRebuildIndexCommand(backend Backend, streams Streams) *cobra.Command {
	return &cobra.Command{
		Use:   "rebuild-index",
		Short: "Rebuild the reconstructible knowledge lexical index",
		Long: "Clears the lexical FTS index and re-enqueues every truth identity for reindexing. " +
			"It never touches knowledge_claims, knowledge_preferences, knowledge_documents, or any other truth table. " +
			"Reindexing then drains through the running agent's normal lexical worker poll loop.",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			rebuilder, ok := backend.(KnowledgeIndexRebuildBackend)
			if !ok {
				return &ExitError{Code: 1, Cause: errors.New("knowledge rebuild-index is unavailable")}
			}
			result, err := rebuilder.RebuildKnowledgeIndexes(command.Context())
			if err != nil {
				return &ExitError{Code: 1, Cause: err}
			}
			if result.LexicalRebuilt {
				_, _ = fmt.Fprintln(streams.Out, "Lexical index cleared and re-enqueued. Reindexing will drain through the running agent's normal lexical worker poll loop.")
			}
			if result.EmbeddingSkippedReason != "" {
				_, _ = fmt.Fprintf(streams.Out, "Embedding index: %s\n", result.EmbeddingSkippedReason)
			}
			return nil
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
	command.AddCommand(
		newJobsInspectCommand(backend, streams),
		newJobsReconcileCommand(backend, streams),
		newJobsCloseCommand(backend, streams),
		newJobsQuarantineLegacyIdentityCommand(backend, streams),
	)
	return command
}

// Frozen invariant predicate texts the preview prints next to the counts.
// They describe exactly what the command matches and marks; no row value is
// ever printed.
const (
	quarantineJobsPredicateText        = "jobs predicate: status = 'completed' AND result identity incomplete (result_bytes <= 0 OR result_sha256 is not 64 lowercase hex chars) AND created_at <= cutoff"
	quarantineActivationsPredicateText = "activations predicate: terminal_status = 'completed' AND content_bytes <= 0 AND last_error_code = '' AND created_at <= cutoff"
)

func newJobsQuarantineLegacyIdentityCommand(backend Backend, streams Streams) *cobra.Command {
	var apply bool
	var expectJobs int
	var expectActivations int
	var assumeYes bool
	command := &cobra.Command{
		Use:   "quarantine-legacy-identity",
		Short: "Mark historical jobs and activations without result identity as informational legacy rows",
		Long: "Previews how many completed external-agent jobs and activations carry no result identity " +
			"and predate the frozen rollout cutoff. With --apply it stamps those exact rows with the " +
			"informational legacy markers inside one checked transaction. It never generates or repairs content.",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			ctx := command.Context()
			if !apply {
				previewer, ok := backend.(LegacyIdentityQuarantinePreviewBackend)
				if !ok {
					return &ExitError{Code: 1, Cause: errors.New("jobs quarantine-legacy-identity is unavailable")}
				}
				preview, err := previewer.PreviewLegacyIdentityQuarantine(ctx)
				if err != nil {
					return &ExitError{Code: 1, Cause: err}
				}
				printQuarantinePreview(streams.Out, preview)
				return nil
			}
			if !command.Flags().Changed("expect-jobs-matched") {
				return &ExitError{Code: 1, Cause: errors.New("--expect-jobs-matched is required with --apply")}
			}
			if !command.Flags().Changed("expect-activations-matched") {
				return &ExitError{Code: 1, Cause: errors.New("--expect-activations-matched is required with --apply")}
			}
			applier, ok := backend.(LegacyIdentityQuarantineApplyBackend)
			if !ok {
				return &ExitError{Code: 1, Cause: errors.New("jobs quarantine-legacy-identity --apply is unavailable")}
			}
			if !assumeYes {
				prompt := fmt.Sprintf("Marcar %d jobs y %d activations legacy sin identidad de resultado como excepcion informativa. No se genera ni repara contenido.", expectJobs, expectActivations)
				confirmed, err := NewPrompter(streams.In, streams.Out).Confirm(prompt, false)
				if err != nil {
					return &ExitError{Code: 1, Cause: err}
				}
				if !confirmed {
					_, _ = fmt.Fprintln(streams.Out, "Operacion cancelada.")
					return nil
				}
			}
			expected := rollout.LegacyIdentityQuarantinePreview{JobsMatched: expectJobs, ActivationsMatched: expectActivations}
			report, err := applier.ApplyLegacyIdentityQuarantine(ctx, expected)
			if err != nil {
				return &ExitError{Code: 1, Cause: err}
			}
			if report.AlreadyApplied {
				_, _ = fmt.Fprintf(streams.Out, "already_applied: true\napplied_at: %s\n", inspectionTime(report.AppliedAt))
				return nil
			}
			_, _ = fmt.Fprintf(streams.Out, "jobs_marked: %d\nactivations_marked: %d\napplied_at: %s\n", report.JobsMarked, report.ActivationsMarked, inspectionTime(report.AppliedAt))
			return nil
		},
	}
	command.Flags().BoolVar(&apply, "apply", false, "mark the matched rows instead of only previewing them")
	command.Flags().IntVar(&expectJobs, "expect-jobs-matched", -1, "exact job count a fresh preview must reproduce before any write")
	command.Flags().IntVar(&expectActivations, "expect-activations-matched", -1, "exact activation count a fresh preview must reproduce before any write")
	command.Flags().BoolVar(&assumeYes, "yes", false, "skip the confirmation prompt")
	return command
}

// printQuarantinePreview renders the content-free preview. A completed
// disposition reports already_applied with zero counts and never re-runs the
// match queries.
func printQuarantinePreview(out io.Writer, preview rollout.LegacyIdentityQuarantinePreview) {
	if preview.AlreadyApplied {
		_, _ = fmt.Fprintf(out, "already_applied: true\napplied_at: %s\njobs_matched: 0\nactivations_matched: 0\n", inspectionTime(preview.AppliedAt))
		return
	}
	_, _ = fmt.Fprintf(out, "cutoff: %s\n", inspectionTime(preview.Cutoff))
	_, _ = fmt.Fprintf(out, "jobs_matched: %d\n", preview.JobsMatched)
	_, _ = fmt.Fprintf(out, "activations_matched: %d\n", preview.ActivationsMatched)
	_, _ = fmt.Fprintln(out, quarantineJobsPredicateText)
	_, _ = fmt.Fprintln(out, quarantineActivationsPredicateText)
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
			_, _ = fmt.Fprintf(streams.Out, "job_id: %s\nstatus: %s\nstatus_revision: %d\nresult_available: %t\n", view.JobID, view.Status, view.StatusRevision, view.ResultAvailable)
			_, _ = fmt.Fprintf(streams.Out, "session_id: %s\n", inspectionSessionID(view.ExternalAgentSessionID))
			if view.ErrorCode != "" {
				_, _ = fmt.Fprintf(streams.Out, "error_code: %s\n", view.ErrorCode)
			}
			_, _ = fmt.Fprintln(streams.Out, "next_action: inspect the durable notification or Slack thread")
			return nil
		},
	}
	command.Flags().IntVar(&expectedRevision, "expect-revision", -1, "exact durable job status revision required for reconciliation")
	command.Flags().BoolVar(&confirm, "confirm", false, "confirm reconciliation of existing provider state")
	_ = command.MarkFlagRequired("expect-revision")
	return command
}

func newJobsCloseCommand(backend Backend, streams Streams) *cobra.Command {
	var expectedRevision int
	var confirm bool
	command := &cobra.Command{
		Use:   "close <job_id>",
		Short: "Close a completion-unknown external-agent job without resuming it",
		Long: "Marks a completion-unknown job as abandoned. Use it after you inspect the " +
			"external state by hand and decide that no recovery is needed. The command " +
			"asserts nothing about that state: it only stops the job from waiting. " +
			"To learn what the agent actually did, use jobs reconcile instead.",
		Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			if !confirm {
				return &ExitError{Code: 1, Cause: errors.New("--confirm is required")}
			}
			closer, ok := backend.(JobClosureBackend)
			if !ok {
				return &ExitError{Code: 1, Cause: errors.New("jobs close is unavailable")}
			}
			view, err := closer.CloseJob(command.Context(), args[0], expectedRevision)
			if err != nil {
				return &ExitError{Code: 1, Cause: err}
			}
			_, _ = fmt.Fprintf(streams.Out, "job_id: %s\nstatus: %s\nstatus_revision: %d\n", view.JobID, view.Status, view.StatusRevision)
			if view.ErrorCode != "" {
				_, _ = fmt.Fprintf(streams.Out, "error_code: %s\n", view.ErrorCode)
			}
			_, _ = fmt.Fprintln(streams.Out, "next_action: external state was not asserted to be reverted")
			return nil
		},
	}
	command.Flags().IntVar(&expectedRevision, "expect-revision", -1, "exact durable job status revision required for closure")
	command.Flags().BoolVar(&confirm, "confirm", false, "confirm that external state needs no recovery")
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
				_, _ = fmt.Fprintln(streams.Out, "job: not found")
				return nil
			}
			writeJobInspection(streams.Out, *view)
			return nil
		},
	}
}

func writeJobInspection(out io.Writer, view domain.ExternalAgentJobInspection) {
	_, _ = fmt.Fprintf(out, "job_id: %s\n", view.JobID)
	if view.Provider != "" {
		_, _ = fmt.Fprintf(out, "provider: %s\nprofile: %s\n", view.Provider, view.Profile)
	}
	_, _ = fmt.Fprintf(out, "status: %s\n", view.Status)
	_, _ = fmt.Fprintf(out, "status_revision: %d\n", view.StatusRevision)
	_, _ = fmt.Fprintf(out, "session_id: %s\n", inspectionSessionID(view.ExternalAgentSessionID))
	_, _ = fmt.Fprintf(out, "transcript_path: %s\n", inspectionTranscriptPath(view.TranscriptPath))
	if view.Phase != "" || view.Health != "" || view.ProcessAlive != nil {
		_, _ = fmt.Fprintf(out, "phase: %s\n", view.Phase)
		_, _ = fmt.Fprintf(out, "health: %s\n", view.Health)
		_, _ = fmt.Fprintf(out, "last_event: %s\n", view.LastEventKind)
		_, _ = fmt.Fprintf(out, "last_acp_activity: %s\n", inspectionAge(view.LastTransportActivityAt))
		_, _ = fmt.Fprintf(out, "prompt_elapsed: %s\n", inspectionDuration(view.PromptElapsedSeconds))
		_, _ = fmt.Fprintf(out, "active_tools: %d\n", view.ActiveToolCount)
		_, _ = fmt.Fprintf(out, "pending_permission: %t\n", view.PendingPermission)
		_, _ = fmt.Fprintf(out, "process: %s\n", inspectionProcess(view.ProcessAlive))
		if view.StopReason != "" {
			_, _ = fmt.Fprintf(out, "stop_reason: %s\n", view.StopReason)
		}
	}
	_, _ = fmt.Fprintf(out, "finished_at: %s\n", inspectionTime(view.FinishedAt))
	if len(view.Deliveries) == 0 {
		_, _ = fmt.Fprintln(out, "delivery_mode:")
		_, _ = fmt.Fprintln(out, "notification_kind:")
		_, _ = fmt.Fprintln(out, "publish_state:")
		_, _ = fmt.Fprintln(out, "attempts: 0")
		_, _ = fmt.Fprintln(out, "lease_owner:")
		_, _ = fmt.Fprintln(out, "lease_owner_present: false")
		_, _ = fmt.Fprintln(out, "lease_expiry:")
		_, _ = fmt.Fprintln(out, "last_error_code:")
		_, _ = fmt.Fprintln(out, "next_attempt_at:")
		_, _ = fmt.Fprintln(out, "recovered_slack_ts:")
		return
	}
	for index, delivery := range view.Deliveries {
		if len(view.Deliveries) > 1 {
			_, _ = fmt.Fprintf(out, "delivery_%d:\n", index+1)
		}
		_, _ = fmt.Fprintf(out, "delivery_revision: %d\n", delivery.StatusRevision)
		_, _ = fmt.Fprintf(out, "delivery_mode: %s\n", delivery.DeliveryMode)
		_, _ = fmt.Fprintf(out, "notification_kind: %s\n", delivery.NotificationKind)
		_, _ = fmt.Fprintf(out, "publish_state: %s\n", delivery.PublishState)
		_, _ = fmt.Fprintf(out, "attempts: %d\n", delivery.Attempts)
		_, _ = fmt.Fprintf(out, "lease_owner: %s\n", delivery.LeaseOwner)
		_, _ = fmt.Fprintf(out, "lease_owner_present: %t\n", delivery.LeaseOwnerPresent)
		_, _ = fmt.Fprintf(out, "lease_expiry: %s\n", inspectionTime(delivery.LeaseExpiry))
		_, _ = fmt.Fprintf(out, "last_error_code: %s\n", delivery.LastErrorCode)
		_, _ = fmt.Fprintf(out, "next_attempt_at: %s\n", inspectionTime(delivery.NextAttemptAt))
		_, _ = fmt.Fprintf(out, "recovered_slack_ts: %s\n", delivery.RecoveredSlackTS)
		if delivery.DeliveryMode == domain.JobResultDeliveryFile {
			_, _ = fmt.Fprintf(out, "upload_state: %s\n", delivery.UploadState)
			_, _ = fmt.Fprintf(out, "slack_file_id_present: %t\n", delivery.SlackFileIDPresent)
		}
	}
}

func inspectionTranscriptPath(value string) string {
	if value == "" {
		return "unavailable"
	}
	return value
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
	elapsed := max(time.Since(value), 0)
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
