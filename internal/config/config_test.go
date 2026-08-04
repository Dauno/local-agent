package config_test

import (
	"errors"
	"math"
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
		Agent: config.AgentConfig{Name: "Dev Agent"},
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
				SummaryEnabled: true, SummaryMaxChars: 8_000,
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
		Model: config.ModelConfig{
			Name:            "deepseek-v4-flash",
			BaseURL:         "https://api.deepseek.com",
			APIKeyEnv:       "DEEPSEEK_API_KEY",
			ReasoningEffort: "high",
			ExtraBody: map[string]any{
				"thinking": map[string]any{"type": "enabled"},
			},
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
		Memory: config.MemoryConfig{
			Enabled:               false,
			Directory:             "",
			MaxTopicsRecall:       3,
			MaxCharsRecall:        2000,
			RecallTimeoutSeconds:  2,
			CuratorTimeoutSeconds: 30,
			CuratorMaxRetries:     3,
			WorkerIntervalSeconds: 60,
			RetentionDays:         90,
			MaxTopics:             100,
			MaxLinks:              50,
			MaxTopicChars:         10000,
			MaxPatchOps:           10,
		},
		Sandbox:          config.SandboxConfig{Enabled: true, Projects: map[string]string{"workspace": "."}, CommandTimeoutSeconds: 30, MaxOutputBytes: 65536},
		Canvases:         config.CanvasesConfig{MaxTitleChars: 150, MaxContentChars: 50000, MaxContentBytes: 5 * 1024 * 1024, TimeoutSeconds: 30},
		Exports:          config.ExportsConfig{MaxFilenameChars: 128, MaxContentBytes: 1024 * 1024, TimeoutSeconds: 30},
		OpenCode:         config.OpenCodeConfig{Management: config.OpenCodeManagementConfig{AllowedUserIDs: []string{}}},
		ACP:              config.ACPConfig{MaxFrameBytes: 8 * 1024 * 1024, MaxInlineResultBytes: 64 * 1024, MaxResultArtifactBytes: 16 * 1024 * 1024, StderrTailBytes: 128 * 1024, DefaultJobTimeoutSeconds: 7200, MaxJobTimeoutSeconds: 86400, ReconciliationTimeoutSeconds: 1800, WorkerConcurrency: 1, ArtifactRetentionDays: 30, Delivery: config.ACPDeliveryConfig{MaxMarkdownParts: 6, MaxFileBytes: 16 * 1024 * 1024}},
		CodeIntelligence: &config.CodeIntelligenceConfig{Enabled: false, MaxProcesses: 4, InitTimeoutSeconds: 20, RequestTimeoutSeconds: 10},
	}

	got := config.Default()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Default() mismatch\n got: %#v\nwant: %#v", got, want)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("default config must validate: %v", err)
	}
}

func TestDefaultDoesNotShareExtraBody(t *testing.T) {
	t.Parallel()

	first := config.Default()
	second := config.Default()
	first.Model.ExtraBody["thinking"].(map[string]any)["type"] = "disabled"

	got := second.Model.ExtraBody["thinking"].(map[string]any)["type"]
	if got != "enabled" {
		t.Fatalf("defaults share mutable extra_body state: got %v", got)
	}
}

func TestADKCompactionDefaultsAndProductionValidation(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	if cfg.Context.ADKCompaction == nil || !cfg.Context.ADKCompaction.Enabled || cfg.Context.ADKCompaction.MaxHistoryChars != 120000 || cfg.Context.ADKCompaction.RecentTurns != 8 || !cfg.Context.ADKCompaction.SummaryEnabled || cfg.Context.ADKCompaction.SummaryMaxChars != 8000 {
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
}

func TestACPDeliveryPolicyValidation(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.ACP.Delivery.MaxMarkdownParts = 9
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "max_markdown_parts") {
		t.Fatalf("part policy error = %v", err)
	}
	cfg = config.Default()
	cfg.ACP.Delivery.MaxFileBytes = cfg.ACP.MaxResultArtifactBytes + 1
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "max_file_bytes") {
		t.Fatalf("file policy error = %v", err)
	}
	cfg = config.Default()
	cfg.ACP.Delivery.MaxMarkdownParts = 1
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "delivery capacity") {
		t.Fatalf("inline capacity error = %v", err)
	}
}

func TestACPDeliveryFileBoundDefaultsToConfiguredArtifactBound(t *testing.T) {
	t.Parallel()
	cfg, err := config.Parse([]byte("acp:\n  max_result_artifact_bytes: 1048576\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ACP.Delivery.MaxFileBytes != cfg.ACP.MaxResultArtifactBytes {
		t.Fatalf("max_file_bytes = %d, artifact bound = %d", cfg.ACP.Delivery.MaxFileBytes, cfg.ACP.MaxResultArtifactBytes)
	}
}

func TestMarshalDefaultYAML(t *testing.T) {
	t.Parallel()

	got, err := config.Marshal(config.Default())
	if err != nil {
		t.Fatalf("Marshal() error: %v", err)
	}
	want := `agent:
  name: Dev Agent
state:
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
model:
  name: deepseek-v4-flash
  base_url: https://api.deepseek.com
  api_key_env: DEEPSEEK_API_KEY
  reasoning_effort: high
  extra_body:
    thinking:
      type: enabled
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
memory:
  enabled: false
  directory: ""
  max_topics_recall: 3
  max_chars_recall: 2000
  recall_timeout_seconds: 2
  curator_timeout_seconds: 30
  curator_max_retries: 3
  worker_interval_seconds: 60
  retention_days: 90
  max_topics: 100
  max_links: 50
  max_topic_chars: 10000
  max_patch_ops: 10
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
opencode:
  management:
    allowed_user_ids: []
acp:
  max_frame_bytes: 8388608
  max_inline_result_bytes: 65536
  max_result_artifact_bytes: 16777216
  stderr_tail_bytes: 131072
   default_job_timeout_seconds: 7200
   max_job_timeout_seconds: 86400
   idle_timeout_seconds: 0
   reconciliation_timeout_seconds: 1800
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
         `

	if !reflect.DeepEqual(strings.Fields(string(got)), strings.Fields(want)) {
		t.Fatalf("default YAML fields mismatch\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

func TestParseAppliesOnlyMissingDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := config.Parse([]byte(`agent:
  name: Release Agent
model:
  headers:
    X-Client: local-agent
  extra_body: {}
slack:
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

	if cfg.Agent.Name != "Release Agent" {
		t.Fatalf("agent.name = %q", cfg.Agent.Name)
	}
	if cfg.Model.Name != "deepseek-v4-flash" {
		t.Fatalf("missing model.name did not receive default: %q", cfg.Model.Name)
	}
	if len(cfg.Model.ExtraBody) != 0 {
		t.Fatalf("explicit empty extra_body was overwritten: %#v", cfg.Model.ExtraBody)
	}
	if cfg.Model.Headers["X-Client"] != "local-agent" {
		t.Fatalf("model headers not decoded: %#v", cfg.Model.Headers)
	}
	if cfg.Slack.AllowedUserIDs == nil || len(cfg.Slack.AllowedUserIDs) != 0 {
		t.Fatalf("allowed_user_ids should normalize to an empty slice: %#v", cfg.Slack.AllowedUserIDs)
	}
	if cfg.Slack.Files.MaxBytesPerFile != 1048576 || cfg.Slack.Files.MaxProcessedChars != 4096 || cfg.Slack.Files.TranscriptionProfile != "openai/stt" || cfg.Slack.Files.TranscriptionTimeoutSeconds != 45 {
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
		if !reflect.DeepEqual(cfg.Agent, want.Agent) ||
			!reflect.DeepEqual(cfg.State, want.State) ||
			!reflect.DeepEqual(cfg.Context, want.Context) ||
			!reflect.DeepEqual(cfg.Runtime, want.Runtime) ||
			!reflect.DeepEqual(cfg.Model, want.Model) ||
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
	if cfg.Context.ADKCompaction == nil || !cfg.Context.ADKCompaction.Enabled || cfg.Context.ADKCompaction.MaxHistoryChars != 120000 || cfg.Context.ADKCompaction.SummaryMaxChars != 8000 {
		t.Fatalf("legacy compaction defaults = %#v", cfg.Context.ADKCompaction)
	}
}

func TestParseAndMarshalPreserveUnknownFieldsAndComments(t *testing.T) {
	t.Parallel()

	input := []byte(`# operator note
agent:
  name: Old Name # keep this comment
  tone: terse
plugin_extension:
  enabled: true
model:
  headers:
    X-Trace: enabled
`)
	cfg, err := config.Parse(input)
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	cfg.Agent.Name = "New Name"
	cfg.Model.Headers = nil

	output, err := config.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal() error: %v", err)
	}
	text := string(output)
	for _, fragment := range []string{
		"# operator note",
		"name: New Name # keep this comment",
		"tone: terse",
		"plugin_extension:",
		"enabled: true",
	} {
		if !strings.Contains(text, fragment) {
			t.Errorf("output lost %q:\n%s", fragment, text)
		}
	}
	if strings.Contains(text, "headers:") || strings.Contains(text, "X-Trace") {
		t.Fatalf("cleared known headers were retained:\n%s", text)
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
		name, input := name, input
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
	cfg.Agent.Name = " "
	cfg.State.DB = ""
	cfg.Context.MaxMessages = 0
	cfg.Runtime.LogLevel = "verbose"
	cfg.Runtime.ModelTimeoutSeconds = -1
	cfg.Runtime.SlackAPITimeoutSeconds = -1
	cfg.Runtime.MaxConcurrentModelCalls = 0
	cfg.Runtime.ShutdownGraceSeconds = 0
	cfg.Model.BaseURL = "https://example.com/v1/chat/completions"
	cfg.ACP.ReconciliationTimeoutSeconds = 0
	cfg.Model.APIKeyEnv = "NOT-AN-ENV"
	cfg.Model.ReasoningEffort = "maximum"
	cfg.Model.Headers = map[string]string{"Bad Header": "line\nbreak"}
	cfg.Model.ExtraBody = map[string]any{"bad": math.NaN(), "stream": true}
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
		"agent.name",
		"state.db",
		"context.max_messages",
		"runtime.log_level",
		"runtime.model_timeout_seconds",
		"runtime.slack_api_timeout_seconds",
		"runtime.max_concurrent_model_calls",
		"runtime.shutdown_grace_seconds",
		"model.base_url",
		"acp.reconciliation_timeout_seconds",
		"model.api_key_env",
		"model.reasoning_effort",
		`model.headers["Bad Header"]`,
		"model.extra_body",
		"model.extra_body.stream",
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
	cfg.Model.Headers = map[string]string{"X-Client-Version": "1", "X_Custom": "ok"}
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

func TestValidateRejectsSensitiveModelHeaders(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.Model.Headers = map[string]string{"Authorization": "Bearer secret"}
	err := cfg.Validate()
	var validation *config.ValidationError
	if !errors.As(err, &validation) || !validation.Has(`model.headers["Authorization"]`) {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestValidateRejectsInvalidMemoryLimits(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.Memory.RecallTimeoutSeconds = 0
	cfg.Memory.MaxPatchOps = 0
	err := cfg.Validate()
	var validation *config.ValidationError
	if !errors.As(err, &validation) || !validation.Has("memory.recall_timeout_seconds") || !validation.Has("memory.max_patch_ops") {
		t.Fatalf("Validate() error = %v", err)
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
		ProjectRoot:         root,
		StateDir:            filepath.Join(root, "var", "state"),
		DatabaseFile:        filepath.Join(root, "outside", "agent.db"),
		ConfigFile:          filepath.Join(root, ".local-agent", "config.yaml"),
		ManifestFile:        filepath.Join(root, ".local-agent", "app-manifest.local.yaml"),
		EnvExampleFile:      filepath.Join(root, ".local-agent", "local.env.example"),
		EnvFile:             filepath.Join(root, ".env"),
		MemoryDir:           filepath.Join(root, "var", "state", "memory"),
		OpenCodeWorktreeDir: filepath.Join(root, "var", "state", "worktrees"),
		ArtifactDir:         filepath.Join(root, "var", "state", "artifacts"),
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
	input := []byte("agent:\n  name: Existing\nextension:\n  value: retained\n")
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
	cfg.Agent.Name = "Updated"
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
	if !strings.Contains(string(data), "name: Updated") || !strings.Contains(string(data), "value: retained") {
		t.Fatalf("saved data lost changes or extensions:\n%s", data)
	}

	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("re-Load() error: %v", err)
	}
	if reloaded.Agent.Name != "Updated" {
		t.Fatalf("reloaded agent.name = %q", reloaded.Agent.Name)
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

func TestParseAppliesOpenCodeManagementAllowlist(t *testing.T) {
	t.Parallel()
	cfg, err := config.Parse([]byte(`opencode:
  management:
    allowed_user_ids: [U12345678]
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.OpenCode.Management.AllowedUserIDs; len(got) != 1 || got[0] != "U12345678" {
		t.Fatalf("allowed user IDs = %v", got)
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
		tt := tt
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
