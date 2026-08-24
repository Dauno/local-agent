package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/Dauno/slack-local-agent/internal/usecase/rollout"
)

// DatabaseUpgradeBackend is optional so existing embedders of Backend stay
// valid; the concrete application implements the preview/apply split the
// confirmation flow needs.
type DatabaseUpgradeBackend interface {
	PreviewDatabaseUpgrade(ctx context.Context, opts rollout.UpgradeOptions) (rollout.UpgradePreview, error)
	ApplyDatabaseUpgrade(ctx context.Context, opts rollout.UpgradeOptions, expected rollout.UpgradePreview) (rollout.UpgradeReport, error)
}

// DatabaseRollbackCheckBackend is optional for the same reason.
type DatabaseRollbackCheckBackend interface {
	CheckRollbackDrain(ctx context.Context) (rollout.SummaryDiscoveryDrainStatus, error)
}

const (
	databaseAlreadyCompleteText = "database already at schema v42; nothing to do"
	freshUpgradeSummaryFormat   = "will migrate v%d to v42; backup will be written under %s"
	adoptionSummaryFormat       = "database is already at v42 but was never rolled out through local-agent db upgrade; will record a baseline and cutoff now and back up first, under %s"
	resumeNeededSummaryText     = "database is at v42 with an incomplete rollout (postflight not yet passed); will re-run postflight"
	upgradeCancelledText        = "Actualizacion cancelada."
)

func newDBCommand(backend Backend, streams Streams) *cobra.Command {
	command := &cobra.Command{
		Use:   "db",
		Short: "Operate the local database schema",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return command.Help()
		},
	}
	command.AddCommand(newDBUpgradeCommand(backend, streams), newDBRollbackCheckCommand(backend, streams))
	return command
}

func newDBUpgradeCommand(backend Backend, streams Streams) *cobra.Command {
	var assumeYes bool
	var backupDir string
	command := &cobra.Command{
		Use:   "upgrade",
		Short: "Migrate the database to schema v42 through a verified backup",
		Long: "Classifies the durable database state, shows what will happen, and after " +
			"confirmation creates a verified backup before any write. A previously " +
			"interrupted run resumes from its durable state without repeating a completed row.",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			upgrader, ok := backend.(DatabaseUpgradeBackend)
			if !ok {
				return &ExitError{Code: 1, Cause: errors.New("db upgrade is unavailable")}
			}
			ctx := command.Context()
			opts := rollout.UpgradeOptions{BackupDir: backupDir}
			preview, err := upgrader.PreviewDatabaseUpgrade(ctx, opts)
			if err != nil {
				return &ExitError{Code: schemaRangeExitCode(err), Cause: err}
			}
			if preview.Kind == rollout.UpgradeAlreadyComplete {
				_, _ = fmt.Fprintln(streams.Out, databaseAlreadyCompleteText)
				return nil
			}
			printUpgradeSummary(streams.Out, preview)
			if !assumeYes && !confirmUpgrade(streams, preview) {
				_, _ = fmt.Fprintln(streams.Out, upgradeCancelledText)
				return nil
			}
			report, err := upgrader.ApplyDatabaseUpgrade(ctx, opts, preview)
			if err != nil {
				return &ExitError{Code: schemaRangeExitCode(err), Cause: err}
			}
			if !report.RolloutAdvanced {
				// The re-read under the lock found a different row than the
				// preview observed: another process advanced or replaced the
				// state concurrently. Nothing was mutated.
				_, _ = fmt.Fprintf(streams.Out, "database changed concurrently during db upgrade; it now classifies %s; nothing was done\n", upgradeKindLabel(report.Kind))
				return nil
			}
			if report.Backup.Path != "" {
				_, _ = fmt.Fprintf(streams.Out, "backup verified: %s\n", report.Backup.Path)
			}
			_, _ = fmt.Fprintf(streams.Out, "jobs completed without result identity: baseline %d, post %d\n", report.BaselineJobsWithoutIdentity, report.PostJobsWithoutIdentity)
			_, _ = fmt.Fprintf(streams.Out, "activations without content: baseline %d, post %d\n", report.BaselineActivationsWithoutContent, report.PostActivationsWithoutContent)
			_, _ = fmt.Fprintln(streams.Out, "run local-agent jobs quarantine-legacy-identity")
			return nil
		},
	}
	command.Flags().BoolVar(&assumeYes, "yes", false, "skip every confirmation prompt")
	command.Flags().StringVar(&backupDir, "backup-dir", "", "directory for the verified backup (default: the directory containing the configured database file)")
	return command
}

// confirmUpgrade renders the frozen prompt set for the previewed Kind.
// Every prompt is skippable only by --yes, which the caller already handled;
// a decline cancels before any lock is taken.
func confirmUpgrade(streams Streams, preview rollout.UpgradePreview) bool {
	prompter := NewPrompter(streams.In, streams.Out)
	confirm := func(label string) bool {
		confirmed, err := prompter.Confirm(label, false)
		if err != nil {
			return false
		}
		return confirmed
	}
	switch preview.Kind {
	case rollout.UpgradeFreshUpgrade:
		first := fmt.Sprintf("Aplicar la migracion de schema v%d a v42 sobre %s. Se creara un backup verificado antes de escribir. Confirmar.", preview.FromVersion, preview.DatabasePath)
		return confirm(first) && confirm("El proceso v33 desplegado no participa en el protocolo de bloqueo de este comando. Confirme que ese proceso esta detenido antes de continuar.")
	case rollout.UpgradeAdoption:
		first := fmt.Sprintf(adoptionSummaryFormat, preview.ResolvedBackupDir)
		return confirm(first) && confirm("Este comando no puede detectar si un binario que no implementa el bloqueo de este comando sigue escribiendo esta base de datos v42. Confirme que ningun proceso asi esta en ejecucion antes de continuar.")
	default:
		return confirm(resumeNeededSummaryText)
	}
}

func printUpgradeSummary(out io.Writer, preview rollout.UpgradePreview) {
	switch preview.Kind {
	case rollout.UpgradeFreshUpgrade:
		_, _ = fmt.Fprintf(out, freshUpgradeSummaryFormat+"\n", preview.FromVersion, preview.ResolvedBackupDir)
	case rollout.UpgradeAdoption:
		_, _ = fmt.Fprintf(out, adoptionSummaryFormat+"\n", preview.ResolvedBackupDir)
	default:
		_, _ = fmt.Fprintln(out, resumeNeededSummaryText)
	}
}

func upgradeKindLabel(kind rollout.UpgradeKind) string {
	switch kind {
	case rollout.UpgradeFreshUpgrade:
		return "FreshUpgrade"
	case rollout.UpgradeAdoption:
		return "Adoption"
	case rollout.UpgradeResumeNeeded:
		return "ResumeNeeded"
	default:
		return "AlreadyComplete"
	}
}

// schemaRangeExitCode maps out-of-range classifications to exit code 2; a
// retry of this exact command against this exact file can never succeed.
func schemaRangeExitCode(err error) int {
	if errors.Is(err, rollout.ErrFutureSchema) || errors.Is(err, rollout.ErrUnsupportedSourceSchema) {
		return 2
	}
	return 1
}

func newDBRollbackCheckCommand(backend Backend, streams Streams) *cobra.Command {
	return &cobra.Command{
		Use:   "rollback-check",
		Short: "Check the TRD 08 rollback drain precondition",
		Long: "Reports whether any session still has a pending context-summary discovery " +
			"marker, which blocks rolling back to a schema-v41-compatible binary at or before 3cfe091.",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			checker, ok := backend.(DatabaseRollbackCheckBackend)
			if !ok {
				return &ExitError{Code: 1, Cause: errors.New("db rollback-check is unavailable")}
			}
			status, err := checker.CheckRollbackDrain(command.Context())
			if err != nil {
				return &ExitError{Code: 1, Cause: err}
			}
			if status.Clear {
				_, _ = fmt.Fprintln(streams.Out, "rollback drain clear: 0 sessions have a pending discovery marker; safe to run a schema-v41-compatible binary at or before 3cfe091")
				return nil
			}
			_, _ = fmt.Fprintf(streams.Out, "rollback blocked: %d sessions have a pending discovery marker; let the current binary drain them or cancel them explicitly before rolling back to a binary at or before 3cfe091\n", len(status.PendingSessionIdentities))
			for _, identity := range status.PendingSessionIdentities {
				_, _ = fmt.Fprintln(streams.Out, identity)
			}
			return &ExitError{Code: 1}
		},
	}
}
