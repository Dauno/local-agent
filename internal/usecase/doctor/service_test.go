package doctor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
	bot, app, context, model int
	modelAPIKey              string
}

func (f *fakeLive) CheckSlackBot(context.Context, string) error         { f.bot++; return nil }
func (f *fakeLive) CheckSlackApp(context.Context, string, string) error { f.app++; return nil }
func (f *fakeLive) CheckSlackContext(context.Context, string) error     { f.context++; return nil }
func (f *fakeLive) CheckModel(context.Context, config.ModelConfig, string) error {
	f.model++
	return nil
}

func (f *fakeLive) CheckResolvedModel(_ context.Context, _ *agentdef.ResolvedModel, apiKey string) error {
	f.model++
	f.modelAPIKey = apiKey
	return nil
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
		Database: database,
		Live:     live,
	}, database, live
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

func TestLiveDoctorCallsEveryLiveCheck(t *testing.T) {
	deps, _, live := validDependencies()
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
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "agents", "root_agent.yaml"), []byte(`
agent_class: LlmAgent
name: root_agent
model: test/default
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
