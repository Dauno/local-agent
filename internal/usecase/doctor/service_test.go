package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/agentdef"
	"github.com/Dauno/slack-local-agent/internal/config"
)

type fakeSecrets struct {
	values map[string]string
	err    error
}

func (f fakeSecrets) Resolve(keys ...string) (map[string]string, error) {
	result := make(map[string]string, len(keys))
	for _, key := range keys {
		if value, ok := f.values[key]; ok {
			result[key] = value
		}
	}
	return result, f.err
}

type fakeDatabase struct{ calls int }

func (d *fakeDatabase) CheckDatabase(context.Context, string) error { d.calls++; return nil }

type failingDatabase struct{ err error }

func (d failingDatabase) CheckDatabase(context.Context, string) error { return d.err }

type fakeLive struct {
	bot, app, context, canvas, exports, model, attachment, audio, embedding int
	modelAPIKey                                                             string
	audioAPIKey                                                             string
	embeddingAPIKey                                                         string
	embeddingErr                                                            error
}

type fakeCLI struct {
	models        []string
	shimName      string
	providerErr   error
	describeCalls int
	authCalls     int
	authShims     []string
}

func (f *fakeCLI) CheckProvider(_ context.Context, resolved *agentdef.ResolvedModel, _ config.Config, _ string, describe bool) (CLIProviderCheck, error) {
	f.models = append(f.models, resolved.Model)
	if f.providerErr != nil {
		return CLIProviderCheck{}, f.providerErr
	}
	if describe {
		f.describeCalls++
		name := f.shimName
		if name == "" {
			name = "opencode"
		}
		return CLIProviderCheck{Detail: "profile validated", ShimName: name}, nil
	}
	return CLIProviderCheck{Detail: "profile validated"}, nil
}

func (f *fakeCLI) CheckAuthentication(_ context.Context, _ *agentdef.ResolvedModel, shimName string) (string, error) {
	f.authCalls++
	f.authShims = append(f.authShims, shimName)
	return "authenticated", nil
}

func (f *fakeLive) CheckSlackBot(context.Context, string) error         { f.bot++; return nil }
func (f *fakeLive) CheckSlackApp(context.Context, string, string) error { f.app++; return nil }
func (f *fakeLive) CheckSlackContext(context.Context, string) error     { f.context++; return nil }
func (f *fakeLive) CheckSlackCanvas(context.Context, string) error      { f.canvas++; return nil }
func (f *fakeLive) CheckSlackExports(context.Context, string) error     { f.exports++; return nil }
func (f *fakeLive) CheckResolvedModel(_ context.Context, _ *agentdef.ResolvedModel, apiKey string) error {
	f.model++
	f.modelAPIKey = apiKey
	return nil
}

func (f *fakeLive) CheckAttachmentAnalyzer(_ context.Context, _ *agentdef.ResolvedModel, apiKey string) error {
	f.attachment++
	f.modelAPIKey = apiKey
	return nil
}

func (f *fakeLive) CheckAudioTranscription(_ context.Context, _ *agentdef.ResolvedModel, apiKey string) error {
	f.audio++
	f.audioAPIKey = apiKey
	return nil
}

func (f *fakeLive) CheckKnowledgeEmbedding(_ context.Context, _ config.KnowledgeEmbeddingConfig, apiKey string) error {
	f.embedding++
	f.embeddingAPIKey = apiKey
	return f.embeddingErr
}

// fakeCounter implements CounterChecker with a configurable availability set.
type fakeCounter struct {
	implemented map[string]string
}

func (f *fakeCounter) CheckCounter(strategy, id string) error {
	if f == nil || f.implemented == nil {
		return nil
	}
	if f.implemented[strategy] == id {
		return nil
	}
	return fmt.Errorf("unsupported token counter id: estimator id %q", id)
}

func validDependencies() (Dependencies, *fakeDatabase, *fakeLive) {
	database := &fakeDatabase{}
	live := &fakeLive{}
	return Dependencies{
		ConfigPath: "/tmp/project/.local-agent/config.yaml",
		LoadConfig: func(string) (config.Config, error) { return config.Default(), nil },
		Secrets: fakeSecrets{values: map[string]string{
			"DEEPSEEK_API_KEY": "secret-model-key",
			SlackBotTokenKey:   "xoxb-secret-token",
			SlackAppTokenKey:   "xapp-secret-token",
		}},
		Database:      database,
		Live:          live,
		SQLiteRuntime: &fakeSQLiteRuntimeChecker{health: healthySQLiteRuntime()},
	}, database, live
}

func TestDoctorLiveEmbeddingCheckRunsOnlyWhenEnabledAndKeyResolves(t *testing.T) {
	deps, _, live := validDependencies()
	live.embeddingErr = errors.New("embedding endpoint exploded")
	deps.LoadConfig = func(string) (config.Config, error) {
		cfg := config.Default()
		cfg.Orchestration.Knowledge.Enabled = true
		cfg.Orchestration.Knowledge.Retrieval.Enabled = true
		cfg.Orchestration.Knowledge.Retrieval.Embedding.Enabled = true
		cfg.Orchestration.Knowledge.Retrieval.Embedding.ProviderID = "acme"
		cfg.Orchestration.Knowledge.Retrieval.Embedding.BaseURL = "https://embeddings.example.test"
		cfg.Orchestration.Knowledge.Retrieval.Embedding.APIKeyEnv = "EMBEDDING_API_KEY"
		cfg.Orchestration.Knowledge.Retrieval.Embedding.Model = "embed-3"
		cfg.Orchestration.Knowledge.Retrieval.Embedding.Dimensions = 4
		cfg.Orchestration.Knowledge.Retrieval.Embedding.MinSimilarityBasisPoints = 5000
		cfg.Orchestration.Knowledge.Retrieval.Embedding.TimeoutSeconds = 5
		return cfg, nil
	}
	service, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	// The embedding key is resolved before the live check: a missing key
	// fails the secret check and the live call never runs.
	report := service.Run(t.Context(), true)
	if live.embedding != 0 {
		t.Fatalf("embedding live check ran without a resolved key: %#v", live)
	}
	secretMissing := false
	for _, result := range report.Results {
		if result.Name == "knowledge embedding API key" && result.Status == StatusFail {
			secretMissing = true
		}
	}
	if !secretMissing {
		t.Fatalf("missing embedding API key was not reported: %#v", report.Results)
	}

	deps.Secrets = fakeSecrets{values: map[string]string{
		"DEEPSEEK_API_KEY":  "secret-model-key",
		"EMBEDDING_API_KEY": "secret-embedding-key",
		SlackBotTokenKey:    "xoxb-secret-token",
		SlackAppTokenKey:    "xapp-secret-token",
	}}
	service, err = New(deps)
	if err != nil {
		t.Fatal(err)
	}
	report = service.Run(t.Context(), true)
	if live.embedding != 1 || live.embeddingAPIKey != "secret-embedding-key" {
		t.Fatalf("embedding live check calls = %d key %q, want one call with the resolved key", live.embedding, live.embeddingAPIKey)
	}
	failed := false
	for _, result := range report.Results {
		if result.Name == "knowledge embedding endpoint" {
			if result.Status != StatusFail || !strings.Contains(result.Detail, "embedding endpoint exploded") {
				t.Fatalf("embedding endpoint result = %#v, want a failure with the checked error", result)
			}
			failed = true
		}
	}
	if !failed {
		t.Fatalf("embedding endpoint failure missing: %#v", report.Results)
	}

	// With embeddings disabled the key is neither resolved nor checked.
	disabled, _, _ := validDependencies()
	disabledSecrets := fakeSecrets{values: map[string]string{
		"DEEPSEEK_API_KEY": "secret-model-key",
		SlackBotTokenKey:   "xoxb-secret-token",
		SlackAppTokenKey:   "xapp-secret-token",
	}}
	disabled.Secrets = disabledSecrets
	disabled.Live = &fakeLive{}
	service, err = New(disabled)
	if err != nil {
		t.Fatal(err)
	}
	report = service.Run(t.Context(), true)
	for _, result := range report.Results {
		if strings.Contains(result.Name, "embedding") {
			t.Fatalf("embedding-disabled doctor reported an embedding check: %#v", report.Results)
		}
	}
}

func TestOfflineDoctorCannotCallLiveChecks(t *testing.T) {
	deps, database, live := validDependencies()
	service, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	report := service.Run(t.Context(), false)
	if report.ExitCode() != 0 || database.calls != 1 {
		t.Fatalf("report=%#v database calls=%d", report, database.calls)
	}
	if live.bot != 0 || live.app != 0 || live.model != 0 {
		t.Fatalf("offline doctor made live calls: %#v", live)
	}
}

func TestDoctorValidatesADKCompactionModesAndLimits(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*config.Config)
		pass   bool
	}{
		{name: "enabled defaults", mutate: func(*config.Config) {}, pass: true},
		{name: "disabled durable root", mutate: func(cfg *config.Config) { cfg.Context.ADKCompaction.Enabled = false }, pass: false},
		{name: "SQLite limit", mutate: func(cfg *config.Config) { cfg.Context.ADKCompaction.SummaryMaxChars = 9000 }, pass: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			deps, _, _ := validDependencies()
			root := t.TempDir()
			stateDir := filepath.Join(root, ".local-agent")
			writeDoctorOpenAIDefinitions(t, stateDir)
			deps.ConfigPath = filepath.Join(stateDir, "config.yaml")
			deps.LoadConfig = func(string) (config.Config, error) {
				cfg := config.Default()
				test.mutate(&cfg)
				return cfg, nil
			}
			service, err := New(deps)
			if err != nil {
				t.Fatal(err)
			}
			report := service.Run(t.Context(), false)
			foundFailure := false
			for _, result := range report.Results {
				if result.Name == "ADK compaction" && result.Status == StatusFail {
					foundFailure = true
				}
			}
			if test.pass && foundFailure || !test.pass && !foundFailure {
				t.Fatalf("ADK compaction result mismatch: %#v", report.Results)
			}
		})
	}
}

func TestLiveDoctorCallsEveryLiveCheck(t *testing.T) {
	deps, _, live := validDependencies()
	root := t.TempDir()
	stateDir := filepath.Join(root, ".local-agent")
	writeDoctorOpenAIDefinitions(t, stateDir)
	deps.ConfigPath = filepath.Join(stateDir, "config.yaml")
	service, _ := New(deps)
	report := service.Run(t.Context(), true)
	if report.ExitCode() != 0 || live.bot != 1 || live.app != 1 || live.context != 0 || live.model != 1 {
		t.Fatalf("report=%#v live=%#v", report, live)
	}
}

func TestLiveDoctorChecksContextCapabilityWhenEnabled(t *testing.T) {
	deps, _, live := validDependencies()
	deps.LoadConfig = func(string) (config.Config, error) {
		cfg := config.Default()
		cfg.Slack.Context.Enabled = true
		return cfg, nil
	}
	service, _ := New(deps)
	if report := service.Run(t.Context(), true); report.ExitCode() != 0 || live.context != 1 {
		t.Fatalf("report=%#v live=%#v", report, live)
	}
}

func TestLiveDoctorChecksCanvasCapabilityWhenEnabled(t *testing.T) {
	deps, _, live := validDependencies()
	deps.LoadConfig = func(string) (config.Config, error) {
		cfg := config.Default()
		cfg.Canvases.Enabled = true
		return cfg, nil
	}
	service, _ := New(deps)
	report := service.Run(t.Context(), true)
	if report.ExitCode() != 0 || live.canvas != 1 {
		t.Fatalf("report=%#v live=%#v", report, live)
	}
}

func TestLiveDoctorChecksGeneratedFileCapabilityWhenEnabled(t *testing.T) {
	deps, _, live := validDependencies()
	deps.LoadConfig = func(string) (config.Config, error) {
		cfg := config.Default()
		cfg.Exports.Enabled = true
		return cfg, nil
	}
	service, _ := New(deps)
	report := service.Run(t.Context(), true)
	if report.ExitCode() != 0 || live.exports != 1 {
		t.Fatalf("report=%#v live=%#v", report, live)
	}
}

func TestDoctorValidatesAndChecksDedicatedAudioTranscriptionProfile(t *testing.T) {
	deps, _, live := validDependencies()
	root := t.TempDir()
	stateDir := filepath.Join(root, ".local-agent")
	if err := os.MkdirAll(filepath.Join(stateDir, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(stateDir, "providers"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "providers", "provider.yaml"), []byte(`
name: test
type: openai_compatible
base_url: https://example.test
api_key_env: DEEPSEEK_API_KEY
profiles:
  default:
    model: test-model
    context_window_tokens: 128000
    max_output_tokens: 2048
    token_counter:
      strategy: byte_bound
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "agents", "root_agent.yaml"), []byte(`
agent_class: LlmAgent
name: root_agent
model: test/default
global_instruction: policy here
instruction: test
`), 0o644); err != nil {
		t.Fatal(err)
	}
	deps.ConfigPath = filepath.Join(stateDir, "config.yaml")
	deps.LoadConfig = func(string) (config.Config, error) {
		cfg := config.Default()
		cfg.Slack.Files.TranscriptionProfile = "test/default"
		return cfg, nil
	}
	service, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	report := service.Run(t.Context(), true)
	if report.ExitCode() != 0 {
		t.Fatalf("doctor failed: %#v", report.Results)
	}
	if live.audio != 1 || live.audioAPIKey != "secret-model-key" {
		t.Fatalf("audio live checks=%d api key=%q", live.audio, live.audioAPIKey)
	}
	foundProfile, foundEndpoint := false, false
	for _, result := range report.Results {
		if result.Name == "audio transcription profile" && result.Status == StatusPass {
			foundProfile = true
		}
		if result.Name == "audio transcription endpoint" && result.Status == StatusPass {
			foundEndpoint = true
		}
		if result.Name == "model endpoint (audio)" {
			t.Fatalf("audio profile used generic Chat check: %#v", result)
		}
	}
	if !foundProfile || !foundEndpoint {
		t.Fatalf("dedicated audio doctor results missing: %#v", report.Results)
	}
}

func TestLiveDoctorUsesDeclarativeModelCredential(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, ".local-agent")
	if err := os.MkdirAll(filepath.Join(stateDir, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(stateDir, "providers"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "providers", "provider.yaml"), []byte(`
name: test
type: openai_compatible
base_url: https://example.test
api_key_env: DECLARATIVE_MODEL_KEY
profiles:
  default:
    model: test-model
    context_window_tokens: 128000
    max_output_tokens: 2048
    token_counter:
      strategy: byte_bound
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "agents", "root_agent.yaml"), []byte(`
agent_class: LlmAgent
name: root_agent
model: test/default
global_instruction: policy here
instruction: test
`), 0o644); err != nil {
		t.Fatal(err)
	}

	deps, _, live := validDependencies()
	deps.ConfigPath = filepath.Join(stateDir, "config.yaml")
	deps.Secrets = fakeSecrets{values: map[string]string{
		"DECLARATIVE_MODEL_KEY": "declarative-secret",
		SlackBotTokenKey:        "xoxb-secret-token",
		SlackAppTokenKey:        "xapp-secret-token",
	}}
	service, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	if report := service.Run(t.Context(), true); report.ExitCode() != 0 {
		t.Fatalf("report=%#v", report)
	}
	if live.model != 1 || live.modelAPIKey != "declarative-secret" {
		t.Fatalf("live model checks=%d api key=%q", live.model, live.modelAPIKey)
	}
}

func TestDoctorRejectsMissingRootContextCapability(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, ".local-agent")
	for _, directory := range []string{"agents", "providers"} {
		if err := os.MkdirAll(filepath.Join(stateDir, directory), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(stateDir, "providers", "provider.yaml"), []byte(`
name: test
type: openai_compatible
base_url: https://example.test
api_key_env: DECLARATIVE_MODEL_KEY
profiles:
  default:
    model: test-model
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "agents", "root_agent.yaml"), []byte(`
agent_class: LlmAgent
name: root_agent
model: test/default
global_instruction: policy
instruction: test
`), 0o644); err != nil {
		t.Fatal(err)
	}
	deps, _, _ := validDependencies()
	deps.ConfigPath = filepath.Join(stateDir, "config.yaml")
	deps.Secrets = fakeSecrets{values: map[string]string{"DECLARATIVE_MODEL_KEY": "secret", SlackBotTokenKey: "xoxb-secret", SlackAppTokenKey: "xapp-secret"}}
	service, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	report := service.Run(t.Context(), false)
	for _, result := range report.Results {
		if result.Name == "model context capability" && result.Status == StatusFail && strings.Contains(result.Detail, "context_window_tokens") {
			return
		}
	}
	t.Fatalf("missing context capability failure: %#v", report.Results)
}

func TestSecretPrefixFailuresAreActionableAndRedacted(t *testing.T) {
	deps, _, _ := validDependencies()
	deps.Secrets = fakeSecrets{values: map[string]string{
		"DEEPSEEK_API_KEY": "model-secret",
		SlackBotTokenKey:   "wrong-bot-secret",
		SlackAppTokenKey:   "wrong-app-secret",
	}}
	service, _ := New(deps)
	report := service.Run(t.Context(), false)
	if report.ExitCode() != 1 {
		t.Fatalf("exit code=%d results=%#v", report.ExitCode(), report.Results)
	}
	for _, result := range report.Results {
		if result.Detail == "wrong-bot-secret" || result.Detail == "wrong-app-secret" {
			t.Fatalf("secret leaked: %#v", result)
		}
	}
}

func TestConfigExitCodes(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code int
	}{
		{"missing", os.ErrNotExist, 1},
		{"typed invalid", &config.ValidationError{Fields: []config.FieldError{{Field: "model.name", Problem: "must not be empty"}}}, 1},
		{"malformed YAML", errors.New("decode configuration YAML: bad syntax"), 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps, _, _ := validDependencies()
			deps.LoadConfig = func(string) (config.Config, error) { return config.Config{}, tt.err }
			service, _ := New(deps)
			if got := service.Run(t.Context(), false).ExitCode(); got != tt.code {
				t.Fatalf("exit code=%d, want %d", got, tt.code)
			}
		})
	}
}

func TestDatabaseUsesTypedRemediation(t *testing.T) {
	deps, _, _ := validDependencies()
	deps.Database = failingDatabase{err: &ActionableError{Err: errors.New("future schema"), Fix: "upgrade local-agent"}}
	service, _ := New(deps)
	report := service.Run(t.Context(), false)
	for _, result := range report.Results {
		if result.Name == "SQLite" {
			if result.Remediation != "upgrade local-agent" {
				t.Fatalf("remediation=%q", result.Remediation)
			}
			return
		}
	}
	t.Fatal("SQLite result missing")
}

func TestDoctorValidatesEverySelectedCLIProfileAndDescribesProviderOnce(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, ".local-agent")
	writeDoctorCLIDefinitions(t, stateDir, false)

	cli := &fakeCLI{}
	deps, _, _ := validDependencies()
	deps.ConfigPath = filepath.Join(stateDir, "config.yaml")
	deps.LoadConfig = func(string) (config.Config, error) {
		cfg := config.Default()
		cfg.Sandbox.Enabled = true
		cfg.Sandbox.Projects = map[string]string{"workspace": "."}
		return cfg, nil
	}
	deps.CLI = cli
	service, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	report := service.Run(t.Context(), false)
	if report.ExitCode() != 0 {
		t.Fatalf("doctor failed: %#v", report.Results)
	}
	if len(cli.models) != 2 || cli.models[0] != "anthropic/root" || cli.models[1] != "anthropic/worker" {
		t.Fatalf("selected CLI profiles not all validated: %v", cli.models)
	}
	if cli.describeCalls != 1 {
		t.Fatalf("shared CLI provider described %d times, want 1", cli.describeCalls)
	}
}

func TestLiveDoctorAuthenticatesWithRetainedShimIdentity(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, ".local-agent")
	writeDoctorCLIDefinitions(t, stateDir, false)

	cli := &fakeCLI{shimName: "codex"}
	deps, _, _ := validDependencies()
	deps.ConfigPath = filepath.Join(stateDir, "config.yaml")
	deps.LoadConfig = func(string) (config.Config, error) {
		cfg := config.Default()
		cfg.Sandbox.Enabled = true
		cfg.Sandbox.Projects = map[string]string{"workspace": "."}
		return cfg, nil
	}
	deps.CLI = cli
	service, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	report := service.Run(t.Context(), true)
	if report.ExitCode() != 0 {
		t.Fatalf("doctor failed: %#v", report.Results)
	}
	if cli.authCalls != 1 {
		t.Fatalf("shared CLI provider authenticated %d times, want 1", cli.authCalls)
	}
	if len(cli.authShims) != 1 || cli.authShims[0] != "codex" {
		t.Fatalf("authentication did not receive retained mapper identity: %v", cli.authShims)
	}
}

func TestLiveDoctorChecksOpenAIAttachmentWithAgentCLIRoot(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, ".local-agent")
	writeDoctorCLIDefinitions(t, stateDir, false)
	if err := os.WriteFile(filepath.Join(stateDir, "providers", "vision.yaml"), []byte(`
name: vision
type: openai_compatible
base_url: https://example.com/v1
api_key_env: VISION_API_KEY
profiles:
  analyzer:
    model: vision/model
    context_window_tokens: 128000
    token_counter:
      strategy: estimator
      id: visual-tile-conservative-v1
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "agents", "attachment_analyzer.yaml"), []byte(`
agent_class: LlmAgent
name: attachment_analyzer
model: vision/analyzer
instruction: inspect image
`), 0o644); err != nil {
		t.Fatal(err)
	}
	deps, _, live := validDependencies()
	deps.ConfigPath = filepath.Join(stateDir, "config.yaml")
	deps.LoadConfig = func(string) (config.Config, error) {
		cfg := config.Default()
		cfg.Sandbox.Enabled = true
		cfg.Sandbox.Projects = map[string]string{"workspace": "."}
		return cfg, nil
	}
	deps.Secrets = fakeSecrets{values: map[string]string{
		"VISION_API_KEY": "vision-secret", SlackBotTokenKey: "xoxb-secret-token", SlackAppTokenKey: "xapp-secret-token",
	}}
	deps.CLI = &fakeCLI{}
	deps.Counter = &fakeCounter{implemented: map[string]string{"estimator": agentdef.VisualEstimatorID}}
	service, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	report := service.Run(t.Context(), true)
	if report.ExitCode() != 0 || live.attachment != 1 || live.modelAPIKey != "vision-secret" {
		t.Fatalf("report=%#v live=%#v", report.Results, live)
	}
}

func TestDoctorRejectsUnimplementedCounterCombination(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, ".local-agent")
	if err := os.MkdirAll(filepath.Join(stateDir, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(stateDir, "providers"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "providers", "test.yaml"), []byte(`
name: test
type: openai_compatible
base_url: https://example.test
api_key_env: DECLARATIVE_MODEL_KEY
profiles:
  default:
    model: test-model
    context_window_tokens: 128000
    token_counter:
      strategy: estimator
      id: not-installed
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "agents", "root_agent.yaml"), []byte(`
agent_class: LlmAgent
name: root_agent
model: test/default
global_instruction: policy here
instruction: test
`), 0o644); err != nil {
		t.Fatal(err)
	}
	deps, _, _ := validDependencies()
	deps.ConfigPath = filepath.Join(stateDir, "config.yaml")
	deps.Counter = &fakeCounter{implemented: map[string]string{"estimator": agentdef.VisualEstimatorID}}
	service, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	report := service.Run(t.Context(), false)
	if report.ExitCode() != 1 {
		t.Fatalf("unknown estimator id must fail doctor, got %#v", report.Results)
	}
	found := false
	for _, result := range report.Results {
		if result.Name == "token counter" && result.Status == StatusFail && strings.Contains(result.Detail, "not-installed") {
			found = true
		}
	}
	if !found {
		t.Fatalf("actionable token counter failure missing: %#v", report.Results)
	}
}

func TestLiveDoctorSkipsAuthenticationWhenProviderDescribeFails(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, ".local-agent")
	writeDoctorCLIDefinitions(t, stateDir, false)

	cli := &fakeCLI{providerErr: errors.New("cli-v1 describe failed")}
	deps, _, _ := validDependencies()
	deps.ConfigPath = filepath.Join(stateDir, "config.yaml")
	deps.LoadConfig = func(string) (config.Config, error) {
		cfg := config.Default()
		cfg.Sandbox.Enabled = true
		cfg.Sandbox.Projects = map[string]string{"workspace": "."}
		return cfg, nil
	}
	deps.CLI = cli
	service, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	report := service.Run(t.Context(), true)
	if report.ExitCode() != 1 {
		t.Fatalf("provider failure should remain actionable: %#v", report.Results)
	}
	if cli.authCalls != 0 {
		t.Fatalf("authentication ran without a trusted shim identity: %d calls", cli.authCalls)
	}
	for _, result := range report.Results {
		if strings.HasPrefix(result.Name, "agent CLI authentication") {
			t.Fatalf("misleading authentication result emitted after describe failure: %#v", result)
		}
	}
}

func TestDoctorRejectsAgentCLIAttachmentAnalyzer(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, ".local-agent")
	writeDoctorCLIDefinitions(t, stateDir, true)

	deps, _, _ := validDependencies()
	deps.ConfigPath = filepath.Join(stateDir, "config.yaml")
	deps.LoadConfig = func(string) (config.Config, error) {
		cfg := config.Default()
		cfg.Sandbox.Enabled = true
		cfg.Sandbox.Projects = map[string]string{"workspace": "."}
		return cfg, nil
	}
	deps.CLI = &fakeCLI{}
	service, _ := New(deps)
	report := service.Run(t.Context(), false)
	if report.ExitCode() != 1 {
		t.Fatalf("expected attachment incompatibility, got %#v", report.Results)
	}
	for _, result := range report.Results {
		if result.Name == "agent definitions" && strings.Contains(result.Detail, "attachment_analyzer cannot use an agent_cli") {
			return
		}
	}
	t.Fatalf("actionable attachment incompatibility missing: %#v", report.Results)
}

func TestDoctorValidatesReferencedAgentTool(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, ".local-agent")
	if err := os.MkdirAll(filepath.Join(stateDir, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(stateDir, "providers"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		filepath.Join(stateDir, "providers", "deepseek.yaml"): `
name: deepseek
type: openai_compatible
base_url: https://api.deepseek.com
api_key_env: DEEPSEEK_API_KEY
profiles:
  root:
    model: deepseek-v4-flash
    context_window_tokens: 128000
    max_output_tokens: 2048
    token_counter:
      strategy: byte_bound
`,
		filepath.Join(stateDir, "providers", "opencode.yaml"): `
name: opencode
type: agent_cli
shim:
  command: self
  args: [shim, opencode]
profiles:
  build:
    model: opencode/big-pickle
`,
		filepath.Join(stateDir, "agents", "root_agent.yaml"): `
agent_class: LlmAgent
name: root_agent
model: deepseek/root
global_instruction: policy
instruction: delegate coding tasks
agent_tools: [opencode_worker]
`,
		filepath.Join(stateDir, "agents", "opencode_worker.yaml"): `
agent_class: LlmAgent
name: opencode_worker
model: opencode/build
description: Handles delegated coding tasks.
instruction: complete the delegated task
include_contents: none
`,
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cli := &fakeCLI{}
	deps, _, _ := validDependencies()
	deps.ConfigPath = filepath.Join(stateDir, "config.yaml")
	deps.LoadConfig = func(string) (config.Config, error) {
		cfg := config.Default()
		cfg.Sandbox.Enabled = true
		cfg.Sandbox.Projects = map[string]string{"workspace": "."}
		return cfg, nil
	}
	deps.CLI = cli
	service, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	report := service.Run(t.Context(), false)
	if report.ExitCode() != 0 {
		t.Fatalf("doctor failed: %#v", report.Results)
	}
	if len(cli.models) != 1 || cli.models[0] != "opencode/big-pickle" || cli.describeCalls != 1 {
		t.Fatalf("agent tool CLI validation = models %v, describes %d", cli.models, cli.describeCalls)
	}
}

func TestDoctorValidatesScopedOpenAICompatibleAgentToolCredentialsWithoutCLI(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, ".local-agent")
	if err := os.MkdirAll(filepath.Join(stateDir, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(stateDir, "providers"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		filepath.Join(stateDir, "providers", "deepseek.yaml"): `
name: deepseek
type: openai_compatible
base_url: https://api.deepseek.com
api_key_env: DEEPSEEK_API_KEY
profiles:
  root:
    model: deepseek-v4-flash
    context_window_tokens: 128000
    max_output_tokens: 2048
    token_counter:
      strategy: byte_bound
`,
		filepath.Join(stateDir, "providers", "explorer.yaml"): `
name: explorer
type: openai_compatible
base_url: https://explorer.example.test
api_key_env: EXPLORER_API_KEY
profiles:
  scout:
    model: explorer-scout
    context_window_tokens: 128000
    max_output_tokens: 2048
    token_counter:
      strategy: byte_bound
`,
		filepath.Join(stateDir, "agents", "root_agent.yaml"): `
agent_class: LlmAgent
name: root_agent
model: deepseek/root
global_instruction: policy
instruction: delegate exploration tasks
agent_tools: [explore]
`,
		filepath.Join(stateDir, "agents", "explore.yaml"): `
agent_class: LlmAgent
name: explore
model: explorer/scout
description: Explores registered projects and returns read-only evidence.
instruction: investigate the delegated request
include_contents: none
tool_scope: invocation_scoped
`,
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cli := &fakeCLI{}
	deps, _, _ := validDependencies()
	deps.ConfigPath = filepath.Join(stateDir, "config.yaml")
	deps.LoadConfig = func(string) (config.Config, error) {
		cfg := config.Default()
		cfg.Sandbox.Enabled = true
		cfg.Sandbox.Projects = map[string]string{"workspace": "."}
		return cfg, nil
	}
	deps.CLI = cli
	service, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	report := service.Run(t.Context(), false)
	if report.ExitCode() != 1 {
		t.Fatalf("doctor accepted a missing scoped child credential: %#v", report.Results)
	}
	missingChildKey := false
	for _, result := range report.Results {
		if result.Name == "model API key (explore)" && strings.Contains(result.Detail, "EXPLORER_API_KEY is not set") {
			missingChildKey = true
		}
	}
	if !missingChildKey {
		t.Fatalf("missing scoped child credential was not reported: %#v", report.Results)
	}

	deps.Secrets = fakeSecrets{values: map[string]string{
		"DEEPSEEK_API_KEY": "secret-model-key",
		"EXPLORER_API_KEY": "secret-explorer-key",
		SlackBotTokenKey:   "xoxb-secret-token",
		SlackAppTokenKey:   "xapp-secret-token",
	}}
	service, err = New(deps)
	if err != nil {
		t.Fatal(err)
	}
	report = service.Run(t.Context(), false)
	if report.ExitCode() != 0 {
		t.Fatalf("doctor failed: %#v", report.Results)
	}
	if len(cli.models) != 0 || cli.describeCalls != 0 || cli.authCalls != 0 {
		t.Fatalf("openai_compatible agent tool triggered CLI validation: %#v", cli)
	}
}

func TestDoctorRejectsUnimplementedOpenAIAuxiliaryCounterWithCLIRoot(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, ".local-agent")
	writeDoctorCLIDefinitions(t, stateDir, false)
	if err := os.WriteFile(filepath.Join(stateDir, "providers", "auxiliary.yaml"), []byte(`
name: auxiliary
type: openai_compatible
base_url: https://auxiliary.example.test
api_key_env: DEEPSEEK_API_KEY
profiles:
  default:
    model: auxiliary-model
    context_window_tokens: 128000
    token_counter:
      strategy: estimator
      id: not-installed
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "agents", "scout.yaml"), []byte(`
agent_class: LlmAgent
name: scout
model: auxiliary/default
description: inspect workspace
instruction: inspect the workspace
tool_scope: invocation_scoped
`), 0o644); err != nil {
		t.Fatal(err)
	}

	deps, _, _ := validDependencies()
	deps.ConfigPath = filepath.Join(stateDir, "config.yaml")
	deps.CLI = &fakeCLI{}
	deps.Counter = &fakeCounter{implemented: map[string]string{"estimator": agentdef.VisualEstimatorID}}
	service, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	report := service.Run(t.Context(), false)
	if report.ExitCode() != 1 {
		t.Fatalf("doctor accepted unimplemented auxiliary counter: %#v", report.Results)
	}
	for _, result := range report.Results {
		if result.Name == "token counter (scout)" && result.Status == StatusFail && strings.Contains(result.Detail, "not-installed") {
			return
		}
	}
	t.Fatalf("auxiliary token counter failure missing: %#v", report.Results)
}

func writeDoctorCLIDefinitions(t *testing.T, stateDir string, includeAttachment bool) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(stateDir, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(stateDir, "providers"), 0o755); err != nil {
		t.Fatal(err)
	}
	provider := `
name: opencode
type: agent_cli
shim:
  command: self
  args: [shim, opencode]
profiles:
  root:
    model: anthropic/root
  worker:
    model: anthropic/worker
  attachment:
    model: anthropic/attachment
`
	rootAgent := `
agent_class: LlmAgent
name: root_agent
model: opencode/root
global_instruction: policy
instruction: root
`
	worker := `
agent_class: LlmAgent
name: worker
description: Handles worker tasks.
model: opencode/worker
instruction: work
`
	files := map[string]string{
		filepath.Join(stateDir, "providers", "opencode.yaml"): provider,
		filepath.Join(stateDir, "agents", "root_agent.yaml"):  rootAgent,
		filepath.Join(stateDir, "agents", "worker.yaml"):      worker,
	}
	if includeAttachment {
		files[filepath.Join(stateDir, "agents", "attachment_analyzer.yaml")] = `
agent_class: LlmAgent
name: attachment_analyzer
model: opencode/attachment
instruction: inspect image
`
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func writeDoctorOpenAIDefinitions(t *testing.T, stateDir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(stateDir, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(stateDir, "providers"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "providers", "provider.yaml"), []byte(`
name: test
type: openai_compatible
base_url: https://example.test
api_key_env: DEEPSEEK_API_KEY
profiles:
  default:
    model: test-model
    context_window_tokens: 128000
    max_output_tokens: 2048
    token_counter:
      strategy: byte_bound
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "agents", "root_agent.yaml"), []byte(`
agent_class: LlmAgent
name: root_agent
model: test/default
global_instruction: policy here
instruction: test
`), 0o644); err != nil {
		t.Fatal(err)
	}
}
