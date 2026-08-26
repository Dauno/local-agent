package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/domain"
)

func TestDefaultMatchesPRD(t *testing.T) {
	t.Parallel()

	want := config.Config{
		State: config.StateConfig{
			Dir: ".local-agent",
			DB:  ".local-agent/local-agent.db",
		},
		Context: config.ContextConfig{
			MaxMessages:                   30,
			MaxChars:                      20_000,
			RetainMessagesPerConversation: 100,
			ModelBudget:                   &config.ModelBudgetConfig{MaxRequestPercent: 60},
			RecoverableResults: &config.RecoverableResultsConfig{
				MaxResultBytes:   4 * 1024 * 1024,
				ChunkMaxBytes:    16384,
				RetentionDays:    7,
				CleanupBatchSize: 100,
			},
			ADKCompaction: &config.ADKCompactionConfig{
				Enabled: true, MaxHistoryChars: 120_000, RecentTurns: 8,
				SummaryEnabled: true, SummaryMaxChars: 8_000, SummaryBudgetTokens: 2_048,
			},
			ContextFeatures: &config.ContextFeaturesConfig{},
		},
		Runtime: config.RuntimeConfig{
			LogLevel:                "info",
			ModelTimeoutSeconds:     0,
			SlackAPITimeoutSeconds:  30,
			MaxConcurrentModelCalls: 4,
			ShutdownGraceSeconds:    30,
			BusyMessage:             "El bot está ocupado procesando otras solicitudes. Intenta de nuevo en unos minutos.",
			ModelErrorMessage:       "No pude completar la respuesta por un error del modelo. Intenta de nuevo.",
		},
		Slack: config.SlackConfig{
			AppName:             "Local Agent",
			BotDisplayName:      "Dev Agent",
			UnauthorizedMessage: "No tienes permiso para usar este bot. Pide acceso a quien administra local-agent.",
			AllowedUserIDs:      []string{},
			AllowedTeamIDs:      []string{},
			AllowedChannelIDs:   []string{},
			PartLabels:          true,
			StandardAgent: config.SlackStandardAgentConfig{SuggestedPrompts: []string{
				"Resume el contexto y destaca las decisiones pendientes.",
				"Analiza el proyecto y señala los riesgos principales.",
				"Prepara un plan de implementación verificable.",
			}, UpdateIntervalSeconds: 3, ProgressLabels: map[domain.ProgressState]string{}},
			Context: config.SlackContextConfig{
				Enabled:                     false,
				MaxChars:                    1500,
				TimeoutSeconds:              5,
				ProfileCacheTTLMinutes:      60,
				ConversationCacheTTLMinutes: 15,
			},
			Files: config.SlackFilesConfig{
				MaxBytesPerFile: 5 * 1024 * 1024, MaxProcessedChars: 20_000,
				TranscriptionTimeoutSeconds: 120,
			},
		},
		Sandbox:  config.SandboxConfig{Enabled: true, Projects: map[string]string{"workspace": "."}, CommandTimeoutSeconds: 30, MaxOutputBytes: 65536},
		Canvases: config.CanvasesConfig{MaxTitleChars: 150, MaxContentChars: 50000, MaxContentBytes: 5 * 1024 * 1024, TimeoutSeconds: 30},
		Exports:  config.ExportsConfig{MaxFilenameChars: 128, MaxContentBytes: 1024 * 1024, TimeoutSeconds: 30},
		ExternalAgent: config.ExternalAgentConfig{
			MaxInlineResultBytes:         64 * 1024,
			MaxResultArtifactBytes:       16 * 1024 * 1024,
			DefaultJobTimeoutSeconds:     7200,
			MaxJobTimeoutSeconds:         86400,
			ReconciliationTimeoutSeconds: 1800,
			ProgressWarningSeconds:       900,
			WorkerConcurrency:            1,
			ArtifactRetentionDays:        30,
			Delivery:                     config.ExternalAgentDeliveryConfig{MaxMarkdownParts: 6, MaxFileBytes: 16 * 1024 * 1024},
		},
		CodeIntelligence: &config.CodeIntelligenceConfig{Enabled: false, MaxProcesses: 4, InitTimeoutSeconds: 20, RequestTimeoutSeconds: 10},
		Orchestration: config.OrchestrationConfig{
			Workstreams: config.WorkstreamConfig{Enabled: false, MaxNonTerminalTasks: 32, MaxDependenciesPerTask: 8, SnapshotBudgetTokens: domain.DefaultWorkstreamSnapshotBudgetTokens},
			ResultHandles: config.ResultHandlesConfig{
				Enabled: false, MaxProducingCallsPerStep: 1, ProducingCallReserveTokens: 2_048,
				Retention: config.ResultRetentionConfig{
					ContextDays: domain.DefaultResultRetentionContextDays, ConversationDays: domain.DefaultResultRetentionConversationDays,
					WorkstreamDays: domain.DefaultResultRetentionWorkstreamDays, ExportedDays: domain.DefaultResultRetentionExportedDays,
				},
			},
			Knowledge: config.KnowledgeConfig{
				Enabled:                   false,
				ProjectionIntervalSeconds: 60,
				ProjectionMaxRetries:      3,
				ProjectionRetentionDays:   90,
				MaxCardTokens:             domain.DefaultMaxKnowledgeCardBudget,
				Retrieval: config.KnowledgeRetrievalConfig{
					Enabled:                 false,
					TimeoutSeconds:          domain.DefaultKnowledgeRetrievalTimeoutSeconds,
					MaxQueryRunes:           domain.DefaultKnowledgeRetrievalMaxQueryRunes,
					MaxCandidatesPerChannel: domain.DefaultKnowledgeRetrievalMaxCandidatesPerChannel,
					MaxCards:                domain.DefaultKnowledgeRetrievalMaxCards,
					MaxDocumentBytes:        domain.DefaultKnowledgeRetrievalMaxDocumentBytes,
					WorkerIntervalSeconds:   domain.DefaultKnowledgeRetrievalWorkerIntervalSeconds,
					WorkerMaxRetries:        domain.DefaultKnowledgeRetrievalWorkerMaxRetries,
					WorkerBatchSize:         domain.DefaultKnowledgeRetrievalWorkerBatchSize,
					Embedding: config.KnowledgeEmbeddingConfig{
						Enabled:        false,
						TimeoutSeconds: domain.DefaultKnowledgeEmbeddingTimeoutSeconds,
					},
				},
			},
			ResultAnalysis: config.ResultAnalysisConfig{
				Enabled:               false,
				MaxSegmentBytes:       24576,
				OverlapBasisPoints:    1000,
				OverlapMaxBytes:       4096,
				MaxLeaves:             64,
				MaxReductionFanIn:     8,
				MaxReductionDepth:     4,
				MaxConcurrentLeaves:   2,
				MaxAttemptsPerStep:    2,
				CallTimeoutSeconds:    120,
				WallTimeSeconds:       900,
				WorkerIntervalSeconds: 5,
				Evidence: config.ResultAnalysisEvidenceConfig{
					ExcerptBytes:        2048,
					SelectorsPerLeaf:    8,
					ReferencesPerPacket: 32,
					BundleBytes:         32768,
				},
			},
		},
	}

	got := config.Default()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Default() mismatch\n got: %#v\nwant: %#v", got, want)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("default config must validate: %v", err)
	}
}

func TestADKCompactionDefaultsAndProductionValidation(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	if cfg.Context.ADKCompaction == nil || !cfg.Context.ADKCompaction.Enabled || cfg.Context.ADKCompaction.MaxHistoryChars != 120000 || cfg.Context.ADKCompaction.RecentTurns != 8 ||
		!cfg.Context.ADKCompaction.SummaryEnabled ||
		cfg.Context.ADKCompaction.SummaryMaxChars != 8000 ||
		cfg.Context.ADKCompaction.SummaryBudgetTokens != 2048 {
		t.Fatalf("unexpected ADK compaction defaults: %#v", cfg.Context.ADKCompaction)
	}
	cfg.Context.ADKCompaction.Enabled = false
	if err := config.ValidateADKCompaction(cfg, true, true); err == nil || !strings.Contains(err.Error(), "must be true") {
		t.Fatalf("disabled durable compaction error = %v", err)
	}
	cfg.Context.ADKCompaction.Enabled = true
	cfg.Context.ADKCompaction.MaxHistoryChars = cfg.Context.ADKCompaction.SummaryMaxChars + 1000
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "reserve more than 1000") {
		t.Fatalf("insufficient compaction reserve error = %v", err)
	}
	cfg = config.Default()
	cfg.Context.ADKCompaction.SummaryMaxChars = 9000
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "SQLite summary limit") {
		t.Fatalf("SQLite summary limit error = %v", err)
	}
	cfg = config.Default()
	cfg.Context.ADKCompaction.SummaryBudgetTokens = 0
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "summary_budget_tokens") {
		t.Fatalf("summary source budget error = %v", err)
	}
	parsed, err := config.Parse([]byte("context:\n  adk_compaction:\n    summary_budget_tokens: 1024\n"))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Context.ADKCompaction == nil || parsed.Context.ADKCompaction.SummaryBudgetTokens != 1024 {
		t.Fatalf("parsed summary source budget = %#v", parsed.Context.ADKCompaction)
	}
}

func TestExternalAgentDeliveryPolicyValidation(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.ExternalAgent.Delivery.MaxMarkdownParts = 9
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "max_markdown_parts") {
		t.Fatalf("part policy error = %v", err)
	}
	cfg = config.Default()
	cfg.ExternalAgent.Delivery.MaxFileBytes = cfg.ExternalAgent.MaxResultArtifactBytes + 1
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "max_file_bytes") {
		t.Fatalf("file policy error = %v", err)
	}
	cfg = config.Default()
	cfg.ExternalAgent.Delivery.MaxMarkdownParts = 1
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "delivery capacity") {
		t.Fatalf("inline capacity error = %v", err)
	}
}

func TestExternalAgentDeliveryFileBoundDefaultsToConfiguredArtifactBound(t *testing.T) {
	t.Parallel()
	cfg, err := config.Parse([]byte("acp:\n  max_result_artifact_bytes: 1048576\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ExternalAgent.Delivery.MaxFileBytes != cfg.ExternalAgent.MaxResultArtifactBytes {
		t.Fatalf("max_file_bytes = %d, artifact bound = %d", cfg.ExternalAgent.Delivery.MaxFileBytes, cfg.ExternalAgent.MaxResultArtifactBytes)
	}
}

func TestMarshalDefaultYAML(t *testing.T) {
	t.Parallel()

	got, err := config.Marshal(config.Default())
	if err != nil {
		t.Fatalf("Marshal() error: %v", err)
	}
	want := `state:
  dir: .local-agent
  db: .local-agent/local-agent.db
context:
  max_messages: 30
  max_chars: 20000
  retain_messages_per_conversation: 100
  adk_compaction:
    enabled: true
    max_history_chars: 120000
     recent_turns: 8
     summary_enabled: true
     summary_max_chars: 8000
     summary_budget_tokens: 2048
  model_budget:
    max_request_percent: 60
   recoverable_results:
     max_result_bytes: 4194304
     chunk_max_bytes: 16384
     retention_days: 7
     cleanup_batch_size: 100
   context_features:
     model_budget_enabled: false
     recoverable_results_enabled: false
     continuity_capsule_enabled: false
runtime:
  log_level: info
  model_timeout_seconds: 0
  slack_api_timeout_seconds: 30
  max_concurrent_model_calls: 4
  shutdown_grace_seconds: 30
  busy_message: El bot está ocupado procesando otras solicitudes. Intenta de nuevo en unos minutos.
  model_error_message: No pude completar la respuesta por un error del modelo. Intenta de nuevo.
slack:
  app_name: Local Agent
  bot_display_name: Dev Agent
  unauthorized_message: No tienes permiso para usar este bot. Pide acceso a quien administra local-agent.
  allow_all_users: false
  allowed_user_ids: []
  allowed_team_ids: []
  allowed_channel_ids: []
  part_labels: true
  standard_agent:
    threaded_dm: false
    progress_enabled: false
    prompts_enabled: false
    suggested_prompts:
      - Resume el contexto y destaca las decisiones pendientes.
      - Analiza el proyecto y señala los riesgos principales.
      - Prepara un plan de implementación verificable.
    streaming_enabled: false
    update_interval_seconds: 3
    progress_labels: {}
  context:
    enabled: false
    max_chars: 1500
    timeout_seconds: 5
    profile_cache_ttl_minutes: 60
    conversation_cache_ttl_minutes: 15
	files:
    max_bytes_per_file: 5242880
    max_processed_chars: 20000
    transcription_profile: ""
    transcription_timeout_seconds: 120
sandbox:
  enabled: true
  projects:
    workspace: .
  command_timeout_seconds: 30
  max_output_bytes: 65536
canvases:
  enabled: false
  max_title_chars: 150
  max_content_chars: 50000
  max_content_bytes: 5242880
  timeout_seconds: 30
exports:
  enabled: false
  max_filename_chars: 128
  max_content_bytes: 1048576
  timeout_seconds: 30
acp:
  max_inline_result_bytes: 65536
  max_result_artifact_bytes: 16777216
   default_job_timeout_seconds: 7200
   max_job_timeout_seconds: 86400
   reconciliation_timeout_seconds: 1800
   progress_warning_seconds: 900
   worker_concurrency: 1
   artifact_retention_days: 30
   delivery:
     max_markdown_parts: 6
     max_file_bytes: 16777216
code_intelligence:
  enabled: false
  max_processes: 4
  initialization_timeout_seconds: 20
  request_timeout_seconds: 10
  lsp_servers: []
   lsp_routes: {}
orchestration:
  workstreams:
    enabled: false
    max_non_terminal_tasks: 32
    max_dependencies_per_task: 8
    snapshot_budget_tokens: 2048
  result_handles:
    enabled: false
    max_producing_calls_per_step: 1
    producing_call_reserve_tokens: 2048
    retention:
      context_days: 7
      conversation_days: 30
      workstream_days: 180
      exported_days: 30
  knowledge:
    enabled: false
    projection_interval_seconds: 60
    projection_max_retries: 3
    projection_retention_days: 90
    max_card_tokens: 1024
    retrieval:
      enabled: false
      timeout_seconds: 2
      max_query_runes: 2048
      max_candidates_per_channel: 32
      max_cards: 8
      max_document_bytes: 65536
      worker_interval_seconds: 10
      worker_max_retries: 3
      worker_batch_size: 32
      embedding:
        enabled: false
        provider_id: ""
        base_url: ""
        api_key_env: ""
        model: ""
        dimensions: 0
        min_similarity_basis_points: 0
        timeout_seconds: 5
  result_analysis:
    enabled: false
    max_segment_bytes: 24576
    overlap_basis_points: 1000
    overlap_max_bytes: 4096
    max_leaves: 64
    max_reduction_fan_in: 8
    max_reduction_depth: 4
    max_concurrent_leaves: 2
    max_attempts_per_step: 2
    call_timeout_seconds: 120
    wall_time_seconds: 900
    worker_interval_seconds: 5
    evidence:
      excerpt_bytes: 2048
      selectors_per_leaf: 8
      references_per_packet: 32
      bundle_bytes: 32768
    model:
      enabled: false
      provider_id: ""
      base_url: ""
      api_key_env: ""
      model: ""
      reasoning_effort: ""
           `

	if !reflect.DeepEqual(strings.Fields(string(got)), strings.Fields(want)) {
		t.Fatalf("default YAML fields mismatch\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

func TestParseAppliesOnlyMissingDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := config.Parse([]byte(`slack:
  allow_all_users: true
  allowed_user_ids: null
  files:
    max_bytes_per_file: 1048576
    max_processed_chars: 4096
    transcription_profile: openai/stt
    transcription_timeout_seconds: 45
`))
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}

	if cfg.Slack.AllowedUserIDs == nil || len(cfg.Slack.AllowedUserIDs) != 0 {
		t.Fatalf("allowed_user_ids should normalize to an empty slice: %#v", cfg.Slack.AllowedUserIDs)
	}
	if cfg.Slack.Files.MaxBytesPerFile != 1048576 || cfg.Slack.Files.MaxProcessedChars != 4096 || cfg.Slack.Files.TranscriptionProfile != "openai/stt" ||
		cfg.Slack.Files.TranscriptionTimeoutSeconds != 45 {
		t.Fatalf("slack.files overrides not decoded: %#v", cfg.Slack.Files)
	}
}

func TestTranscriptionConfigRoundTrip(t *testing.T) {
	t.Parallel()

	want := config.Default()
	want.Slack.Files.TranscriptionProfile = "openai/stt"
	want.Slack.Files.TranscriptionTimeoutSeconds = 45
	data, err := config.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal() error: %v", err)
	}
	got, err := config.Parse(data)
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	if got.Slack.Files.TranscriptionProfile != want.Slack.Files.TranscriptionProfile || got.Slack.Files.TranscriptionTimeoutSeconds != want.Slack.Files.TranscriptionTimeoutSeconds {
		t.Fatalf("transcription config round trip = %#v, want %#v", got.Slack.Files, want.Slack.Files)
	}
}

func TestParseEmptyOrCommentOnlyUsesDefaults(t *testing.T) {
	t.Parallel()

	for _, input := range []string{"", "   \n", "# intentionally using defaults\n"} {
		cfg, err := config.Parse([]byte(input))
		if err != nil {
			t.Fatalf("Parse(%q) error: %v", input, err)
		}
		want := config.Default()
		if !reflect.DeepEqual(cfg.State, want.State) ||
			!reflect.DeepEqual(cfg.Context, want.Context) ||
			!reflect.DeepEqual(cfg.Runtime, want.Runtime) ||
			!reflect.DeepEqual(cfg.Slack, want.Slack) {
			t.Fatalf("Parse(%q) did not produce defaults: %#v", input, cfg)
		}
	}
}

func TestParseLegacyYAMLReceivesADKCompactionDefaults(t *testing.T) {
	cfg, err := config.Parse([]byte("agent:\n  name: Legacy\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Context.ADKCompaction == nil || !cfg.Context.ADKCompaction.Enabled || cfg.Context.ADKCompaction.MaxHistoryChars != 120000 || cfg.Context.ADKCompaction.SummaryMaxChars != 8000 ||
		cfg.Context.ADKCompaction.SummaryBudgetTokens != 2048 {
		t.Fatalf("legacy compaction defaults = %#v", cfg.Context.ADKCompaction)
	}
}

func TestParseAndMarshalPreserveUnknownFieldsAndComments(t *testing.T) {
	t.Parallel()

	input := []byte(`# operator note
plugin_extension:
  enabled: true
slack:
  app_name: Old Name # keep this comment
  tone: terse
`)
	cfg, err := config.Parse(input)
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	cfg.Slack.AppName = "New Name"

	output, err := config.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal() error: %v", err)
	}
	text := string(output)
	for _, fragment := range []string{
		"# operator note",
		"app_name: New Name # keep this comment",
		"tone: terse",
		"plugin_extension:",
		"enabled: true",
	} {
		if !strings.Contains(text, fragment) {
			t.Errorf("output lost %q:\n%s", fragment, text)
		}
	}
}

func TestParseRejectsMalformedDocuments(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"sequence root":      "- invalid\n",
		"multiple documents": "agent: {}\n---\nagent: {}\n",
		"duplicate key":      "agent:\n  name: one\n  name: two\n",
		"wrong typed value":  "context:\n  max_messages: many\n",
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := config.Parse([]byte(input)); err == nil {
				t.Fatal("Parse() unexpectedly succeeded")
			}
		})
	}
}

func TestValidationReportsTypedFieldErrors(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	cfg.State.DB = ""
	cfg.Context.MaxMessages = 0
	cfg.Runtime.LogLevel = "verbose"
	cfg.Runtime.ModelTimeoutSeconds = -1
	cfg.Runtime.SlackAPITimeoutSeconds = -1
	cfg.Runtime.MaxConcurrentModelCalls = 0
	cfg.Runtime.ShutdownGraceSeconds = 0
	cfg.ExternalAgent.ReconciliationTimeoutSeconds = 0
	cfg.Slack.AllowedUserIDs = []string{"not-a-user"}
	cfg.Slack.AllowedTeamIDs = []string{"U12345678"}
	cfg.Slack.AllowedChannelIDs = []string{"D12345678"}
	cfg.Slack.Files.MaxBytesPerFile = 5*1024*1024 + 1
	cfg.Slack.Files.MaxProcessedChars = 20_001
	cfg.Slack.Files.TranscriptionProfile = "invalid-profile"
	cfg.Slack.Files.TranscriptionTimeoutSeconds = 0

	err := cfg.Validate()
	var validation *config.ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("Validate() error type = %T, want *config.ValidationError: %v", err, err)
	}
	for _, field := range []string{
		"state.db",
		"context.max_messages",
		"runtime.log_level",
		"runtime.model_timeout_seconds",
		"runtime.slack_api_timeout_seconds",
		"runtime.max_concurrent_model_calls",
		"runtime.shutdown_grace_seconds",
		"acp.reconciliation_timeout_seconds",
		"slack.allowed_user_ids[0]",
		"slack.allowed_team_ids[0]",
		"slack.allowed_channel_ids[0]",
		"slack.files.max_bytes_per_file",
		"slack.files.max_processed_chars",
		"slack.files.transcription_profile",
		"slack.files.transcription_timeout_seconds",
	} {
		if !validation.Has(field) {
			t.Errorf("validation did not report %s: %v", field, err)
		}
	}
}

func TestValidateAcceptsConfiguredAccessListsAndHeaders(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	cfg.Slack.AllowedUserIDs = []string{"U12345678", "W12345678"}
	cfg.Slack.AllowedTeamIDs = []string{"T12345678"}
	cfg.Slack.AllowedChannelIDs = []string{"C12345678", "G12345678"}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error: %v", err)
	}
}

func TestParseAppliesProgressLabels(t *testing.T) {
	t.Parallel()
	cfg, err := config.Parse([]byte(`slack:
  standard_agent:
    progress_labels:
      working: Pensando
      waiting_confirmation: Revisión pendiente
      finalizing: Finalizando
      cleared: Listo
      failed: Error
      interrupted: Interrumpido
`))
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	want := map[domain.ProgressState]string{
		domain.ProgressWorking:             "Pensando",
		domain.ProgressWaitingConfirmation: "Revisión pendiente",
		domain.ProgressFinalizing:          "Finalizando",
		domain.ProgressCleared:             "Listo",
		domain.ProgressFailed:              "Error",
		domain.ProgressInterrupted:         "Interrumpido",
	}
	if !reflect.DeepEqual(cfg.Slack.StandardAgent.ProgressLabels, want) {
		t.Fatalf("progress labels = %#v, want %#v", cfg.Slack.StandardAgent.ProgressLabels, want)
	}
}

func TestParseMissingProgressLabelsStayEmpty(t *testing.T) {
	t.Parallel()
	cfg, err := config.Parse([]byte(`agent:
  name: minimal
`))
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	if labels := cfg.Slack.StandardAgent.ProgressLabels; labels == nil || len(labels) != 0 {
		t.Fatalf("absent progress_labels should resolve to an empty map: %#v", labels)
	}
}

func TestParseRejectsUnknownProgressLabelKeys(t *testing.T) {
	t.Parallel()
	_, err := config.Parse([]byte(`slack:
  standard_agent:
    progress_labels:
      working: Pensando
      unknown_state: Surprise
`))
	if err == nil || !strings.Contains(err.Error(), "unknown_state") || !strings.Contains(err.Error(), "progress_labels") {
		t.Fatalf("Parse() error = %v, want unknown progress state error", err)
	}
}

func TestValidateRejectsEmptyProgressLabel(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.Slack.StandardAgent.ProgressLabels = map[domain.ProgressState]string{
		domain.ProgressWorking: "",
	}
	err := cfg.Validate()
	var validation *config.ValidationError
	if !errors.As(err, &validation) || !validation.Has(`slack.standard_agent.progress_labels["working"]`) {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestValidateRejectsOversizedProgressLabels(t *testing.T) {
	t.Parallel()
	for name, label := range map[string]string{
		"ascii":     strings.Repeat("a", domain.ProgressLabelMaxRunes+1),
		"multibyte": strings.Repeat("界", domain.ProgressLabelMaxRunes+1),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := config.Default()
			cfg.Slack.StandardAgent.ProgressLabels = map[domain.ProgressState]string{
				domain.ProgressWorking: label,
			}
			err := cfg.Validate()
			var validation *config.ValidationError
			if !errors.As(err, &validation) || !validation.Has(`slack.standard_agent.progress_labels["working"]`) {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}
}

func TestValidateProgressLabelLimitCountsRunesNotBytes(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.Slack.StandardAgent.ProgressLabels = map[domain.ProgressState]string{
		domain.ProgressWorking: strings.Repeat("界", domain.ProgressLabelMaxRunes),
		domain.ProgressCleared: strings.Repeat("é", 6000),
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("labels within the Unicode code point limit should validate, got %v", err)
	}
}

func TestValidateStandardAgentFeaturesRequireThreadedDMAndBoundedPrompts(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.Slack.StandardAgent.ProgressEnabled = true
	cfg.Slack.StandardAgent.PromptsEnabled = true
	cfg.Slack.StandardAgent.SuggestedPrompts = []string{"one", "two", "three", "four", "five", "six"}
	err := cfg.Validate()
	var validation *config.ValidationError
	if !errors.As(err, &validation) || !validation.Has("slack.standard_agent.threaded_dm") || !validation.Has("slack.standard_agent.suggested_prompts") {
		t.Fatalf("Validate() error = %v", err)
	}

	cfg.Slack.StandardAgent.ThreadedDM = true
	cfg.Slack.StandardAgent.SuggestedPrompts = []string{"one"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid standard agent config: %v", err)
	}
}

func TestValidateRejectsContextTimeoutAboveSlackAPITimeout(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.Slack.Context.Enabled = true
	cfg.Runtime.SlackAPITimeoutSeconds = 1
	cfg.Slack.Context.TimeoutSeconds = 2
	err := cfg.Validate()
	var validation *config.ValidationError
	if !errors.As(err, &validation) || !validation.Has("slack.context.timeout_seconds") {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestValidateWorkstreamLimitsAndParseGate(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.Orchestration.Workstreams.Enabled = true
	cfg.Orchestration.Workstreams.MaxNonTerminalTasks = domain.HardMaxWorkstreamTasks + 1
	cfg.Orchestration.Workstreams.MaxDependenciesPerTask = domain.HardMaxWorkstreamDependencies + 1
	err := cfg.Validate()
	var validation *config.ValidationError
	if !errors.As(err, &validation) || !validation.Has("orchestration.workstreams.max_non_terminal_tasks") || !validation.Has("orchestration.workstreams.max_dependencies_per_task") {
		t.Fatalf("workstream validation = %v", err)
	}
	parsed, err := config.Parse([]byte(`orchestration:
  workstreams:
    enabled: true
    max_non_terminal_tasks: 12
    max_dependencies_per_task: 4
  result_handles:
    enabled: true
`))
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.Orchestration.Workstreams.Enabled || parsed.Orchestration.Workstreams.MaxNonTerminalTasks != 12 || parsed.Orchestration.Workstreams.MaxDependenciesPerTask != 4 ||
		!parsed.Orchestration.ResultHandles.Enabled {
		t.Fatalf("parsed orchestration config = %+v", parsed.Orchestration)
	}
	if parsed.Orchestration.ResultHandles.MaxProducingCallsPerStep != 1 || parsed.Orchestration.ResultHandles.ProducingCallReserveTokens != 2_048 {
		t.Fatalf("parsed result-handle reservation = %+v", parsed.Orchestration.ResultHandles)
	}
	cfg = config.Default()
	cfg.Orchestration.ResultHandles.MaxProducingCallsPerStep = 2
	cfg.Orchestration.ResultHandles.ProducingCallReserveTokens = 0
	err = cfg.Validate()
	if !errors.As(err, &validation) || !validation.Has("orchestration.result_handles.max_producing_calls_per_step") || !validation.Has("orchestration.result_handles.producing_call_reserve_tokens") {
		t.Fatalf("result-handle reservation validation = %v", err)
	}
	emptyRegistry := config.Default()
	emptyRegistry.Orchestration.Workstreams.Enabled = true
	emptyRegistry.Sandbox.Projects = nil
	err = emptyRegistry.Validate()
	if !errors.As(err, &validation) || !validation.Has("orchestration.workstreams") {
		t.Fatalf("workstream project registry validation = %v", err)
	}
}

// TestValidateResultRetentionDaysBounds pins hallazgo 6: each of the four
// TRD 02 retention classes has an independently validated, configurable age
// in days, bounded to [1, domain.HardMaxResultRetentionDays].
func TestValidateResultRetentionDaysBounds(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.Orchestration.ResultHandles.Retention = config.ResultRetentionConfig{
		ContextDays: 0, ConversationDays: -1, WorkstreamDays: domain.HardMaxResultRetentionDays + 1, ExportedDays: domain.HardMaxResultRetentionDays,
	}
	err := cfg.Validate()
	var validation *config.ValidationError
	if !errors.As(err, &validation) ||
		!validation.Has("orchestration.result_handles.retention.context_days") ||
		!validation.Has("orchestration.result_handles.retention.conversation_days") ||
		!validation.Has("orchestration.result_handles.retention.workstream_days") ||
		validation.Has("orchestration.result_handles.retention.exported_days") {
		t.Fatalf("retention days validation = %v", err)
	}
}

func TestKnowledgeGateDefaultParseSchemaAndRoundTrip(t *testing.T) {
	t.Parallel()
	if config.Default().Orchestration.Knowledge.Enabled {
		t.Fatal("knowledge gate must default to disabled")
	}
	parsed, err := config.Parse([]byte(`orchestration:
  knowledge:
    enabled: true
`))
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.Orchestration.Knowledge.Enabled {
		t.Fatalf("parsed knowledge gate = %+v", parsed.Orchestration.Knowledge)
	}
	if err := parsed.Validate(); err != nil {
		t.Fatalf("enabled knowledge gate must validate: %v", err)
	}
	rendered, err := config.Marshal(parsed)
	if err != nil {
		t.Fatal(err)
	}
	roundTripped, err := config.Parse(rendered)
	if err != nil {
		t.Fatal(err)
	}
	if !roundTripped.Orchestration.Knowledge.Enabled {
		t.Fatalf("round-tripped knowledge gate = %+v", roundTripped.Orchestration.Knowledge)
	}
	withExtra, err := config.Parse([]byte(`orchestration:
  knowledge:
    enabled: true
    future_extension: preserved
`))
	if err != nil {
		t.Fatal(err)
	}
	if !withExtra.Orchestration.Knowledge.Enabled {
		t.Fatalf("knowledge gate with extension = %+v", withExtra.Orchestration.Knowledge)
	}
	preserved, err := config.Marshal(withExtra)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(preserved), "future_extension: preserved") || !strings.Contains(string(preserved), "enabled: true") {
		t.Fatalf("extension field was not preserved: %s", preserved)
	}
}

func TestKnowledgeProjectionConfigDefaultsParseSchemaAndRoundTrip(t *testing.T) {
	t.Parallel()
	def := config.Default().Orchestration.Knowledge
	if def.ProjectionIntervalSeconds != 60 || def.ProjectionMaxRetries != 3 || def.ProjectionRetentionDays != 90 {
		t.Fatalf("projection defaults = %+v", def)
	}
	parsed, err := config.Parse([]byte(`orchestration:
  knowledge:
    enabled: true
    projection_interval_seconds: 15
    projection_max_retries: 7
    projection_retention_days: 30
`))
	if err != nil {
		t.Fatal(err)
	}
	got := parsed.Orchestration.Knowledge
	if !got.Enabled || got.ProjectionIntervalSeconds != 15 || got.ProjectionMaxRetries != 7 || got.ProjectionRetentionDays != 30 {
		t.Fatalf("parsed projection settings = %+v", got)
	}
	if err := parsed.Validate(); err != nil {
		t.Fatalf("valid projection settings must validate: %v", err)
	}
	rendered, err := config.Marshal(parsed)
	if err != nil {
		t.Fatal(err)
	}
	roundTripped, err := config.Parse(rendered)
	if err != nil {
		t.Fatal(err)
	}
	if roundTripped.Orchestration.Knowledge != got {
		t.Fatalf("projection round trip = %+v, want %+v", roundTripped.Orchestration.Knowledge, got)
	}
	withExtra, err := config.Parse([]byte(`orchestration:
  knowledge:
    projection_interval_seconds: 45
    projection_extra: preserved
`))
	if err != nil {
		t.Fatal(err)
	}
	renderedExtra, err := config.Marshal(withExtra)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(renderedExtra), "projection_extra: preserved") || !strings.Contains(string(renderedExtra), "projection_interval_seconds: 45") {
		t.Fatalf("projection extension was not preserved: %s", renderedExtra)
	}
}

func TestKnowledgeProjectionConfigValidationRejectsInvalidValues(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*config.Config){
		"interval": func(c *config.Config) { c.Orchestration.Knowledge.ProjectionIntervalSeconds = 0 },
		"retries":  func(c *config.Config) { c.Orchestration.Knowledge.ProjectionMaxRetries = -1 },
		"retention": func(c *config.Config) {
			c.Orchestration.Knowledge.ProjectionRetentionDays = 0
		},
	} {
		cfg := config.Default()
		mutate(&cfg)
		var validation *config.ValidationError
		if err := cfg.Validate(); !errors.As(err, &validation) {
			t.Fatalf("%s: invalid projection settings error = %v", name, err)
		}
	}
}

func TestKnowledgeRetrievalConfigDefaultsKeepEverythingDisabled(t *testing.T) {
	t.Parallel()
	def := config.Default().Orchestration.Knowledge
	if def.MaxCardTokens != domain.DefaultMaxKnowledgeCardBudget {
		t.Fatalf("max card tokens default = %d", def.MaxCardTokens)
	}
	if def.Retrieval.Enabled || def.Retrieval.Embedding.Enabled {
		t.Fatalf("retrieval defaults must be disabled: %+v", def.Retrieval)
	}
	if def.Retrieval.TimeoutSeconds != 2 || def.Retrieval.MaxQueryRunes != 2048 ||
		def.Retrieval.MaxCandidatesPerChannel != 32 || def.Retrieval.MaxCards != 8 ||
		def.Retrieval.MaxDocumentBytes != 65536 || def.Retrieval.WorkerIntervalSeconds != 10 ||
		def.Retrieval.WorkerMaxRetries != 3 || def.Retrieval.WorkerBatchSize != 32 {
		t.Fatalf("retrieval defaults diverged: %+v", def.Retrieval)
	}
	if def.Retrieval.Embedding.TimeoutSeconds != 5 || def.Retrieval.Embedding.Dimensions != 0 || def.Retrieval.Embedding.MinSimilarityBasisPoints != 0 {
		t.Fatalf("embedding defaults diverged: %+v", def.Retrieval.Embedding)
	}
	if err := config.Default().Validate(); err != nil {
		t.Fatalf("default config with retrieval fields must validate: %v", err)
	}
}

func TestKnowledgeRetrievalConfigParseSchemaAndRoundTrip(t *testing.T) {
	t.Parallel()
	parsed, err := config.Parse([]byte(`context:
  context_features:
    model_budget_enabled: true
    recoverable_results_enabled: true
orchestration:
  knowledge:
    enabled: true
    max_card_tokens: 2048
    retrieval:
      enabled: true
      timeout_seconds: 4
      max_query_runes: 3000
      max_candidates_per_channel: 48
      max_cards: 12
      max_document_bytes: 131072
      worker_interval_seconds: 20
      worker_max_retries: 5
      worker_batch_size: 64
      embedding:
        enabled: true
        provider_id: internal-embeddings
        base_url: https://embeddings.internal.example
        api_key_env: EMBEDDING_API_KEY
        model: text-embedding-local
        dimensions: 768
        min_similarity_basis_points: 7000
        timeout_seconds: 15
`))
	if err != nil {
		t.Fatal(err)
	}
	got := parsed.Orchestration.Knowledge
	if !got.Enabled || got.MaxCardTokens != 2048 {
		t.Fatalf("parsed knowledge retrieval gate = %+v", got)
	}
	retrieval := got.Retrieval
	if !retrieval.Enabled || retrieval.TimeoutSeconds != 4 || retrieval.MaxQueryRunes != 3000 ||
		retrieval.MaxCandidatesPerChannel != 48 || retrieval.MaxCards != 12 ||
		retrieval.MaxDocumentBytes != 131072 || retrieval.WorkerIntervalSeconds != 20 ||
		retrieval.WorkerMaxRetries != 5 || retrieval.WorkerBatchSize != 64 {
		t.Fatalf("parsed retrieval settings = %+v", retrieval)
	}
	embedding := retrieval.Embedding
	if !embedding.Enabled || embedding.ProviderID != "internal-embeddings" ||
		embedding.BaseURL != "https://embeddings.internal.example" ||
		embedding.APIKeyEnv != "EMBEDDING_API_KEY" || embedding.Model != "text-embedding-local" ||
		embedding.Dimensions != 768 || embedding.MinSimilarityBasisPoints != 7000 || embedding.TimeoutSeconds != 15 {
		t.Fatalf("parsed embedding settings = %+v", embedding)
	}
	if err := parsed.Validate(); err != nil {
		t.Fatalf("valid retrieval settings must validate: %v", err)
	}
	rendered, err := config.Marshal(parsed)
	if err != nil {
		t.Fatal(err)
	}
	roundTripped, err := config.Parse(rendered)
	if err != nil {
		t.Fatal(err)
	}
	if roundTripped.Orchestration.Knowledge != got {
		t.Fatalf("retrieval round trip = %+v, want %+v", roundTripped.Orchestration.Knowledge, got)
	}
}

func TestKnowledgeRetrievalConfigPreservesExtensions(t *testing.T) {
	t.Parallel()
	withExtra, err := config.Parse([]byte(`context:
  context_features:
    model_budget_enabled: true
    recoverable_results_enabled: true
orchestration:
  knowledge:
    enabled: true
    retrieval:
      enabled: true
      future_retrieval_extra: preserved
      embedding:
        future_embedding_extra: preserved
`))
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := config.Marshal(withExtra)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rendered), "future_retrieval_extra: preserved") || !strings.Contains(string(rendered), "future_embedding_extra: preserved") {
		t.Fatalf("retrieval extension fields were not preserved: %s", rendered)
	}
}

func TestKnowledgeRetrievalConfigValidationRejectsInvalidValues(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*config.Config){
		"card tokens zero": func(c *config.Config) { c.Orchestration.Knowledge.MaxCardTokens = 0 },
		"card tokens over hard max": func(c *config.Config) {
			c.Orchestration.Knowledge.MaxCardTokens = domain.HardMaxKnowledgeCardBudget + 1
		},
		"timeout zero": func(c *config.Config) { c.Orchestration.Knowledge.Retrieval.TimeoutSeconds = 0 },
		"timeout over hard max": func(c *config.Config) {
			c.Orchestration.Knowledge.Retrieval.TimeoutSeconds = domain.HardMaxKnowledgeRetrievalTimeoutSeconds + 1
		},
		"query runes over hard max": func(c *config.Config) {
			c.Orchestration.Knowledge.Retrieval.MaxQueryRunes = domain.HardMaxKnowledgeRetrievalMaxQueryRunes + 1
		},
		"candidates over hard max": func(c *config.Config) {
			c.Orchestration.Knowledge.Retrieval.MaxCandidatesPerChannel = domain.HardMaxKnowledgeRetrievalMaxCandidatesPerChannel + 1
		},
		"cards over hard max": func(c *config.Config) {
			c.Orchestration.Knowledge.Retrieval.MaxCards = domain.HardMaxKnowledgeRetrievalMaxCards + 1
		},
		"document bytes over hard max": func(c *config.Config) {
			c.Orchestration.Knowledge.Retrieval.MaxDocumentBytes = domain.HardMaxKnowledgeRetrievalMaxDocumentBytes + 1
		},
		"worker interval over hard max": func(c *config.Config) {
			c.Orchestration.Knowledge.Retrieval.WorkerIntervalSeconds = domain.HardMaxKnowledgeRetrievalWorkerIntervalSeconds + 1
		},
		"worker retries over hard max": func(c *config.Config) {
			c.Orchestration.Knowledge.Retrieval.WorkerMaxRetries = domain.HardMaxKnowledgeRetrievalWorkerMaxRetries + 1
		},
		"worker batch over hard max": func(c *config.Config) {
			c.Orchestration.Knowledge.Retrieval.WorkerBatchSize = domain.HardMaxKnowledgeRetrievalWorkerBatchSize + 1
		},
		"embedding timeout over hard max": func(c *config.Config) {
			c.Orchestration.Knowledge.Retrieval.Embedding.TimeoutSeconds = domain.HardMaxKnowledgeEmbeddingTimeoutSeconds + 1
		},
		"embedding dimensions over hard max": func(c *config.Config) {
			c.Orchestration.Knowledge.Retrieval.Embedding.Dimensions = domain.HardMaxKnowledgeEmbeddingDimensions + 1
		},
		"negative embedding dimensions": func(c *config.Config) {
			c.Orchestration.Knowledge.Retrieval.Embedding.Dimensions = -1
		},
		"similarity over hard max": func(c *config.Config) {
			c.Orchestration.Knowledge.Retrieval.Embedding.MinSimilarityBasisPoints = domain.HardMaxKnowledgeMinSimilarityBasisPoints + 1
		},
		"retrieval without knowledge": func(c *config.Config) {
			c.Orchestration.Knowledge.Retrieval.Enabled = true
		},
		"retrieval without model budget": func(c *config.Config) {
			c.Orchestration.Knowledge.Enabled = true
			c.Orchestration.Knowledge.Retrieval.Enabled = true
		},
		"embedding without retrieval": func(c *config.Config) {
			c.Orchestration.Knowledge.Retrieval.Embedding.Enabled = true
		},
		"bad api key env": func(c *config.Config) {
			c.Orchestration.Knowledge.Retrieval.Embedding.APIKeyEnv = "not a name"
		},
		"unbounded api key env": func(c *config.Config) {
			c.Orchestration.Knowledge.Retrieval.Embedding.APIKeyEnv = strings.Repeat("E", 129)
		},
		"multiline provider id": func(c *config.Config) {
			c.Orchestration.Knowledge.Retrieval.Embedding.ProviderID = "two\nlines"
		},
	} {
		cfg := config.Default()
		mutate(&cfg)
		var validation *config.ValidationError
		if err := cfg.Validate(); !errors.As(err, &validation) {
			t.Fatalf("%s: invalid retrieval settings error = %v", name, err)
		}
	}
}

func TestKnowledgeEmbeddingEnabledRequiresFullConfiguration(t *testing.T) {
	t.Parallel()
	enable := func() config.Config {
		cfg := config.Default()
		cfg.Context.ContextFeatures.ModelBudgetEnabled = true
		cfg.Context.ContextFeatures.RecoverableResultsEnabled = true
		cfg.Orchestration.Knowledge.Enabled = true
		cfg.Orchestration.Knowledge.Retrieval.Enabled = true
		cfg.Orchestration.Knowledge.Retrieval.Embedding.Enabled = true
		cfg.Orchestration.Knowledge.Retrieval.Embedding.ProviderID = "internal"
		cfg.Orchestration.Knowledge.Retrieval.Embedding.BaseURL = "https://embeddings.internal.example"
		cfg.Orchestration.Knowledge.Retrieval.Embedding.APIKeyEnv = "EMBEDDING_API_KEY"
		cfg.Orchestration.Knowledge.Retrieval.Embedding.Model = "text-embedding-local"
		cfg.Orchestration.Knowledge.Retrieval.Embedding.Dimensions = 768
		cfg.Orchestration.Knowledge.Retrieval.Embedding.MinSimilarityBasisPoints = 7000
		return cfg
	}
	if err := enable().Validate(); err != nil {
		t.Fatalf("fully configured embedding must validate: %v", err)
	}
	for name, mutate := range map[string]func(*config.Config){
		"empty provider":  func(c *config.Config) { c.Orchestration.Knowledge.Retrieval.Embedding.ProviderID = "" },
		"empty model":     func(c *config.Config) { c.Orchestration.Knowledge.Retrieval.Embedding.Model = "" },
		"empty key env":   func(c *config.Config) { c.Orchestration.Knowledge.Retrieval.Embedding.APIKeyEnv = "" },
		"zero dimensions": func(c *config.Config) { c.Orchestration.Knowledge.Retrieval.Embedding.Dimensions = 0 },
		"zero threshold":  func(c *config.Config) { c.Orchestration.Knowledge.Retrieval.Embedding.MinSimilarityBasisPoints = 0 },
		"empty base url":  func(c *config.Config) { c.Orchestration.Knowledge.Retrieval.Embedding.BaseURL = "" },
		"insecure non-loopback": func(c *config.Config) {
			c.Orchestration.Knowledge.Retrieval.Embedding.BaseURL = "http://embeddings.internal.example"
		},
		"base url userinfo": func(c *config.Config) {
			c.Orchestration.Knowledge.Retrieval.Embedding.BaseURL = "https://user:pass@embeddings.internal.example"
		},
		"base url query": func(c *config.Config) {
			c.Orchestration.Knowledge.Retrieval.Embedding.BaseURL = "https://embeddings.internal.example?token=x"
		},
		"base url fragment": func(c *config.Config) {
			c.Orchestration.Knowledge.Retrieval.Embedding.BaseURL = "https://embeddings.internal.example#frag"
		},
	} {
		cfg := enable()
		mutate(&cfg)
		var validation *config.ValidationError
		if err := cfg.Validate(); !errors.As(err, &validation) {
			t.Fatalf("%s: enabled embedding error = %v", name, err)
		}
	}
	loopback := enable()
	loopback.Orchestration.Knowledge.Retrieval.Embedding.BaseURL = "http://localhost:8080"
	if err := loopback.Validate(); err != nil {
		t.Fatalf("loopback http embedding base url rejected: %v", err)
	}
}

func TestKnowledgeRetrievalEnabledRequiresModelBudgetInParsedConfig(t *testing.T) {
	t.Parallel()
	if _, err := config.Parse([]byte(`orchestration:
  knowledge:
    enabled: true
    retrieval:
      enabled: true
`)); err == nil || !strings.Contains(err.Error(), "model_budget_enabled") {
		t.Fatalf("parsed retrieval without model budget error = %v", err)
	}
	// A fully enabled positive fixture passes: knowledge plus model budget
	// with the recovery dependency satisfied.
	if _, err := config.Parse([]byte(`context:
  context_features:
    model_budget_enabled: true
    recoverable_results_enabled: true
orchestration:
  knowledge:
    enabled: true
    retrieval:
      enabled: true
`)); err != nil {
		t.Fatalf("fully enabled retrieval fixture rejected: %v", err)
	}
}

func TestResolvePaths(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cfg := config.Default()
	cfg.Sandbox.Enabled = false
	cfg.Sandbox.Projects = map[string]string{}
	cfg.State.Dir = "var/state"
	cfg.State.DB = filepath.Join(root, "outside", "agent.db")

	paths, err := cfg.ResolvePaths(root)
	if err != nil {
		t.Fatalf("ResolvePaths() error: %v", err)
	}
	want := config.Paths{
		ProjectRoot:    root,
		StateDir:       filepath.Join(root, "var", "state"),
		DatabaseFile:   filepath.Join(root, "outside", "agent.db"),
		ConfigFile:     filepath.Join(root, ".local-agent", "config.yaml"),
		ManifestFile:   filepath.Join(root, ".local-agent", "app-manifest.local.yaml"),
		EnvExampleFile: filepath.Join(root, ".local-agent", "local.env.example"),
		EnvFile:        filepath.Join(root, ".env"),
		MemoryDir:      filepath.Join(root, "var", "state", "memory"),
		ArtifactDir:    filepath.Join(root, "var", "state", "artifacts"),
		ToolsDir:       filepath.Join(root, "var", "state", "tools"),
	}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("ResolvePaths()\n got: %#v\nwant: %#v", paths, want)
	}

	configPath, err := config.ConfigPath(root)
	if err != nil {
		t.Fatalf("ConfigPath() error: %v", err)
	}
	if configPath != want.ConfigFile {
		t.Fatalf("ConfigPath() = %q, want %q", configPath, want.ConfigFile)
	}
}

func TestSaveAndLoadPreserveFileModeAndExtensions(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "nested", "config.yaml")
	input := []byte("extension:\n  value: retained\n")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, input, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if err := config.Save(path, cfg); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("saved mode = %04o, want 0600", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "value: retained") || strings.Contains("\n"+string(data), "\nagent:\n") {
		t.Fatalf("saved data lost extensions or retained legacy agent config:\n%s", data)
	}
}

func TestSaveCreatesParentAndUsesNonSensitiveMode(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "missing", "config.yaml")
	if err := config.Save(path, config.Default()); err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Fatalf("new config mode = %04o, want 0644", got)
	}
}

func TestParseAppliesSandboxConfig(t *testing.T) {
	t.Parallel()

	cfg, err := config.Parse([]byte(`sandbox:
  enabled: true
  projects:
    workspace: .
    api: ../api
  command_timeout_seconds: 60
  max_output_bytes: 32768
`))
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	if !cfg.Sandbox.Enabled {
		t.Fatal("sandbox.enabled should be true")
	}
	if len(cfg.Sandbox.Projects) != 2 {
		t.Fatalf("sandbox.projects count = %d, want 2", len(cfg.Sandbox.Projects))
	}
	if cfg.Sandbox.Projects["workspace"] != "." {
		t.Fatalf("sandbox.projects[workspace] = %q", cfg.Sandbox.Projects["workspace"])
	}
	if cfg.Sandbox.CommandTimeoutSeconds != 60 {
		t.Fatalf("sandbox.command_timeout_seconds = %d", cfg.Sandbox.CommandTimeoutSeconds)
	}
	if cfg.Sandbox.MaxOutputBytes != 32768 {
		t.Fatalf("sandbox.max_output_bytes = %d", cfg.Sandbox.MaxOutputBytes)
	}
}

func TestParseSandboxEnabledByDefault(t *testing.T) {
	t.Parallel()

	cfg, err := config.Parse([]byte(`agent:
  name: minimal
`))
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	if !cfg.Sandbox.Enabled {
		t.Fatal("sandbox should be enabled by default")
	}
	if len(cfg.Sandbox.Projects) != 1 || cfg.Sandbox.Projects["workspace"] != "." {
		t.Fatalf("sandbox projects should register workspace by default: %v", cfg.Sandbox.Projects)
	}
}

func TestParseAppliesCanvasConfig(t *testing.T) {
	cfg, err := config.Parse([]byte(`canvases:
  enabled: true
  max_title_chars: 100
  max_content_chars: 2000
  max_content_bytes: 4096
  timeout_seconds: 12
`))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Canvases.Enabled || cfg.Canvases.MaxTitleChars != 100 || cfg.Canvases.MaxContentChars != 2000 || cfg.Canvases.MaxContentBytes != 4096 || cfg.Canvases.TimeoutSeconds != 12 {
		t.Fatalf("parsed canvases config = %#v", cfg.Canvases)
	}
}

func TestParseAndValidateExportConfig(t *testing.T) {
	cfg, err := config.Parse([]byte(`exports:
  enabled: true
  max_filename_chars: 80
  max_content_bytes: 65536
  timeout_seconds: 12
`))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Exports.Enabled || cfg.Exports.MaxFilenameChars != 80 || cfg.Exports.MaxContentBytes != 65536 || cfg.Exports.TimeoutSeconds != 12 {
		t.Fatalf("parsed exports config = %#v", cfg.Exports)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	cfg.Exports.MaxFilenameChars = 129
	cfg.Exports.MaxContentBytes = 1024*1024 + 1
	err = cfg.Validate()
	var validation *config.ValidationError
	if !errors.As(err, &validation) || !validation.Has("exports.max_filename_chars") || !validation.Has("exports.max_content_bytes") {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestValidateRejectsEnabledSandboxWithoutProjects(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.Sandbox.Enabled = true
	cfg.Sandbox.Projects = map[string]string{}
	err := cfg.Validate()
	var validation *config.ValidationError
	if !errors.As(err, &validation) || !validation.Has("sandbox.projects") {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestResolvePathsResolvesSandboxProjects(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cfg := config.Default()
	cfg.Sandbox.Enabled = true
	cfg.Sandbox.Projects = map[string]string{
		"workspace": ".",
		"api":       "../api",
		"frontend":  "/absolute/frontend",
	}

	paths, err := cfg.ResolvePaths(root)
	if err != nil {
		t.Fatalf("ResolvePaths() error: %v", err)
	}
	if paths.SandboxProjectRoots["workspace"] != root {
		t.Fatalf("workspace = %q, want %q", paths.SandboxProjectRoots["workspace"], root)
	}
	wantAPI := filepath.Join(filepath.Dir(root), "api")
	if paths.SandboxProjectRoots["api"] != wantAPI {
		t.Fatalf("api = %q, want %q", paths.SandboxProjectRoots["api"], wantAPI)
	}
	wantFrontend := "/absolute/frontend"
	if paths.SandboxProjectRoots["frontend"] != wantFrontend {
		t.Fatalf("frontend = %q, want %q", paths.SandboxProjectRoots["frontend"], wantFrontend)
	}
}

func TestPathResolvesEmptySandboxToNil(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cfg := config.Default()
	cfg.Sandbox.Enabled = false
	cfg.Sandbox.Projects = map[string]string{}
	paths, err := cfg.ResolvePaths(root)
	if err != nil {
		t.Fatalf("ResolvePaths() error: %v", err)
	}
	if paths.SandboxProjectRoots != nil {
		t.Fatalf("sandbox project roots should be nil for empty projects: %#v", paths.SandboxProjectRoots)
	}
}

func TestResolvePathsUsesCanonicalProjectRootForSandboxProjects(t *testing.T) {
	parent := t.TempDir()
	physicalRoot := filepath.Join(parent, "physical", "workspace")
	if err := os.MkdirAll(physicalRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	linkedRoot := filepath.Join(parent, "workspace-link")
	if err := os.Symlink(physicalRoot, linkedRoot); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.Sandbox.Enabled = true
	cfg.Sandbox.Projects = map[string]string{"workspace": ".", "api": "../api"}
	paths, err := cfg.ResolvePaths(linkedRoot)
	if err != nil {
		t.Fatalf("ResolvePaths() error: %v", err)
	}
	if paths.ProjectRoot != physicalRoot {
		t.Fatalf("project root = %q, want %q", paths.ProjectRoot, physicalRoot)
	}
	if paths.SandboxProjectRoots["workspace"] != physicalRoot {
		t.Fatalf("workspace = %q, want %q", paths.SandboxProjectRoots["workspace"], physicalRoot)
	}
	wantAPI := filepath.Join(filepath.Dir(physicalRoot), "api")
	if paths.SandboxProjectRoots["api"] != wantAPI {
		t.Fatalf("api = %q, want %q", paths.SandboxProjectRoots["api"], wantAPI)
	}
}

func TestParsePartLabelsDefaultsToTrue(t *testing.T) {
	t.Parallel()
	cfg, err := config.Parse([]byte(`agent:
  name: test
`))
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	if !cfg.Slack.PartLabels {
		t.Fatal("part_labels should default to true")
	}
}

func TestParsePartLabelsExplicitFalse(t *testing.T) {
	t.Parallel()
	cfg, err := config.Parse([]byte(`slack:
  part_labels: false
`))
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	if cfg.Slack.PartLabels {
		t.Fatal("part_labels should be false when explicitly set")
	}
}

func TestParsePartLabelsExplicitTrue(t *testing.T) {
	t.Parallel()
	cfg, err := config.Parse([]byte(`slack:
  part_labels: true
`))
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	if !cfg.Slack.PartLabels {
		t.Fatal("part_labels should be true when explicitly set")
	}
}

func TestParseThreadedDMExplicitTrue(t *testing.T) {
	t.Parallel()
	cfg, err := config.Parse([]byte(`slack:
  standard_agent:
    threaded_dm: true
`))
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	if !cfg.Slack.StandardAgent.ThreadedDM {
		t.Fatal("threaded_dm should be true when explicitly set")
	}
}

func TestValidateModelBudgetBoundaries(t *testing.T) {
	t.Parallel()

	for _, pct := range []int{0, 19, 81, 100} {
		cfg := config.Default()
		cfg.Context.ModelBudget.MaxRequestPercent = pct
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "between 20 and 80") {
			t.Fatalf("Validate(ModelBudget=%d) = %v, want error", pct, err)
		}
	}

	for _, pct := range []int{20, 40, 60, 80} {
		cfg := config.Default()
		cfg.Context.ModelBudget.MaxRequestPercent = pct
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate(ModelBudget=%d) should be valid: %v", pct, err)
		}
	}
}

func TestValidateRecoverableResultsBounds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		mutate      func(cfg *config.Config)
		wantErrText string
	}{
		{
			name:        "max_result_bytes zero",
			mutate:      func(cfg *config.Config) { cfg.Context.RecoverableResults.MaxResultBytes = 0 },
			wantErrText: "max_result_bytes",
		},
		{
			name:        "chunk_max_bytes zero",
			mutate:      func(cfg *config.Config) { cfg.Context.RecoverableResults.ChunkMaxBytes = 0 },
			wantErrText: "chunk_max_bytes",
		},
		{
			name: "chunk exceeds max result",
			mutate: func(cfg *config.Config) {
				cfg.Context.RecoverableResults.ChunkMaxBytes = int(cfg.Context.RecoverableResults.MaxResultBytes) + 1
			},
			wantErrText: "must not exceed max_result_bytes",
		},
		{
			name:        "retention_days zero",
			mutate:      func(cfg *config.Config) { cfg.Context.RecoverableResults.RetentionDays = 0 },
			wantErrText: "retention_days",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := config.Default()
			tt.mutate(&cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.wantErrText) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tt.wantErrText)
			}
		})
	}
}

func TestValidateContextContractMustBePresent(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	cfg.Context.ModelBudget = nil
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "context.model_budget") || !strings.Contains(err.Error(), "must be configured") {
		t.Fatalf("nil ModelBudget validation = %v", err)
	}

	cfg = config.Default()
	cfg.Context.RecoverableResults = nil
	err = cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "context.recoverable_results") || !strings.Contains(err.Error(), "must be configured") {
		t.Fatalf("nil RecoverableResults validation = %v", err)
	}
}

func TestParseWithExplicitZeroBudgetPercentFailsValidation(t *testing.T) {
	t.Parallel()

	_, err := config.Parse([]byte("context:\n  model_budget:\n    max_request_percent: 0\n"))
	if err == nil || !strings.Contains(err.Error(), "between 20 and 80") {
		t.Fatalf("explicit zero should fail Parse validation: %v", err)
	}
}

func TestRecoverableResultsDefaultChunksNotExceed(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	if cfg.Context.RecoverableResults.ChunkMaxBytes > int(cfg.Context.RecoverableResults.MaxResultBytes) {
		t.Fatal("default chunk_max_bytes must not exceed max_result_bytes")
	}
}

func TestCodeIntelligenceRequiresSandboxAndRecoverableResults(t *testing.T) {
	cfg := config.Default()
	cfg.CodeIntelligence.Enabled = true
	cfg.Sandbox.Enabled = false
	cfg.Sandbox.Projects = map[string]string{}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "requires sandbox.enabled") {
		t.Fatalf("Validate() = %v, want sandbox dependency error", err)
	}
	cfg.Sandbox.Enabled = true
	cfg.Sandbox.Projects = map[string]string{"workspace": "."}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "recoverable_results_enabled") {
		t.Fatalf("Validate() = %v, want recoverable result dependency error", err)
	}
	cfg.Context.ContextFeatures.RecoverableResultsEnabled = true
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() with dependencies = %v", err)
	}
}

func TestResultAnalysisDefaultsDisabledAndValid(t *testing.T) {
	t.Parallel()
	def := config.Default().Orchestration.ResultAnalysis
	if def.Enabled || def.Model.Enabled {
		t.Fatalf("result analysis defaults must be disabled: %+v", def)
	}
	if err := config.Default().Validate(); err != nil {
		t.Fatalf("default config with result analysis fields must validate: %v", err)
	}
}

// TestResultAnalysisConfigParseSchemaAndRoundTrip proves every field of
// orchestration.result_analysis set in config.yaml actually reaches the
// parsed struct, field by field, and survives a Marshal/Parse round trip.
// This is the regression test the schema-registration trap requires: a
// field present in the Go struct and in Validate but missing from
// configSchema is silently ignored by Parse instead of erroring.
func TestResultAnalysisConfigParseSchemaAndRoundTrip(t *testing.T) {
	t.Parallel()
	parsed, err := config.Parse([]byte(`orchestration:
  result_analysis:
    enabled: true
    max_segment_bytes: 32768
    overlap_basis_points: 1500
    overlap_max_bytes: 2048
    max_leaves: 128
    max_reduction_fan_in: 4
    max_reduction_depth: 4
    max_concurrent_leaves: 4
    max_attempts_per_step: 3
    call_timeout_seconds: 180
    wall_time_seconds: 1200
    worker_interval_seconds: 15
    evidence:
      excerpt_bytes: 1024
      selectors_per_leaf: 6
      references_per_packet: 20
      bundle_bytes: 16384
    model:
      enabled: true
      provider_id: internal-analysis
      base_url: https://analysis.internal.example
      api_key_env: ANALYSIS_API_KEY
      model: analysis-model-v1
      reasoning_effort: high
`))
	if err != nil {
		t.Fatal(err)
	}
	got := parsed.Orchestration.ResultAnalysis
	if !got.Enabled ||
		got.MaxSegmentBytes != 32768 ||
		got.OverlapBasisPoints != 1500 ||
		got.OverlapMaxBytes != 2048 ||
		got.MaxLeaves != 128 ||
		got.MaxReductionFanIn != 4 ||
		got.MaxReductionDepth != 4 ||
		got.MaxConcurrentLeaves != 4 ||
		got.MaxAttemptsPerStep != 3 ||
		got.CallTimeoutSeconds != 180 ||
		got.WallTimeSeconds != 1200 ||
		got.WorkerIntervalSeconds != 15 {
		t.Fatalf("parsed result analysis top-level fields = %+v", got)
	}
	if got.Evidence.ExcerptBytes != 1024 ||
		got.Evidence.SelectorsPerLeaf != 6 ||
		got.Evidence.ReferencesPerPacket != 20 ||
		got.Evidence.BundleBytes != 16384 {
		t.Fatalf("parsed result analysis evidence fields = %+v", got.Evidence)
	}
	if !got.Model.Enabled ||
		got.Model.ProviderID != "internal-analysis" ||
		got.Model.BaseURL != "https://analysis.internal.example" ||
		got.Model.APIKeyEnv != "ANALYSIS_API_KEY" ||
		got.Model.Model != "analysis-model-v1" ||
		got.Model.ReasoningEffort != "high" {
		t.Fatalf("parsed result analysis model fields = %+v", got.Model)
	}
	if err := parsed.Validate(); err != nil {
		t.Fatalf("valid result analysis settings must validate: %v", err)
	}
	rendered, err := config.Marshal(parsed)
	if err != nil {
		t.Fatal(err)
	}
	roundTripped, err := config.Parse(rendered)
	if err != nil {
		t.Fatal(err)
	}
	if roundTripped.Orchestration.ResultAnalysis != got {
		t.Fatalf("result analysis round trip = %+v, want %+v", roundTripped.Orchestration.ResultAnalysis, got)
	}
}

func TestResultAnalysisConfigPreservesExtensions(t *testing.T) {
	t.Parallel()
	withExtra, err := config.Parse([]byte(`orchestration:
  result_analysis:
    enabled: true
    future_analysis_extra: preserved
    model:
      future_model_extra: preserved
`))
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := config.Marshal(withExtra)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rendered), "future_analysis_extra: preserved") || !strings.Contains(string(rendered), "future_model_extra: preserved") {
		t.Fatalf("result analysis extension fields were not preserved: %s", rendered)
	}
}

func TestResultAnalysisConfigValidationRejectsInvalidValues(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*config.Config){
		"max segment bytes zero": func(c *config.Config) {
			c.Orchestration.ResultAnalysis.MaxSegmentBytes = 0
		},
		"max segment bytes over hard max": func(c *config.Config) {
			c.Orchestration.ResultAnalysis.MaxSegmentBytes = domain.HardMaxAnalysisSegmentBytes + 1
		},
		"overlap basis points over hard max": func(c *config.Config) {
			c.Orchestration.ResultAnalysis.OverlapBasisPoints = domain.HardMaxAnalysisOverlapBasisPoints + 1
		},
		"overlap max bytes zero": func(c *config.Config) {
			c.Orchestration.ResultAnalysis.OverlapMaxBytes = 0
		},
		"max leaves over hard max": func(c *config.Config) {
			c.Orchestration.ResultAnalysis.MaxLeaves = domain.HardMaxAnalysisLeaves + 1
		},
		"max reduction fan-in zero": func(c *config.Config) {
			c.Orchestration.ResultAnalysis.MaxReductionFanIn = 0
		},
		"max reduction depth over hard max": func(c *config.Config) {
			c.Orchestration.ResultAnalysis.MaxReductionDepth = domain.HardMaxAnalysisReductionDepth + 1
		},
		"max concurrent leaves zero": func(c *config.Config) {
			c.Orchestration.ResultAnalysis.MaxConcurrentLeaves = 0
		},
		"max attempts per step over hard max": func(c *config.Config) {
			c.Orchestration.ResultAnalysis.MaxAttemptsPerStep = domain.HardMaxAnalysisAttemptsPerStep + 1
		},
		"call timeout zero": func(c *config.Config) {
			c.Orchestration.ResultAnalysis.CallTimeoutSeconds = 0
		},
		"wall time over hard max": func(c *config.Config) {
			c.Orchestration.ResultAnalysis.WallTimeSeconds = domain.HardMaxAnalysisWallTimeSeconds + 1
		},
		"worker interval seconds zero": func(c *config.Config) {
			c.Orchestration.ResultAnalysis.WorkerIntervalSeconds = 0
		},
		"evidence excerpt bytes zero": func(c *config.Config) {
			c.Orchestration.ResultAnalysis.Evidence.ExcerptBytes = 0
		},
		"evidence selectors per leaf over hard max": func(c *config.Config) {
			c.Orchestration.ResultAnalysis.Evidence.SelectorsPerLeaf = domain.HardMaxAnalysisEvidencePerLeaf + 1
		},
		"evidence references per packet over hard max": func(c *config.Config) {
			c.Orchestration.ResultAnalysis.Evidence.ReferencesPerPacket = domain.HardMaxAnalysisEvidencePerPacket + 1
		},
		"evidence bundle bytes zero": func(c *config.Config) {
			c.Orchestration.ResultAnalysis.Evidence.BundleBytes = 0
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := config.Default()
			mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatalf("expected validation error for %s", name)
			}
		})
	}
}

func TestResultAnalysisModelEnabledRequiresFullConfiguration(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.Orchestration.ResultAnalysis.Model.Enabled = true
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for an enabled analysis model profile with no provider_id, model, or api_key_env")
	}
	cfg.Orchestration.ResultAnalysis.Model.ProviderID = "internal-analysis"
	cfg.Orchestration.ResultAnalysis.Model.Model = "analysis-model-v1"
	cfg.Orchestration.ResultAnalysis.Model.APIKeyEnv = "ANALYSIS_API_KEY"
	cfg.Orchestration.ResultAnalysis.Model.BaseURL = "https://analysis.internal.example"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected a fully configured enabled analysis model profile to validate, got %v", err)
	}
}
