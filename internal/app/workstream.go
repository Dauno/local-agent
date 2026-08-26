package app

import (
	"errors"
	"fmt"

	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	workstreamusecase "github.com/Dauno/slack-local-agent/internal/usecase/workstream"
)

// composeWorkstream is the pure composition step for the workstream
// service. With orchestration.workstreams.enabled false it returns (nil,
// nil) and touches no workstream adapter constructor, mirroring
// composeResultAnalysis's own gate shape.
//
// analysisGate is TRD 07 checkpoint 6's dependent-dispatch gate
// (FIND-107): wiring it here is what makes an incomplete, failed, or stale
// analysis bound to a workstream actually block a root confirmation or a
// start_task transition through the composed service. Extracting this
// wiring into its own function, separate from the rest of composeRuntime's
// heavier Slack and ADK setup, lets a test build the exact same production
// wiring and prove the gate is reachable end to end, not only through the
// workstream package's own unit tests.
func composeWorkstream(
	cfg config.Config,
	paths config.Paths,
	store *adaptersqlite.Store,
	resultPayloadStore port.ResultPayloadStore,
	analysisGate port.AnalysesByWorkstream,
) (*workstreamusecase.Service, error) {
	if !cfg.Orchestration.Workstreams.Enabled {
		return nil, nil
	}
	allowedProjects := make(map[string]struct{}, len(paths.SandboxProjectRoots))
	for project := range paths.SandboxProjectRoots {
		allowedProjects[project] = struct{}{}
	}
	if len(allowedProjects) == 0 {
		return nil, errors.New("initialize workstream service: at least one registered sandbox project is required")
	}
	trustedResults, err := adaptersqlite.NewResultStore(store, resultPayloadStore)
	if err != nil {
		return nil, fmt.Errorf("initialize workstream result verifier: %w", err)
	}
	workstreamStore := adaptersqlite.NewWorkstreamStore(store)
	if workstreamStore == nil {
		return nil, errors.New("initialize workstream store")
	}
	service, err := workstreamusecase.New(workstreamusecase.Config{
		Enabled: true, ResultHandlesEnabled: cfg.Orchestration.ResultHandles.Enabled,
		Limits:          domain.WorkstreamLimits{MaxNonTerminalTasks: cfg.Orchestration.Workstreams.MaxNonTerminalTasks, MaxDependenciesPerTask: cfg.Orchestration.Workstreams.MaxDependenciesPerTask},
		AllowedProjects: allowedProjects,
	}, workstreamusecase.Dependencies{Store: workstreamStore, Clock: port.SystemClock{}, ResultVerifier: trustedResults, LinkCommitter: workstreamStore, ResultReader: trustedResults, AnalysisGate: analysisGate})
	if err != nil {
		return nil, fmt.Errorf("initialize workstream service: %w", err)
	}
	return service, nil
}
