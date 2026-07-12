package bootstrap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/adapter/envfile"
	"github.com/Dauno/slack-local-agent/internal/adapter/fsproject"
	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/manifest"
	"github.com/joho/godotenv"
)

func TestEnsureBaseArtifactsFirstRun(t *testing.T) {
	service := newRealService(t)
	root := t.TempDir()
	snapshot, err := service.EnsureBaseArtifacts(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}

	if snapshot.ProjectRoot != root {
		t.Fatalf("snapshot root = %q, want %q", snapshot.ProjectRoot, root)
	}
	for _, path := range []string{
		snapshot.Paths.ConfigFile,
		snapshot.Paths.ManifestFile,
		snapshot.Paths.EnvExampleFile,
		snapshot.Paths.DatabaseFile,
	} {
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
			t.Fatalf("artifact %s: info=%v err=%v", path, info, err)
		}
	}
	if _, err := os.Stat(snapshot.Paths.EnvFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("EnsureBaseArtifacts created .env: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".gitignore")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("EnsureBaseArtifacts created .gitignore: %v", err)
	}

	wantConfig, err := config.Marshal(config.Default())
	if err != nil {
		t.Fatal(err)
	}
	assertFileEquals(t, snapshot.Paths.ConfigFile, string(wantConfig))
	wantManifest, err := manifest.Render(manifest.Identity{AppName: "Local Agent", BotDisplayName: "Dev Agent"})
	if err != nil {
		t.Fatal(err)
	}
	assertFileEquals(t, snapshot.Paths.ManifestFile, wantManifest)
	example, err := os.ReadFile(snapshot.Paths.EnvExampleFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{"DEEPSEEK_API_KEY=...", "SLACK_BOT_TOKEN=xoxb-...", "SLACK_APP_TOKEN=xapp-..."} {
		if !strings.Contains(string(example), fragment) {
			t.Errorf("env example missing %q:\n%s", fragment, example)
		}
	}

	store, err := adaptersqlite.OpenExisting(t.Context(), snapshot.Paths.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ProbeReadWrite(t.Context()); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureBaseArtifactsRespectsExistingConfigAndNeverOverwritesOrResets(t *testing.T) {
	service := newRealService(t)
	root := t.TempDir()
	stateDir := filepath.Join(root, config.DefaultProjectStateDir)
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Agent.Name = "Configured Agent"
	cfg.Slack.AppName = "Configured Slack App"
	cfg.Slack.BotDisplayName = "Configured Bot"
	cfg.Model.APIKeyEnv = "CUSTOM_MODEL_KEY"
	cfg.State.Dir = "custom-state"
	cfg.State.DB = "custom-state/context.db"
	configData, err := config.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, config.DefaultConfigFile)
	if err := os.WriteFile(configPath, configData, 0o640); err != nil {
		t.Fatal(err)
	}

	snapshot, err := service.EnsureBaseArtifacts(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	assertFileEquals(t, configPath, string(configData))
	if snapshot.Paths.DatabaseFile != filepath.Join(root, "custom-state", "context.db") {
		t.Fatalf("configured DB path = %q", snapshot.Paths.DatabaseFile)
	}
	wantManifest, _ := manifest.Render(manifest.Identity{AppName: cfg.Slack.AppName, BotDisplayName: cfg.Slack.BotDisplayName})
	assertFileEquals(t, snapshot.Paths.ManifestFile, wantManifest)
	example, _ := os.ReadFile(snapshot.Paths.EnvExampleFile)
	if !strings.Contains(string(example), "CUSTOM_MODEL_KEY=...") || strings.Contains(string(example), "DEEPSEEK_API_KEY") {
		t.Fatalf("env example does not use configured key:\n%s", example)
	}

	store, err := adaptersqlite.OpenExisting(t.Context(), snapshot.Paths.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	metadata := domain.ConversationMetadata{
		Key: "slack:T12345678:dm:D12345678", TeamID: "T12345678",
		ChannelID: "D12345678", ChannelKind: domain.ChannelDM, LastTS: "1700000000.000001",
	}
	if err := store.AppendMessage(t.Context(), metadata, domain.Message{
		Role: domain.RoleUser, Content: "persisted", UserID: "U12345678",
		CreatedAt: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	}, 100); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(snapshot.Paths.ManifestFile, []byte("operator manifest\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(snapshot.Paths.EnvExampleFile, []byte("operator example\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := service.EnsureBaseArtifacts(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	assertFileEquals(t, second.Paths.ConfigFile, string(configData))
	assertFileEquals(t, second.Paths.ManifestFile, "operator manifest\n")
	assertFileEquals(t, second.Paths.EnvExampleFile, "operator example\n")
	store, err = adaptersqlite.OpenExisting(t.Context(), second.Paths.DatabaseFile)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	messages, err := store.RecentMessages(t.Context(), metadata.Key, 10)
	if err != nil || len(messages) != 1 || messages[0].Content != "persisted" {
		t.Fatalf("database was reset: messages=%#v err=%v", messages, err)
	}
}

func TestApplyConfirmedUpdatesPreservesExtensionsAndUnrelatedFiles(t *testing.T) {
	service := newRealService(t)
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshot, err := service.EnsureBaseArtifacts(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}

	configSource := `# operator note
agent:
  name: Before # preserve comment
  extension: retained
plugin_extension:
  enabled: true
model:
  api_key_env: CUSTOM_MODEL_KEY
slack:
  app_name: Before App
  bot_display_name: Before Bot
`
	if err := os.WriteFile(snapshot.Paths.ConfigFile, []byte(configSource), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(snapshot.Paths.ConfigFile, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(snapshot.Paths.ManifestFile, []byte("stale manifest\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(snapshot.Paths.ManifestFile, 0o640); err != nil {
		t.Fatal(err)
	}
	envSource := "# secret note\nUNRELATED=keep\nCUSTOM_MODEL_KEY=old-model\nDEEPSEEK_API_KEY=preserve-old-provider-key\nSLACK_BOT_TOKEN=old-bot\nEXTRA_SECRET=preserve\n"
	if err := os.WriteFile(snapshot.Paths.EnvFile, []byte(envSource), 0o644); err != nil {
		t.Fatal(err)
	}
	ignorePath := filepath.Join(root, ".gitignore")
	if err := os.WriteFile(ignorePath, []byte("dist/\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	identity := Identity{
		AgentName: "Confirmed Agent", SlackAppName: "Confirmed App", SlackBotDisplayName: "Confirmed Bot",
	}
	access := AccessControl{
		AllowedUserIDs: []string{"U12345678"}, AllowedTeamIDs: []string{"T12345678"},
		AllowedChannelIDs: []string{"C12345678"}, ContextEnabled: true,
	}
	secrets := Secrets{
		ModelAPIKey: "model-secret", SlackBotToken: "xoxb-bot-secret", SlackAppToken: "xapp-app-secret",
	}
	updated, err := service.ApplyConfirmedUpdates(t.Context(), snapshot, identity, access, secrets)
	if err != nil {
		t.Fatal(err)
	}

	configOutput, err := os.ReadFile(updated.Paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{
		"# operator note", "name: Confirmed Agent # preserve comment", "extension: retained",
		"plugin_extension:", "enabled: true",
	} {
		if !strings.Contains(string(configOutput), fragment) {
			t.Errorf("updated config lost %q:\n%s", fragment, configOutput)
		}
	}
	for _, secret := range []string{secrets.ModelAPIKey, secrets.SlackBotToken, secrets.SlackAppToken} {
		if strings.Contains(string(configOutput), secret) {
			t.Fatalf("config contains secret %q", secret)
		}
	}
	loaded, err := config.Load(updated.Paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Agent.Name != identity.AgentName || loaded.Slack.AppName != identity.SlackAppName || loaded.Slack.BotDisplayName != identity.SlackBotDisplayName {
		t.Fatalf("identity not applied: %#v %#v", loaded.Agent, loaded.Slack)
	}
	if strings.Join(loaded.Slack.AllowedUserIDs, ",") != "U12345678" || strings.Join(loaded.Slack.AllowedTeamIDs, ",") != "T12345678" || strings.Join(loaded.Slack.AllowedChannelIDs, ",") != "C12345678" {
		t.Fatalf("access control not applied: %#v", loaded.Slack)
	}
	if !loaded.Slack.Context.Enabled {
		t.Fatalf("context setting not applied: %#v", loaded.Slack.Context)
	}

	wantManifest, _ := manifest.Render(manifest.Identity{AppName: identity.SlackAppName, BotDisplayName: identity.SlackBotDisplayName})
	assertFileEquals(t, updated.Paths.ManifestFile, wantManifest)
	values, err := godotenv.Read(updated.Paths.EnvFile)
	if err != nil {
		t.Fatal(err)
	}
	if values["CUSTOM_MODEL_KEY"] != secrets.ModelAPIKey || values[SlackBotTokenEnv] != secrets.SlackBotToken || values[SlackAppTokenEnv] != secrets.SlackAppToken {
		t.Fatalf("known secrets not updated: %#v", values)
	}
	if values["UNRELATED"] != "keep" || values["EXTRA_SECRET"] != "preserve" || values["DEEPSEEK_API_KEY"] != "preserve-old-provider-key" {
		t.Fatalf("unrelated env content changed: %#v", values)
	}
	assertMode(t, updated.Paths.EnvFile, 0o600)
	assertMode(t, updated.Paths.ConfigFile, 0o600)
	assertMode(t, updated.Paths.ManifestFile, 0o640)
	assertMode(t, ignorePath, 0o640)
	ignore, _ := os.ReadFile(ignorePath)
	if string(ignore) != "dist/\n.env\n" {
		t.Fatalf(".gitignore = %q", ignore)
	}

	updated, err = service.ApplyConfirmedUpdates(t.Context(), updated, identity, access, secrets)
	if err != nil {
		t.Fatal(err)
	}
	ignore, _ = os.ReadFile(ignorePath)
	if strings.Count(string(ignore), ".env") != 1 {
		t.Fatalf("repeated apply duplicated .env ignore: %q", ignore)
	}
}

func TestApplyOutsideGitDoesNotCreateGitIgnore(t *testing.T) {
	service := newRealService(t)
	root := t.TempDir()
	snapshot, err := service.EnsureBaseArtifacts(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.ApplyConfirmedUpdates(t.Context(), snapshot,
		Identity{AgentName: "Agent", SlackAppName: "App", SlackBotDisplayName: "Bot"},
		AccessControl{AllowedUserIDs: []string{"U12345678"}},
		Secrets{ModelAPIKey: "key", SlackBotToken: "xoxb-token", SlackAppToken: "xapp-token"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".gitignore")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Apply created .gitignore outside Git: %v", err)
	}
}

func TestCancellationAndValidationDoNotWrite(t *testing.T) {
	service := newRealService(t)
	root := t.TempDir()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.EnsureBaseArtifacts(canceled, root); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Ensure error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, config.DefaultProjectStateDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled Ensure created state: %v", err)
	}

	snapshot, err := service.EnsureBaseArtifacts(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	configBefore, _ := os.ReadFile(snapshot.Paths.ConfigFile)
	manifestBefore, _ := os.ReadFile(snapshot.Paths.ManifestFile)
	identity := Identity{AgentName: "New", SlackAppName: "New", SlackBotDisplayName: "New"}
	access := AccessControl{AllowedUserIDs: []string{"U12345678"}}
	validSecrets := Secrets{ModelAPIKey: "key", SlackBotToken: "xoxb-token", SlackAppToken: "xapp-token"}
	if _, err := service.ApplyConfirmedUpdates(canceled, snapshot, identity, access, validSecrets); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Apply error = %v", err)
	}
	assertFileEquals(t, snapshot.Paths.ConfigFile, string(configBefore))
	assertFileEquals(t, snapshot.Paths.ManifestFile, string(manifestBefore))
	if _, err := os.Stat(snapshot.Paths.EnvFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled Apply created .env: %v", err)
	}

	invalid := validSecrets
	invalid.SlackBotToken = "not-a-bot-token"
	if _, err := service.ApplyConfirmedUpdates(t.Context(), snapshot, identity, access, invalid); err == nil {
		t.Fatal("Apply accepted invalid secrets")
	}
	assertFileEquals(t, snapshot.Paths.ConfigFile, string(configBefore))
	assertFileEquals(t, snapshot.Paths.ManifestFile, string(manifestBefore))
	if _, err := os.Stat(snapshot.Paths.EnvFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid Apply created .env: %v", err)
	}
}

func TestEnsureRejectsManagedSymlinkWithoutChangingTarget(t *testing.T) {
	service := newRealService(t)
	root := t.TempDir()
	stateDir := filepath.Join(root, config.DefaultProjectStateDir)
	if err := os.Mkdir(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Marshal(config.Default())
	if err := os.WriteFile(filepath.Join(root, config.DefaultConfigFile), cfg, 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside-manifest")
	if err := os.WriteFile(outside, []byte("outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, config.DefaultManifestFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := service.EnsureBaseArtifacts(t.Context(), root); !errors.Is(err, fsproject.ErrUnsafeSymlink) {
		t.Fatalf("Ensure symlink error = %v", err)
	}
	assertFileEquals(t, outside, "outside\n")
}

func newRealService(t *testing.T) *Service {
	t.Helper()
	service, err := New(fsproject.New(), DatabaseInitializerFunc(func(ctx context.Context, path string) error {
		store, err := adaptersqlite.Initialize(ctx, path)
		if err != nil {
			return err
		}
		return store.Close()
	}), SecretEditorFunc(envfile.Render))
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func assertFileEquals(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want {
		t.Fatalf("%s = %q, want %q", path, data, want)
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != want {
		t.Fatalf("%s mode = %04o, want %04o", path, info.Mode().Perm(), want)
	}
}
