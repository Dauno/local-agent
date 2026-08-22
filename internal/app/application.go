package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Dauno/slack-local-agent/internal/adapter/envfile"
	"github.com/Dauno/slack-local-agent/internal/adapter/fsartifact"
	"github.com/Dauno/slack-local-agent/internal/adapter/fsproject"
	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/agentdef"
	"github.com/Dauno/slack-local-agent/internal/buildinfo"
	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/manifest"
	"github.com/Dauno/slack-local-agent/internal/usecase/bootstrap"
	"github.com/Dauno/slack-local-agent/internal/usecase/doctor"
	externalagent "github.com/Dauno/slack-local-agent/internal/usecase/externalagent"
	"github.com/Dauno/slack-local-agent/internal/usecase/rollout"
)

type Application struct {
	root          string
	logOutput     io.Writer
	forceShutdown chan struct{}
	forceOnce     sync.Once
	schemaLocker  rollout.SchemaLocker
	// schemaProbe, schemaBackupper, and schemaWriter are the private test
	// seams for the db upgrade flow; nil selects the SQLite implementations.
	schemaProbe     rollout.SchemaProbe
	schemaBackupper rollout.DatabaseBackupper
	schemaWriter    rollout.SchemaWriter
	// quarantineStore is the private test seam for the legacy identity
	// disposition; nil selects the SQLite implementation.
	quarantineStore rollout.LegacyIdentityQuarantineStore
	// schemaTrace is the private test seam for the mutation call order
	// (FIND-190): when non-nil it receives ordered "open-current"/"create"
	// markers to place next to the locker's own events. It is nil in
	// production and never a package-level global.
	schemaTrace func(string)
	openCurrent func(context.Context, string) (*adaptersqlite.Store, error)
	create      func(context.Context, string) (*adaptersqlite.Store, error)
}

func (a *Application) traceSchemaEvent(event string) {
	if a.schemaTrace != nil {
		a.schemaTrace(event)
	}
}

func (a *Application) openCurrentTraced(ctx context.Context, path string) (*adaptersqlite.Store, error) {
	a.traceSchemaEvent("open-current")
	open := a.openCurrent
	if open == nil {
		open = adaptersqlite.OpenCurrent
	}
	return open(ctx, path)
}

func (a *Application) createTraced(ctx context.Context, path string) (*adaptersqlite.Store, error) {
	a.traceSchemaEvent("create")
	create := a.create
	if create == nil {
		create = adaptersqlite.Create
	}
	return create(ctx, path)
}

func New(projectRoot string, logOutput io.Writer) (*Application, error) {
	if strings.TrimSpace(projectRoot) == "" {
		return nil, errors.New("project root is required")
	}
	root, err := fsproject.New().CanonicalRoot(projectRoot)
	if err != nil {
		return nil, err
	}
	if logOutput == nil {
		logOutput = io.Discard
	}
	return &Application{root: root, logOutput: logOutput, forceShutdown: make(chan struct{})}, nil
}

// schemaLock acquires the exclusive cross-process mutation lock for one
// database path through the configured locker, defaulting to the
// kernel-held file lock.
func (a *Application) schemaLock(databasePath string) (rollout.Lock, error) {
	locker := a.schemaLocker
	if locker == nil {
		locker = adaptersqlite.FileSchemaLocker{}
	}
	return locker.AcquireExclusive(databasePath)
}

const (
	schemaBehindMessage     = "database schema is behind this binary's v41; run local-agent db upgrade first"
	mutationLockHeldMessage = "another local-agent process is using the database; wait for it to finish"
)

// lockHeldError carries the shared operator text while keeping
// errors.Is(err, rollout.ErrMutationLockHeld) true for callers that need
// the typed distinction.
type lockHeldError struct{}

func (lockHeldError) Error() string { return mutationLockHeldMessage }
func (lockHeldError) Unwrap() error { return rollout.ErrMutationLockHeld }

// schemaLockFailure maps locker failures to the operator-facing texts every
// mutable command shares.
func schemaLockFailure(err error) error {
	switch {
	case errors.Is(err, rollout.ErrMutationLockHeld):
		return lockHeldError{}
	case errors.Is(err, rollout.ErrMutationLockUnsupported):
		return fmt.Errorf("%w", rollout.ErrMutationLockUnsupported)
	default:
		return fmt.Errorf("acquire database mutation lock: %w", err)
	}
}

// schemaOpenFailure maps OpenCurrent rejections to the shared operator
// texts; every other error keeps its own shape. A schema inside [33, 40]
// keeps the upgrade-first message because db upgrade accepts it; a schema
// outside [33, 41] maps to the terminal message that never recommends
// db upgrade for a file db upgrade itself refuses (FIND-179).
func schemaOpenFailure(err error) error {
	var upgrade *adaptersqlite.SchemaUpgradeRequiredError
	if errors.As(err, &upgrade) {
		if upgrade.Found >= rollout.MinSourceVersion && upgrade.Found <= rollout.MaxSourceVersion {
			return errors.New(schemaBehindMessage)
		}
		return newTerminalSchemaError(rollout.ErrUnsupportedSourceSchema, upgrade.Found)
	}
	var future *adaptersqlite.FutureSchemaError
	if errors.As(err, &future) {
		return newTerminalSchemaError(rollout.ErrFutureSchema, future.Found)
	}
	return err
}

// ForceShutdown skips the configured drain period while preserving durable
// worker cleanup and state classification.
func (a *Application) ForceShutdown() {
	if a == nil {
		return
	}
	a.forceOnce.Do(func() { close(a.forceShutdown) })
}

func (a *Application) PrepareSetup(ctx context.Context) (bootstrap.Snapshot, bootstrap.Secrets, error) {
	service, err := a.bootstrapService()
	if err != nil {
		return bootstrap.Snapshot{}, bootstrap.Secrets{}, err
	}
	snapshot, err := service.EnsureBaseArtifacts(ctx, a.root)
	if err != nil {
		return bootstrap.Snapshot{}, bootstrap.Secrets{}, err
	}
	if err := os.MkdirAll(snapshot.Paths.ArtifactDir, 0o700); err != nil {
		return bootstrap.Snapshot{}, bootstrap.Secrets{}, fmt.Errorf("create ACP artifact directory: %w", err)
	}
	values, err := envfile.NewResolver(snapshot.Paths.EnvFile).Resolve(
		snapshot.Config.Model.APIKeyEnv,
		bootstrap.SlackBotTokenEnv,
		bootstrap.SlackAppTokenEnv,
	)
	if err != nil {
		return bootstrap.Snapshot{}, bootstrap.Secrets{}, err
	}
	return snapshot, bootstrap.Secrets{
		ModelAPIKey:   values[snapshot.Config.Model.APIKeyEnv],
		SlackBotToken: values[bootstrap.SlackBotTokenEnv],
		SlackAppToken: values[bootstrap.SlackAppTokenEnv],
	}, nil
}

func (a *Application) ApplySetup(
	ctx context.Context,
	snapshot bootstrap.Snapshot,
	identity bootstrap.Identity,
	access bootstrap.AccessControl,
	secrets bootstrap.Secrets,
) error {
	service, err := a.bootstrapService()
	if err != nil {
		return err
	}
	_, err = service.ApplyConfirmedUpdates(ctx, snapshot, identity, access, secrets)
	return err
}

func (a *Application) Doctor(ctx context.Context, includeLive bool) (doctor.Report, error) {
	configPath, err := config.ConfigPath(a.root)
	if err != nil {
		return doctor.Report{}, err
	}
	dependencies := doctor.Dependencies{
		ConfigPath:      configPath,
		Secrets:         envfile.NewResolver(filepath.Join(a.root, config.DefaultEnvFile)),
		Database:        databaseChecker{},
		Artifacts:       artifactChecker{},
		Jobs:            jobStoreChecker{},
		CLI:             cliProviderChecker{},
		ACP:             acpProviderChecker{},
		Counter:         counterChecker{},
		Knowledge:       knowledgeChecker{},
		ResultRetention: resultRetentionChecker{},
		ResultAnalysis:  resultAnalysisChecker{},
		SQLiteRuntime:   sqliteRuntimeChecker{},
		RecoverableRefs: recoverableReferenceChecker{},
	}
	if includeLive {
		dependencies.Live = liveChecker{}
	}
	service, err := doctor.New(dependencies)
	if err != nil {
		return doctor.Report{}, err
	}
	return service.Run(ctx, includeLive), nil
}

func (a *Application) Manifest(ctx context.Context, write bool) (string, string, error) {
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	configPath, err := config.ConfigPath(a.root)
	if err != nil {
		return "", "", err
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", "", errors.New("Configuration not found. Run: local-agent init")
		}
		return "", "", err
	}
	paths, err := cfg.ResolvePaths(a.root)
	if err != nil {
		return "", "", err
	}
	durableACP := false
	defs, err := agentdef.Load(paths.StateDir)
	if err != nil {
		return "", "", fmt.Errorf("load agent definitions for Slack manifest: %w", err)
	}
	if defs != nil {
		for _, definition := range defs.Agents {
			if definition.AgentClass == "AcpAgent" && definition.ExecutionMode == agentdef.ExecutionModeDurableJob {
				durableACP = true
				break
			}
		}
	}
	rendered, err := manifest.Render(manifest.Identity{
		AppName: cfg.Slack.AppName, BotDisplayName: cfg.Slack.BotDisplayName, CanvasesEnabled: cfg.Canvases.Enabled, ExportsEnabled: cfg.Exports.Enabled, DurableACPEnabled: durableACP,
	})
	if err != nil {
		return "", "", err
	}
	if write {
		files := fsproject.New()
		if err := files.WriteBatch(ctx,
			map[string][]byte{paths.ManifestFile: []byte(rendered)},
			map[string]os.FileMode{paths.ManifestFile: 0o644},
			nil,
		); err != nil {
			return "", "", fmt.Errorf("write Slack manifest: %w", err)
		}
	}
	return rendered, paths.ManifestFile, nil
}

func (*Application) Version() string { return buildinfo.String() }

// InspectJob is a local, read-only administrative query. It deliberately
// avoids runtime composition, Slack credentials, model calls, and migrations.
func (a *Application) InspectJob(ctx context.Context, jobID string) (*domain.ExternalAgentJobInspection, error) {
	if strings.TrimSpace(jobID) == "" {
		return nil, errors.New("job ID is required")
	}
	configPath, err := config.ConfigPath(a.root)
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("configuration not found")
		}
		return nil, err
	}
	paths, err := cfg.ResolvePaths(a.root)
	if err != nil {
		return nil, err
	}
	store, err := adaptersqlite.OpenReadOnly(ctx, paths.DatabaseFile)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	jobStore := adaptersqlite.NewExternalAgentJobStore(store)
	view, err := jobStore.InspectJob(ctx, jobID)
	if err != nil || view == nil {
		return view, err
	}
	// Health and prompt elapsed are derived at read time only when the
	// durable projection exists; a queued or pre-session job has no
	// projection and must never be assigned fabricated health. The read-only
	// CLI process has no runtime process handle, so liveness is unknown and
	// can never claim a process is dead.
	if view.Phase != "" {
		now := time.Now().UTC()
		view.Health = externalagent.DeriveProgressHealth(domain.ExternalAgentJobProgress{
			Phase: view.Phase, LastTransportActivityAt: view.LastTransportActivityAt,
			LastMeaningfulProgressAt: view.LastMeaningfulProgressAt, PromptStartedAt: view.PromptStartedAt,
		}, now, time.Duration(cfg.ACP.ProgressWarningSeconds)*time.Second, nil, isTerminalInspectionStatus(view.Status))
		if !view.PromptStartedAt.IsZero() {
			elapsed := now.Sub(view.PromptStartedAt)
			if elapsed < 0 {
				elapsed = 0
			}
			view.PromptElapsedSeconds = int64(elapsed / time.Second)
		}
	}
	return view, nil
}

func isTerminalInspectionStatus(status domain.ExternalAgentJobStatus) bool {
	switch status {
	case domain.JobCompleted, domain.JobFailed, domain.JobCancelled, domain.JobAbandoned, domain.JobCompletionUnknown:
		return true
	default:
		return false
	}
}

// ResetState implements the destructive init --reset-state command.
// It deletes the SQLite database and generated memory projections.
// Slack messages and remote sandbox resources are not affected.
func (a *Application) ResetState(ctx context.Context) error {
	configPath, err := config.ConfigPath(a.root)
	if err != nil {
		return err
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errors.New("configuration not found — nothing to reset")
		}
		return fmt.Errorf("load config: %w", err)
	}
	paths, err := cfg.ResolvePaths(a.root)
	if err != nil {
		return fmt.Errorf("resolve paths: %w", err)
	}

	dbPath := paths.DatabaseFile
	// The lock is taken before the existence check so a concurrent reset or
	// mutator can never interleave with the destructive replacement below.
	lock, err := a.schemaLock(dbPath)
	if err != nil {
		return schemaLockFailure(err)
	}
	defer func() { _ = lock.Release() }()

	if _, statErr := os.Stat(dbPath); errors.Is(statErr, os.ErrNotExist) {
		return errors.New("no existing database found — nothing to reset")
	}

	if err := os.Remove(dbPath); err != nil {
		return fmt.Errorf("delete database %s: %w", dbPath, err)
	}
	// A confirmed reset may replace an outdated database outright: Create
	// builds the current schema directly, and no OpenCurrent gate applies to
	// this destructive flow.
	store, err := a.createTraced(ctx, dbPath)
	if err != nil {
		return fmt.Errorf("initialize fresh database: %w", err)
	}
	if err := store.Close(); err != nil {
		return fmt.Errorf("close fresh database: %w", err)
	}

	// Clean up memory projections if they exist.
	memoryDir := filepath.Join(a.root, ".local-agent", "memory")
	if _, statErr := os.Stat(memoryDir); statErr == nil {
		if err := os.RemoveAll(memoryDir); err != nil {
			return fmt.Errorf("delete memory projections: %w", err)
		}
	}

	fmt.Fprintf(a.logOutput, "State reset complete. Fresh database initialized and memory projections deleted.\n")
	return nil
}

func (a *Application) bootstrapService() (*bootstrap.Service, error) {
	return bootstrap.New(fsproject.New(), bootstrap.DatabaseInitializerFunc(func(ctx context.Context, path string) error {
		// Lock first; every database decision below happens under it.
		lock, err := a.schemaLock(path)
		if err != nil {
			return schemaLockFailure(err)
		}
		defer func() { _ = lock.Release() }()

		// Rollout-completeness preflight (checkpoint 5): an existing database
		// must already sit on a completed rollout before the write-capable
		// opener runs. A missing file skips the gate and takes the create path
		// below, which records its own complete rollout state at creation.
		a.traceSchemaEvent("preflight")
		if err := a.requireRolloutComplete(ctx, path); err != nil && !errors.Is(err, adaptersqlite.ErrDatabaseNotFound) {
			return rolloutPreflightFailure(err)
		}

		store, err := a.openCurrentTraced(ctx, path)
		switch {
		case err == nil:
			return store.Close()
		case errors.Is(err, adaptersqlite.ErrDatabaseNotFound):
			// Brand-new file: the full create-and-migrate chain runs under
			// the same lock.
			created, createErr := a.createTraced(ctx, path)
			if errors.Is(createErr, os.ErrExist) {
				// Another initializer won the O_EXCL race; revalidate the
				// winner's version instead of migrating it implicitly.
				raced, openErr := a.openCurrentTraced(ctx, path)
				if openErr != nil {
					return schemaOpenFailure(openErr)
				}
				return raced.Close()
			}
			if createErr != nil {
				return createErr
			}
			return created.Close()
		default:
			// An existing outdated database never reaches Create or
			// OpenExisting: init reports the upgrade requirement and stops.
			return schemaOpenFailure(err)
		}
	}), bootstrap.SecretEditorFunc(envfile.Render))
}

type databaseChecker struct{}

type artifactChecker struct{}

func (artifactChecker) CheckArtifactStore(ctx context.Context, path string, maxBytes int) error {
	return fsartifact.CheckDirectory(ctx, path, int64(maxBytes))
}

type jobStoreChecker struct{}

func (jobStoreChecker) CheckExternalAgentJobs(ctx context.Context, path string) error {
	store, err := adaptersqlite.OpenReadOnly(ctx, path)
	if err != nil {
		return err
	}
	defer store.Close()
	return store.CheckExternalAgentJobStore(ctx)
}

func (jobStoreChecker) CheckExternalAgentActivationHealth(ctx context.Context, path string) (domain.ExternalAgentJobActivationHealth, error) {
	store, err := adaptersqlite.OpenReadOnly(ctx, path)
	if err != nil {
		return domain.ExternalAgentJobActivationHealth{}, err
	}
	defer store.Close()
	jobs := adaptersqlite.NewExternalAgentJobStore(store)
	return jobs.ActivationHealth(ctx, time.Now().UTC(), 5*time.Minute)
}

func (jobStoreChecker) CheckExternalAgentResultIdentityHealth(ctx context.Context, path string) (domain.ExternalAgentJobIdentityHealth, error) {
	store, err := adaptersqlite.OpenReadOnly(ctx, path)
	if err != nil {
		return domain.ExternalAgentJobIdentityHealth{}, err
	}
	defer store.Close()
	jobs := adaptersqlite.NewExternalAgentJobStore(store)
	return jobs.IdentityHealth(ctx)
}

type knowledgeChecker struct{}

func (knowledgeChecker) CheckKnowledgeRetrievalState(ctx context.Context, path string) (domain.KnowledgeRetrievalHealth, error) {
	store, err := adaptersqlite.OpenReadOnly(ctx, path)
	if err != nil {
		return domain.KnowledgeRetrievalHealth{}, err
	}
	defer store.Close()
	return store.CheckKnowledgeRetrievalState(ctx)
}

type resultRetentionChecker struct{}

func (resultRetentionChecker) CheckResultRetention(ctx context.Context, path string, ages domain.ResultRetentionAges, now time.Time) (domain.ResultRetentionHealth, error) {
	store, err := adaptersqlite.OpenReadOnly(ctx, path)
	if err != nil {
		return domain.ResultRetentionHealth{}, err
	}
	defer store.Close()
	return store.CheckResultRetention(ctx, ages, now)
}

type resultAnalysisChecker struct{}

func (resultAnalysisChecker) CheckResultAnalysisState(ctx context.Context, path string) (domain.ResultAnalysisHealth, error) {
	store, err := adaptersqlite.OpenReadOnly(ctx, path)
	if err != nil {
		return domain.ResultAnalysisHealth{}, err
	}
	defer store.Close()
	return store.CheckResultAnalysisState(ctx)
}

// Every doctor checker opens the configured database read-only (TRD 09
// checkpoint 2): an offline inspection can never migrate or otherwise change
// database state as a side effect.
type sqliteRuntimeChecker struct{}

func (sqliteRuntimeChecker) CheckSQLiteRuntime(ctx context.Context, path string) (domain.SQLiteRuntimeHealth, error) {
	store, err := adaptersqlite.OpenReadOnly(ctx, path)
	if err != nil {
		return domain.SQLiteRuntimeHealth{}, err
	}
	defer store.Close()
	return store.CheckSQLiteRuntime(ctx)
}

type recoverableReferenceChecker struct{}

func (recoverableReferenceChecker) CheckRecoverableReferenceHealth(ctx context.Context, path string) (domain.RecoverableReferenceHealth, error) {
	store, err := adaptersqlite.OpenReadOnly(ctx, path)
	if err != nil {
		return domain.RecoverableReferenceHealth{}, err
	}
	defer store.Close()
	return store.CheckRecoverableReferenceHealth(ctx)
}

// CheckDatabase is a pure read (TRD 09 checkpoint 2): it opens the database
// read-only and asserts PRAGMA integrity_check returns ok. Write capability
// is no longer doctor's concern; a mutable command's own OpenCurrent call is
// where write access is exercised, at the moment it is needed.
func (databaseChecker) CheckDatabase(ctx context.Context, path string) error {
	store, err := adaptersqlite.OpenReadOnly(ctx, path)
	if err != nil {
		return err
	}
	defer store.Close()
	var outcome string
	if err := store.DB().QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&outcome); err != nil {
		return fmt.Errorf("SQLite integrity check: %w", err)
	}
	if outcome != "ok" {
		return fmt.Errorf("SQLite integrity check reported %q", outcome)
	}
	return nil
}
