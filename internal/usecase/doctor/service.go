package doctor

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Dauno/slack-local-agent/internal/agentdef"
	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/secure"
)

const (
	SlackBotTokenKey                    = "SLACK_BOT_TOKEN"
	SlackAppTokenKey                    = "SLACK_APP_TOKEN"
	defaultAuxiliaryModelTimeoutSeconds = 120
)

type SecretResolver interface {
	Resolve(keys ...string) (map[string]string, error)
}

type DatabaseChecker interface {
	CheckDatabase(ctx context.Context, path string) error
}

type ArtifactChecker interface {
	CheckArtifactStore(ctx context.Context, path string, maxBytes int) error
}

type JobStoreChecker interface {
	CheckExternalAgentJobs(ctx context.Context, path string) error
}

// JobStoreHealthChecker is optional so existing doctor embedders keep the
// original checker contract while activation-aware stores expose bounded
// outbox state.
type JobStoreHealthChecker interface {
	CheckExternalAgentActivationHealth(ctx context.Context, path string) (domain.ExternalAgentJobActivationHealth, error)
}

// JobStoreIdentityChecker is optional so doctor keeps the original checker
// contract while identity-aware stores expose bounded, content-free result
// identity health. The aggregate carries counts only; job IDs, actor,
// conversation, digest, reference, and content values are forbidden.
type JobStoreIdentityChecker interface {
	CheckExternalAgentResultIdentityHealth(ctx context.Context, path string) (domain.ExternalAgentJobIdentityHealth, error)
}

type RemediableError interface {
	error
	Remediation() string
}

type ActionableError struct {
	Err error
	Fix string
}

func (e *ActionableError) Error() string       { return e.Err.Error() }
func (e *ActionableError) Unwrap() error       { return e.Err }
func (e *ActionableError) Remediation() string { return e.Fix }

type LiveChecker interface {
	CheckSlackBot(ctx context.Context, botToken string) error
	CheckSlackApp(ctx context.Context, botToken, appToken string) error
	CheckSlackContext(ctx context.Context, botToken string) error
	CheckSlackCanvas(ctx context.Context, botToken string) error
	CheckSlackExports(ctx context.Context, botToken string) error
	CheckModel(ctx context.Context, model config.ModelConfig, apiKey string) error
	CheckResolvedModel(ctx context.Context, resolved *agentdef.ResolvedModel, apiKey string) error
}

type AttachmentLiveChecker interface {
	CheckAttachmentAnalyzer(ctx context.Context, resolved *agentdef.ResolvedModel, apiKey string) error
}

type AudioTranscriptionLiveChecker interface {
	CheckAudioTranscription(ctx context.Context, resolved *agentdef.ResolvedModel, apiKey string) error
}

// CLIProviderCheck is the typed result of one offline agent_cli provider
// check. ShimName is the trusted mapper identity returned by the cli-v1
// describe exchange; it is empty when describe was not performed.
type CLIProviderCheck struct {
	Detail   string
	ShimName string
}

// CLIProviderChecker validates a selected agent_cli provider. The offline
// check covers shim command resolution, project canonicalization, and the
// cli-v1 describe/validate handshake. The live check reports the CLI's saved
// authentication status without making a model call; it selects the
// application-owned command from the trusted mapper identity, never from the
// configurable provider name or shim-supplied argv.
type CLIProviderChecker interface {
	CheckProvider(ctx context.Context, resolved *agentdef.ResolvedModel, cfg config.Config, projectRoot string, describe bool) (CLIProviderCheck, error)
	CheckAuthentication(ctx context.Context, resolved *agentdef.ResolvedModel, shimName string) (string, error)
}

type ACPProviderChecker interface {
	CheckProvider(ctx context.Context, resolved *agentdef.ResolvedModel, projectRoots map[string]string) (string, error)
}

// CounterChecker validates that a configured token counter strategy and ID
// are implemented by this release. It is the same availability resolution
// used at startup, so doctor offline can never disagree with the binary.
type CounterChecker interface {
	CheckCounter(strategy, id string) error
}

type Dependencies struct {
	ConfigPath string
	LoadConfig func(path string) (config.Config, error)
	Secrets    SecretResolver
	Database   DatabaseChecker
	Artifacts  ArtifactChecker
	Jobs       JobStoreChecker
	Live       LiveChecker
	CLI        CLIProviderChecker
	ACP        ACPProviderChecker
	Counter    CounterChecker
}

type Status string

const (
	StatusPass Status = "pass"
	StatusFail Status = "fail"
)

type Result struct {
	Name        string
	Status      Status
	Detail      string
	Remediation string
	Fatal       bool
}

type Report struct {
	Results []Result
}

func (r Report) ExitCode() int {
	code := 0
	for _, result := range r.Results {
		if result.Status != StatusFail {
			continue
		}
		if result.Fatal {
			return 2
		}
		code = 1
	}
	return code
}

type Service struct {
	deps Dependencies
}

type selectedModel struct {
	agent    string
	resolved *agentdef.ResolvedModel
}

func selectedModelAlreadyIncluded(selected []selectedModel, agent string) bool {
	for _, model := range selected {
		if model.agent == agent {
			return true
		}
	}
	return false
}

func eligibleAgentNames(defs *agentdef.Definitions) []string {
	if defs == nil {
		return nil
	}
	names := make([]string, 0, len(defs.Agents))
	for name, agent := range defs.Agents {
		if name == "root_agent" || agent.Role != "" {
			continue
		}
		if agent.AgentClass != "LlmAgent" && agent.AgentClass != "AcpAgent" {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func New(deps Dependencies) (*Service, error) {
	if strings.TrimSpace(deps.ConfigPath) == "" {
		return nil, errors.New("doctor config path is required")
	}
	if deps.LoadConfig == nil {
		deps.LoadConfig = config.Load
	}
	if deps.Secrets == nil {
		return nil, errors.New("doctor secret resolver is required")
	}
	if deps.Database == nil {
		return nil, errors.New("doctor database checker is required")
	}
	return &Service{deps: deps}, nil
}

func (s *Service) Run(ctx context.Context, includeLive bool) Report {
	report := Report{}
	cfg, err := s.deps.LoadConfig(s.deps.ConfigPath)
	if err != nil {
		result := Result{
			Name:        "configuration",
			Status:      StatusFail,
			Detail:      err.Error(),
			Remediation: "Run: local-agent init, then fix .local-agent/config.yaml as reported.",
		}
		var validation *config.ValidationError
		switch {
		case errors.Is(err, os.ErrNotExist):
			result.Detail = "configuration file is missing"
			result.Remediation = "Run: local-agent init"
		case errors.As(err, &validation):
			// Typed validation failures are ordinary health-check failures.
		default:
			result.Fatal = true
		}
		report.Results = append(report.Results, result)
		return report
	}
	report.pass("configuration", "typed configuration is valid")

	projectRoot := filepath.Dir(filepath.Dir(s.deps.ConfigPath))
	paths, pathErr := cfg.ResolvePaths(projectRoot)
	var (
		defs                    *agentdef.Definitions
		resolvedModel           *agentdef.ResolvedModel
		selectedModels          []selectedModel
		defsLoadFailed          bool
		durableACP              bool
		transcriptionResolved   *agentdef.ResolvedModel
		transcriptionResolveErr error
	)
	if pathErr != nil {
		report.fail("SQLite", pathErr.Error(), "Fix state.dir and state.db in .local-agent/config.yaml.", false)
	} else {
		var defsErr error
		defs, defsErr = agentdef.Load(paths.StateDir)
		if defsErr != nil {
			defsLoadFailed = true
			report.fail("agent definitions", defsErr.Error(), "Fix .local-agent/agents/*.yaml and .local-agent/providers/*.yaml files.", false)
		} else if defs != nil {
			rootDef, ok := defs.Agents["root_agent"]
			if !ok {
				defsLoadFailed = true
				report.fail("agent definitions", "agent definition root_agent is required", "Add .local-agent/agents/root_agent.yaml.", false)
			} else if resolvedModel, defsErr = defs.ResolveModel(rootDef.Model); defsErr != nil {
				defsLoadFailed = true
				report.fail("agent definitions", defsErr.Error(), "Fix root_agent.model in .local-agent/agents/root_agent.yaml.", false)
			} else {
				selectedModels = append(selectedModels, selectedModel{agent: "root_agent", resolved: resolvedModel})
				for _, agentToolName := range rootDef.AgentTools {
					agentTool, exists := defs.Agents[agentToolName]
					if !exists {
						defsErr = fmt.Errorf("agent tool %q is not defined", agentToolName)
						break
					}
					modelRef := agentTool.Model
					if agentTool.AgentClass == "AcpAgent" {
						modelRef = agentTool.Runtime
					}
					agentToolResolved, resolveErr := defs.ResolveModel(modelRef)
					if resolveErr != nil {
						defsErr = fmt.Errorf("resolve agent tool %q model: %w", agentToolName, resolveErr)
						break
					}
					selectedModels = append(selectedModels, selectedModel{agent: agentToolName, resolved: agentToolResolved})
				}
				if defsErr == nil {
					// Tambien revisar agentes auto-descubiertos.
					for _, name := range eligibleAgentNames(defs) {
						if agentdef.IsReservedAgentName(name) {
							continue
						}
						// Skip if already checked via rootDef.AgentTools.
						alreadyChecked := false
						for _, toolName := range rootDef.AgentTools {
							if toolName == name {
								alreadyChecked = true
								break
							}
						}
						if alreadyChecked {
							continue
						}
						agentTool, exists := defs.Agents[name]
						if !exists {
							continue
						}
						modelRef := agentTool.Model
						if agentTool.AgentClass == "AcpAgent" {
							modelRef = agentTool.Runtime
						}
						agentToolResolved, resolveErr := defs.ResolveModel(modelRef)
						if resolveErr != nil {
							defsErr = fmt.Errorf("resolve auto-discovered agent %q model: %w", name, resolveErr)
							break
						}
						selectedModels = append(selectedModels, selectedModel{agent: name, resolved: agentToolResolved})
					}
				}
				if defsErr == nil {
					blueprints := make([]*agentdef.WorkflowBlueprint, 0, len(rootDef.WorkflowTools))
					for _, workflowID := range rootDef.WorkflowTools {
						bp, loadErr := defs.LoadWorkflow(paths.StateDir, workflowID)
						if loadErr != nil {
							defsErr = fmt.Errorf("workflow %q: %w", workflowID, loadErr)
							break
						}
						blueprints = append(blueprints, bp)
						for _, doc := range bp.OrderedDocuments() {
							if doc.AgentClass == agentdef.AgentClassAcp && doc.ACP != nil {
								workflowResolved, resolveErr := defs.ResolveModel(doc.ACP.Runtime)
								if resolveErr != nil {
									defsErr = fmt.Errorf("workflow %q agent %q: resolve runtime %q: %w", workflowID, doc.Name, doc.ACP.Runtime, resolveErr)
									break
								}
								selectedModels = append(selectedModels, selectedModel{agent: "workflow:" + doc.Name, resolved: workflowResolved})
								continue
							}
							if doc.AgentClass != agentdef.AgentClassLLM || doc.LLM == nil {
								continue
							}
							workflowResolved, resolveErr := defs.ResolveModel(doc.LLM.Model)
							if resolveErr != nil {
								defsErr = fmt.Errorf("workflow %q agent %q: resolve model %q: %w", workflowID, doc.Name, doc.LLM.Model, resolveErr)
								break
							}
							selectedModels = append(selectedModels, selectedModel{agent: "workflow:" + doc.Name, resolved: workflowResolved})
						}
						if defsErr != nil {
							break
						}
					}
					if defsErr == nil {
						defsErr = defs.ValidateWorkflowComposition(rootDef, blueprints, cfg.Sandbox.Enabled)
					}
				}
				if defsErr == nil && cfg.Memory.Enabled {
					curator, exists := defs.Agents["memory_curator"]
					if !exists {
						defsErr = errors.New("agent definition memory_curator is required when memory is enabled")
					} else if curatorResolved, resolveErr := defs.ResolveModel(curator.Model); resolveErr != nil {
						defsErr = fmt.Errorf("resolve memory_curator model: %w", resolveErr)
					} else if !selectedModelAlreadyIncluded(selectedModels, "memory_curator") {
						selectedModels = append(selectedModels, selectedModel{agent: "memory_curator", resolved: curatorResolved})
					}
				}
				if defsErr == nil {
					if attachment, exists := defs.Agents["attachment_analyzer"]; exists {
						attachmentResolved, resolveErr := defs.ResolveModel(attachment.Model)
						if resolveErr != nil {
							defsErr = fmt.Errorf("resolve attachment_analyzer model: %w", resolveErr)
						} else if problems := agentdef.ValidateAttachmentModelCapability(attachmentResolved); len(problems) > 0 {
							defsErr = errors.New(strings.Join(problems, "; "))
						} else if !selectedModelAlreadyIncluded(selectedModels, "attachment_analyzer") {
							selectedModels = append(selectedModels, selectedModel{agent: "attachment_analyzer", resolved: attachmentResolved})
						}
					}
				}
				if defsErr != nil {
					defsLoadFailed = true
					report.fail("agent definitions", defsErr.Error(), "Fix selected agent model references and provider compatibility.", false)
				} else {
					report.pass("agent definitions", fmt.Sprintf("%d providers, %d agents loaded", len(defs.Providers), len(defs.Agents)))
				}
			}
		}
	}
	transcriptionProfile := strings.TrimSpace(cfg.Slack.Files.TranscriptionProfile)
	if transcriptionProfile != "" {
		transcriptionDefs := defs
		if transcriptionDefs == nil {
			transcriptionDefs = agentdef.NormalizeLegacy(cfg.Agent.Name, cfg.Model.Name, cfg.Model.BaseURL, cfg.Model.APIKeyEnv, cfg.Model.ReasoningEffort, cfg.Model.Headers, cfg.Model.ExtraBody)
		}
		if transcriptionDefs == nil {
			transcriptionResolveErr = errors.New("no provider registry is available")
		} else {
			transcriptionResolved, transcriptionResolveErr = transcriptionDefs.ResolveModel(transcriptionProfile)
			if transcriptionResolveErr == nil {
				transcriptionResolveErr = validateAudioTranscriptionProfile(transcriptionResolved)
			}
		}
	}
	if defs != nil {
		for _, definition := range defs.Agents {
			if definition.AgentClass == "AcpAgent" && definition.ExecutionMode == agentdef.ExecutionModeDurableJob {
				durableACP = true
				break
			}
		}
	}
	durableOpenAI := defs == nil || (resolvedModel != nil && resolvedModel.Type() == agentdef.ProviderTypeOpenAICompatible)
	if err := config.ValidateADKCompaction(cfg, durableOpenAI, durableOpenAI); err != nil {
		report.fail("ADK compaction", err.Error(), "Set context.adk_compaction.enabled=true with valid positive limits and keep summary composition compatible, then restart.", false)
	} else if durableOpenAI {
		report.pass("ADK compaction", "durable OpenAI-compatible model history projection is enabled")
	}
	if durableACP {
		report.pass("ACP result delivery", fmt.Sprintf("complete results use up to %d Markdown parts or sanitized Markdown files", cfg.ACP.Delivery.MaxMarkdownParts))
	}

	modelAPIKeyEnv := cfg.Model.APIKeyEnv
	rootCLIProvider := resolvedModel != nil && resolvedModel.IsAgentCLI()
	if resolvedModel != nil && !rootCLIProvider {
		modelAPIKeyEnv = resolvedModel.APIKeyEnv
	}
	keys := []string{modelAPIKeyEnv, SlackBotTokenKey, SlackAppTokenKey}
	if defs != nil {
		keys = append(keys, defs.RequiredAPIKeyEnvs()...)
	}
	if transcriptionResolved != nil && transcriptionResolved.Type() == agentdef.ProviderTypeOpenAICompatible && strings.TrimSpace(transcriptionResolved.APIKeyEnv) != "" {
		keys = append(keys, transcriptionResolved.APIKeyEnv)
	}
	keys = uniqueStrings(keys)
	values, err := s.deps.Secrets.Resolve(keys...)
	if err != nil {
		report.fail("secrets", err.Error(), "Fix .env syntax or process environment values.", false)
		return report
	}
	redactionValues := make([]string, 0, len(keys))
	for _, key := range keys {
		redactionValues = append(redactionValues, values[key])
	}
	redactor := secure.NewRedactor(redactionValues...)

	validSecrets := make(map[string]bool, len(keys))
	checkSecret := func(name, key, expectedPrefix, remediation string) {
		value, exists := values[key]
		if !exists || strings.TrimSpace(value) == "" {
			report.fail(name, fmt.Sprintf("%s is not set", key), remediation, false)
			return
		}
		if expectedPrefix != "" && (len(value) <= len(expectedPrefix) || !strings.HasPrefix(value, expectedPrefix)) {
			report.fail(name, fmt.Sprintf("%s must start with %s", key, expectedPrefix), remediation, false)
			return
		}
		validSecrets[key] = true
		report.pass(name, fmt.Sprintf("%s is configured (%s)", key, secure.Mask(value)))
	}
	checkedModelAPIKeys := make(map[string]bool)
	if rootCLIProvider {
		// agent_cli providers require no model API key.
		report.pass("model API key", "agent CLI provider requires no model API key")
	} else {
		checkSecret("model API key", modelAPIKeyEnv, "", "Set "+modelAPIKeyEnv+" in the process environment or .env.")
		checkedModelAPIKeys[modelAPIKeyEnv] = true
	}
	for _, selected := range selectedModels {
		if selected.resolved.IsAgentCLI() || selected.resolved.IsACP() || checkedModelAPIKeys[selected.resolved.APIKeyEnv] {
			continue
		}
		key := selected.resolved.APIKeyEnv
		checkedModelAPIKeys[key] = true
		checkSecret("model API key ("+selected.agent+")", key, "", "Set "+key+" in the process environment or .env.")
	}
	checkSecret("Slack bot token", SlackBotTokenKey, "xoxb-", "Set a Bot User OAuth Token beginning with xoxb-.")
	checkSecret("Slack app token", SlackAppTokenKey, "xapp-", "Set an app-level Socket Mode token beginning with xapp- and connections:write.")

	audioTranscriptionReady := false
	if transcriptionProfile != "" {
		switch {
		case transcriptionResolveErr != nil:
			report.fail("audio transcription profile", redactor.String(fmt.Sprintf("resolve %q: %v", transcriptionProfile, transcriptionResolveErr)), "Fix slack.files.transcription_profile and its openai_compatible provider definition.", false)
		case cfg.Slack.Files.TranscriptionTimeoutSeconds <= 0:
			report.fail("audio transcription profile", "transcription timeout must be greater than zero", "Set slack.files.transcription_timeout_seconds to a positive value.", false)
		case transcriptionResolved.Type() != agentdef.ProviderTypeOpenAICompatible:
			report.fail("audio transcription profile", "profile must resolve to an openai_compatible provider", "Select an openai_compatible profile in slack.files.transcription_profile.", false)
		case !validSecrets[transcriptionResolved.APIKeyEnv]:
			report.fail("audio transcription profile", fmt.Sprintf("profile %q requires %s, which is not set", transcriptionProfile, transcriptionResolved.APIKeyEnv), "Set the transcription provider API key in the process environment or .env.", false)
		default:
			validSecrets[transcriptionResolved.APIKeyEnv] = true
			audioTranscriptionReady = true
			report.pass("audio transcription profile", fmt.Sprintf("profile %q resolved to %s/%s; dedicated multipart STT is configured", transcriptionProfile, transcriptionResolved.Provider.Name, transcriptionResolved.Model))
		}
	}

	if pathErr == nil {
		if err := s.deps.Database.CheckDatabase(ctx, paths.DatabaseFile); err != nil {
			remediation := "Run local-agent init or fix permissions for the configured database path."
			var actionable RemediableError
			if errors.As(err, &actionable) && actionable.Remediation() != "" {
				remediation = actionable.Remediation()
			}
			report.fail("SQLite", redactor.String(err.Error()), remediation, false)
		} else {
			report.pass("SQLite", "database exists, is migrated, and is readable/writable")
		}
		if s.deps.Artifacts != nil {
			if err := s.deps.Artifacts.CheckArtifactStore(ctx, paths.ArtifactDir, cfg.ACP.MaxResultArtifactBytes); err != nil {
				report.fail("ACP artifacts", redactor.String(err.Error()), "Fix the configured artifact directory and permissions; do not place secrets in it.", false)
			} else {
				report.pass("ACP artifacts", fmt.Sprintf("private artifact directory and %d-byte bound are valid", cfg.ACP.MaxResultArtifactBytes))
			}
		}
		if s.deps.Jobs != nil {
			if err := s.deps.Jobs.CheckExternalAgentJobs(ctx, paths.DatabaseFile); err != nil {
				report.fail("external-agent jobs", redactor.String(err.Error()), "Run local-agent init to migrate the local job store, or repair the configured database.", false)
			} else {
				report.pass("external-agent jobs", "durable job and notification outbox are available")
			}
			if healthChecker, ok := s.deps.Jobs.(JobStoreHealthChecker); ok {
				health, healthErr := healthChecker.CheckExternalAgentActivationHealth(ctx, paths.DatabaseFile)
				if healthErr != nil {
					report.fail("external-agent activations", redactor.String(healthErr.Error()), "Drain or investigate the activation outbox before starting the agent.", false)
				} else if health.Stuck > 0 {
					report.fail("external-agent activations", fmt.Sprintf("activation outbox has %d stuck activations (pending=%d, processed=%d, completion_unknown=%d)", health.Stuck, health.Pending, health.Processed, health.CompletionUnknown), "Inspect the activation worker and let expired leases become reclaimable.", false)
				} else {
					report.pass("external-agent activations", fmt.Sprintf("pending=%d, processed=%d, completion_unknown=%d", health.Pending, health.Processed, health.CompletionUnknown))
				}
			}
			// Operational decision (P2-02): a non-terminal foreground
			// activation is the P0 contract violation and fails doctor; an
			// incomplete delivery identity (notification or activation) also
			// fails because every post-v32 delivery must carry bytes and a
			// SHA-256. Retired foreground activations are expected v31 repair
			// evidence. Historical completed jobs without result identity are
			// marked in the existing event ledger and remain informational; current
			// incomplete rows fail closed. Only counts are emitted; no job ID, actor,
			// conversation, digest, reference, or content value.
			if identityChecker, ok := s.deps.Jobs.(JobStoreIdentityChecker); ok {
				identity, identityErr := identityChecker.CheckExternalAgentResultIdentityHealth(ctx, paths.DatabaseFile)
				switch {
				case identityErr != nil:
					report.fail("external-agent result identity", redactor.String(identityErr.Error()), "Repair the configured database so every durable result carries a complete identity, or reset state after backup.", false)
				case identity.ForegroundActivationsActive > 0:
					report.fail("external-agent result identity", fmt.Sprintf("identity health: %d non-terminal foreground activations (defect), %d notifications without identity, %d activations without content, %d activations without identity, %d retired foreground activations, %d current completed jobs without result identity", identity.ForegroundActivationsActive, identity.NotificationsWithoutIdentity, identity.ActivationsWithoutContent, identity.ActivationsWithoutIdentity, identity.RetiredForegroundActivations, identity.JobsCompletedWithoutResultIdentity), "Stop the agent and restore the foreground-suppression build: foreground jobs must never produce claimable activations.", false)
				case identity.NotificationsWithoutIdentity > 0 || identity.ActivationsWithoutContent > 0 || identity.ActivationsWithoutIdentity > 0 || identity.JobsCompletedWithoutResultIdentity > 0:
					report.fail("external-agent result identity", fmt.Sprintf("identity health: %d notifications without identity, %d activations without content, %d activations without identity, %d non-terminal foreground activations, %d retired foreground activations, %d current completed jobs without result identity, %d historical completed jobs without result identity", identity.NotificationsWithoutIdentity, identity.ActivationsWithoutContent, identity.ActivationsWithoutIdentity, identity.ForegroundActivationsActive, identity.RetiredForegroundActivations, identity.JobsCompletedWithoutResultIdentity, identity.JobsCompletedWithoutResultIdentityLegacy), "Do not start the agent until incomplete delivery identity is repaired; results must carry bytes and SHA-256 after transformation.", false)
				default:
					report.pass("external-agent result identity", fmt.Sprintf("result identity is complete (informational: %d historical completed jobs without result identity, %d retired foreground activations)", identity.JobsCompletedWithoutResultIdentityLegacy, identity.RetiredForegroundActivations))
				}
			}
		}
	}
	if pathErr == nil {
		var acpModels []selectedModel
		for _, selected := range selectedModels {
			if selected.resolved.IsACP() {
				acpModels = append(acpModels, selected)
			}
		}
		if len(acpModels) > 0 && s.deps.ACP == nil {
			report.fail("ACP provider", "ACP checker is unavailable", "Reinstall local-agent with ACP support.", false)
		}
		for _, selected := range acpModels {
			if s.deps.ACP == nil {
				break
			}
			detail, err := s.deps.ACP.CheckProvider(ctx, selected.resolved, paths.SandboxProjectRoots)
			if err != nil {
				report.fail("ACP provider ("+selected.agent+")", redactor.String(err.Error()), "Verify the ACP command, saved OpenCode authentication, profile options, and registered projects.", false)
				continue
			}
			report.pass("ACP provider ("+selected.agent+")", detail)
		}
		report.pass("OpenCode management operators", fmt.Sprintf("%d configured", len(cfg.OpenCode.Management.AllowedUserIDs)))
	}

	shimNames := make(map[string]string)
	if pathErr == nil {
		var cliModels []selectedModel
		for _, selected := range selectedModels {
			if selected.resolved.IsAgentCLI() {
				cliModels = append(cliModels, selected)
			}
		}
		if len(cliModels) > 0 && s.deps.CLI == nil {
			report.fail("agent CLI provider", "agent CLI checker is unavailable", "Reinstall local-agent with agent CLI support.", false)
		}
		described := make(map[string]bool)
		for _, selected := range cliModels {
			if s.deps.CLI == nil {
				break
			}
			providerName := selected.resolved.Provider.Name
			describe := !described[providerName]
			resultName := "agent CLI provider (" + selected.agent + ")"
			check, err := s.deps.CLI.CheckProvider(ctx, selected.resolved, cfg, projectRoot, describe)
			if err != nil {
				report.fail(resultName, redactor.String(err.Error()),
					"Verify the shim command, the installed CLI version, selected profile, and sandbox.projects registration.", false)
				continue
			}
			if describe {
				described[providerName] = true
				shimNames[providerName] = check.ShimName
			}
			report.pass(resultName, check.Detail)
		}
	}

	// Context engineering offline checks.
	if cfg.Context.ModelBudget != nil {
		budgetPct := cfg.Context.ModelBudget.MaxRequestPercent
		if budgetPct >= 20 && budgetPct <= 80 {
			report.pass("model budget", fmt.Sprintf("max request utilization %d%% (hard ceiling)", budgetPct))
			if budgetPct > 70 {
				report.fail("model budget", fmt.Sprintf("max request percent %d exceeds 70 — quality may degrade", budgetPct), "Lower context.model_budget.max_request_percent.", false)
			}
		}
	}
	if cfg.Context.RecoverableResults != nil {
		report.pass("recoverable results", fmt.Sprintf("store configured: %d bytes max, %d retention days", cfg.Context.RecoverableResults.MaxResultBytes, cfg.Context.RecoverableResults.RetentionDays))
	}
	if cfg.Context.ModelBudget != nil && resolvedModel != nil && resolvedModel.Type() == agentdef.ProviderTypeOpenAICompatible {
		if capabilityProblems := agentdef.ValidateProfileCapability(resolvedModel); len(capabilityProblems) > 0 {
			report.fail("model context capability", strings.Join(capabilityProblems, "; "), "Fix the selected root profile context capability.", false)
		} else {
			if resolvedModel.MaxOutputTokens < 0 {
				report.fail("model context window", "root model profile max_output_tokens must not be negative", "Set max_output_tokens >= 0 in the root profile.", false)
			} else if resolvedModel.MaxOutputTokens >= resolvedModel.ContextWindowTokens {
				report.fail("model context window", "max_output_tokens must be less than context_window_tokens", "Reduce max_output_tokens in the root profile.", false)
			} else {
				report.pass("model context window", fmt.Sprintf("context window: %d tokens, output: %d tokens", resolvedModel.ContextWindowTokens, resolvedModel.MaxOutputTokens))
			}
			counterCheck := resolvedModel.CounterStrategy
			if counterCheck == "" {
				report.fail("token counter", "no token_counter configured for root openai_compatible model", "Add token_counter.strategy (e.g. byte_bound) to the root profile YAML.", false)
			} else {
				detail := fmt.Sprintf("strategy: %s", counterCheck)
				if resolvedModel.CounterID != "" {
					detail += fmt.Sprintf(", id: %s", resolvedModel.CounterID)
				}
				if s.deps.Counter != nil {
					if counterErr := s.deps.Counter.CheckCounter(resolvedModel.CounterStrategy, resolvedModel.CounterID); counterErr != nil {
						report.fail("token counter", redactor.String(counterErr.Error()), "Use a strategy and id implemented by this release, or roll back to a binary that implements the configured combination. There is no silent fallback.", false)
					} else {
						report.pass("token counter", detail)
					}
				} else {
					report.pass("token counter", detail)
				}
			}
			if attachment, exists := defs.Agents["attachment_analyzer"]; exists {
				if attachmentResolved, resolveErr := defs.ResolveModel(attachment.Model); resolveErr == nil {
					if problems := agentdef.ValidateAttachmentModelCapability(attachmentResolved); len(problems) > 0 {
						report.fail("attachment_analyzer capability", strings.Join(problems, "; "), "Configure the attachment_analyzer profile with token_counter.strategy: estimator and id: "+agentdef.VisualEstimatorID+" before rollout.", false)
					} else {
						report.pass("attachment_analyzer capability", fmt.Sprintf("visual estimator %s is configured and media-capable", agentdef.VisualEstimatorID))
					}
				}
			}
		}
	}
	if cfg.Context.ModelBudget != nil {
		for _, selected := range selectedModels {
			if selected.resolved == nil || selected.resolved.Type() != agentdef.ProviderTypeOpenAICompatible || selected.resolved.CounterStrategy == "" {
				continue
			}
			if selected.agent == "root_agent" {
				continue
			}
			agentName := "token counter (" + selected.agent + ")"
			detail := fmt.Sprintf("strategy: %s", selected.resolved.CounterStrategy)
			if selected.resolved.CounterID != "" {
				detail += fmt.Sprintf(", id: %s", selected.resolved.CounterID)
			}
			if s.deps.Counter == nil {
				report.pass(agentName, detail)
				continue
			}
			if counterErr := s.deps.Counter.CheckCounter(selected.resolved.CounterStrategy, selected.resolved.CounterID); counterErr != nil {
				report.fail(agentName, redactor.String(counterErr.Error()), "Use a strategy and id implemented by this release; there is no silent fallback.", false)
			} else {
				report.pass(agentName, detail)
			}
		}
	}
	if cfg.CodeIntelligence != nil && cfg.CodeIntelligence.Enabled {
		report.pass("code intelligence", fmt.Sprintf("enabled: %d LSP servers, %d routes", len(cfg.CodeIntelligence.LSPServers), len(cfg.CodeIntelligence.LSPRoutes)))
	}

	if !includeLive {
		return report
	}
	if s.deps.Live == nil {
		report.fail("live checks", "live checker is unavailable", "Reinstall local-agent with live-check support.", false)
		return report
	}
	if value := values[SlackBotTokenKey]; validSecrets[SlackBotTokenKey] {
		liveCtx, cancel := checkTimeout(ctx, cfg.Runtime.SlackAPITimeoutSeconds)
		err := s.deps.Live.CheckSlackBot(liveCtx, value)
		cancel()
		if err != nil {
			report.fail("Slack bot connectivity", redactor.String(err.Error()), "Verify SLACK_BOT_TOKEN and Slack workspace access.", false)
		} else {
			report.pass("Slack bot connectivity", "Slack auth check passed")
		}
	}
	if botToken, appToken := values[SlackBotTokenKey], values[SlackAppTokenKey]; validSecrets[SlackBotTokenKey] && validSecrets[SlackAppTokenKey] {
		liveCtx, cancel := checkTimeout(ctx, cfg.Runtime.SlackAPITimeoutSeconds)
		err := s.deps.Live.CheckSlackApp(liveCtx, botToken, appToken)
		cancel()
		if err != nil {
			report.fail("Slack Socket Mode", redactor.String(err.Error()), "Verify SLACK_APP_TOKEN has connections:write and belongs to this app.", false)
		} else {
			report.pass("Slack Socket Mode", "app-level token can open a Socket Mode connection")
		}
	}
	if cfg.Slack.Context.Enabled && validSecrets[SlackBotTokenKey] {
		liveCtx, cancel := checkTimeout(ctx, cfg.Runtime.SlackAPITimeoutSeconds)
		err := s.deps.Live.CheckSlackContext(liveCtx, values[SlackBotTokenKey])
		cancel()
		if err != nil {
			report.fail("Slack context enrichment", redactor.String(err.Error()), "Reinstall the Slack app with users:read, then verify the bot token.", false)
		} else {
			report.pass("Slack context enrichment", "users:read capability check passed")
		}
	}
	if cfg.Canvases.Enabled && validSecrets[SlackBotTokenKey] {
		liveCtx, cancel := checkTimeout(ctx, cfg.Canvases.TimeoutSeconds)
		err := s.deps.Live.CheckSlackCanvas(liveCtx, values[SlackBotTokenKey])
		cancel()
		if err != nil {
			report.fail("Slack Canvas", redactor.String(err.Error()), "Regenerate the manifest, reinstall the Slack app with canvases:write, and verify Canvas availability for the workspace.", false)
		} else {
			report.pass("Slack Canvas", "canvases:write scope check passed")
		}
	}
	if cfg.Exports.Enabled && validSecrets[SlackBotTokenKey] {
		liveCtx, cancel := checkTimeout(ctx, cfg.Exports.TimeoutSeconds)
		err := s.deps.Live.CheckSlackExports(liveCtx, values[SlackBotTokenKey])
		cancel()
		if err != nil {
			report.fail("Slack generated files", redactor.String(err.Error()), "Regenerate the manifest, reinstall the Slack app with files:write, and verify the bot token.", false)
		} else {
			report.pass("Slack generated files", "files:write scope check passed")
		}
	}
	if durableACP && validSecrets[SlackBotTokenKey] {
		liveCtx, cancel := checkTimeout(ctx, cfg.Runtime.SlackAPITimeoutSeconds)
		err := s.deps.Live.CheckSlackExports(liveCtx, values[SlackBotTokenKey])
		cancel()
		if err != nil {
			report.fail("Slack ACP result delivery", redactor.String(err.Error()), "Regenerate the manifest, reinstall the Slack app with files:write, and verify the bot token.", false)
		} else {
			report.pass("Slack ACP result delivery", "files:write scope check passed")
		}
	}
	if s.deps.CLI != nil {
		authChecked := make(map[string]bool)
		for _, selected := range selectedModels {
			if !selected.resolved.IsAgentCLI() {
				continue
			}
			providerName := selected.resolved.Provider.Name
			shimName := shimNames[providerName]
			// A missing identity means describe failed and already produced the
			// actionable provider result. Do not add a misleading auth failure.
			if strings.TrimSpace(shimName) == "" || authChecked[providerName] {
				continue
			}
			authChecked[providerName] = true
			liveCtx, cancel := checkTimeout(ctx, cfg.Runtime.ModelTimeoutSeconds)
			detail, err := s.deps.CLI.CheckAuthentication(liveCtx, selected.resolved, shimName)
			cancel()
			if err != nil {
				report.fail("agent CLI authentication ("+providerName+")", redactor.String(err.Error()),
					"Log in to the agent CLI (for example: opencode auth login or codex login) so it can reuse its saved credentials.", false)
			} else {
				report.pass("agent CLI authentication ("+providerName+")", detail)
			}
		}
	}
	if !rootCLIProvider {
		if apiKey := values[modelAPIKeyEnv]; validSecrets[modelAPIKeyEnv] && !defsLoadFailed {
			liveCtx, cancel := checkTimeout(ctx, cfg.Runtime.ModelTimeoutSeconds)
			var err error
			if resolvedModel != nil {
				err = s.deps.Live.CheckResolvedModel(liveCtx, resolvedModel, apiKey)
			} else if defs == nil {
				err = s.deps.Live.CheckModel(liveCtx, cfg.Model, apiKey)
			}
			cancel()
			if err != nil {
				report.fail("model endpoint", redactor.String(err.Error()), "Verify model.base_url, model.name, request options, and the configured API key.", false)
			} else {
				report.pass("model endpoint", "minimal non-streaming Chat Completions request passed")
			}
		}
	}
	if transcriptionProfile != "" && audioTranscriptionReady && transcriptionResolved != nil {
		checker, ok := s.deps.Live.(AudioTranscriptionLiveChecker)
		if !ok {
			report.fail("audio transcription endpoint", "audio transcription live checker is unavailable", "Reinstall local-agent with dedicated STT live-check support.", false)
		} else {
			liveCtx, cancel := checkTimeout(ctx, defaultAuxiliaryModelTimeoutSeconds)
			err := checker.CheckAudioTranscription(liveCtx, transcriptionResolved, values[transcriptionResolved.APIKeyEnv])
			cancel()
			if err != nil {
				report.fail("audio transcription endpoint", redactor.String(err.Error()), "Verify the transcription provider base URL, model, API key, and /audio/transcriptions support.", false)
			} else {
				report.pass("audio transcription endpoint", "dedicated multipart /audio/transcriptions request passed")
			}
		}
	}
	for _, selected := range selectedModels {
		if selected.resolved == nil || selected.resolved.Type() != agentdef.ProviderTypeOpenAICompatible || selected.agent == "root_agent" {
			continue
		}
		key := selected.resolved.APIKeyEnv
		if !validSecrets[key] {
			continue
		}
		selectedCtx, selectedCancel := checkTimeout(ctx, defaultAuxiliaryModelTimeoutSeconds)
		var selectedErr error
		name := "model endpoint (" + selected.agent + ")"
		if selected.agent == "attachment_analyzer" {
			checker, ok := s.deps.Live.(AttachmentLiveChecker)
			if !ok {
				selectedErr = errors.New("attachment analyzer live checker is unavailable")
			} else {
				selectedErr = checker.CheckAttachmentAnalyzer(selectedCtx, selected.resolved, values[key])
			}
		} else {
			selectedErr = s.deps.Live.CheckResolvedModel(selectedCtx, selected.resolved, values[key])
		}
		selectedCancel()
		if selectedErr != nil {
			report.fail(name, redactor.String(selectedErr.Error()), "Verify the selected auxiliary model endpoint, API key, and tool capability.", false)
		} else {
			report.pass(name, "selected auxiliary model live check passed")
		}
	}
	return report
}

func checkTimeout(ctx context.Context, seconds int) (context.Context, context.CancelFunc) {
	if seconds <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func (r *Report) pass(name, detail string) {
	r.Results = append(r.Results, Result{Name: name, Status: StatusPass, Detail: detail})
}

func (r *Report) fail(name, detail, remediation string, fatal bool) {
	r.Results = append(r.Results, Result{
		Name: name, Status: StatusFail, Detail: detail, Remediation: remediation, Fatal: fatal,
	})
}

func validateAudioTranscriptionProfile(resolved *agentdef.ResolvedModel) error {
	if resolved == nil {
		return errors.New("profile resolved to no model")
	}
	if resolved.Type() != agentdef.ProviderTypeOpenAICompatible {
		return fmt.Errorf("profile requires an %s provider; got %s", agentdef.ProviderTypeOpenAICompatible, resolved.Type())
	}
	if strings.TrimSpace(resolved.Model) == "" {
		return errors.New("profile model must not be empty")
	}
	if strings.TrimSpace(resolved.APIKeyEnv) == "" {
		return errors.New("profile API-key environment variable must not be empty")
	}
	parsed, err := url.Parse(resolved.BaseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return errors.New("profile base URL must be an absolute http or https URL")
	}
	if parsed.User != nil || parsed.Fragment != "" {
		return errors.New("profile base URL must not contain credentials or a fragment")
	}
	return nil
}
