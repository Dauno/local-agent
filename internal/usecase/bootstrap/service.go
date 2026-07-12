// Package bootstrap orchestrates the two non-overlapping setup phases. It does
// not prompt users and it does not depend on concrete filesystem or database
// adapters.
package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/manifest"
)

const (
	SlackBotTokenEnv = "SLACK_BOT_TOKEN"
	SlackAppTokenEnv = "SLACK_APP_TOKEN"
)

type ProjectFiles interface {
	CanonicalRoot(projectRoot string) (string, error)
	EnsureDirectory(ctx context.Context, path string, mode fs.FileMode) error
	CheckRegularFileOrMissing(ctx context.Context, path string) error
	ReadFile(ctx context.Context, path string) ([]byte, error)
	CreateFile(ctx context.Context, path string, content []byte, mode fs.FileMode) (bool, error)
	PrepareGitIgnore(ctx context.Context, projectRoot string) (path string, content []byte, changed bool, err error)
	WriteBatch(ctx context.Context, contents map[string][]byte, defaultModes map[string]fs.FileMode, forceModes map[string]bool) error
}

type DatabaseInitializer interface {
	Initialize(ctx context.Context, path string) error
}

type DatabaseInitializerFunc func(context.Context, string) error

func (f DatabaseInitializerFunc) Initialize(ctx context.Context, path string) error {
	return f(ctx, path)
}

type SecretEditor interface {
	Render(existing []byte, allowedKeys []string, updates map[string]string) ([]byte, error)
}

type SecretEditorFunc func([]byte, []string, map[string]string) ([]byte, error)

func (f SecretEditorFunc) Render(existing []byte, allowedKeys []string, updates map[string]string) ([]byte, error) {
	return f(existing, allowedKeys, updates)
}

type Service struct {
	files    ProjectFiles
	database DatabaseInitializer
	secrets  SecretEditor
}

func New(files ProjectFiles, database DatabaseInitializer, secrets SecretEditor) (*Service, error) {
	if files == nil {
		return nil, errors.New("bootstrap project files are required")
	}
	if database == nil {
		return nil, errors.New("bootstrap database initializer is required")
	}
	if secrets == nil {
		return nil, errors.New("bootstrap secret editor is required")
	}
	return &Service{files: files, database: database, secrets: secrets}, nil
}

type Snapshot struct {
	ProjectRoot string
	Config      config.Config
	Paths       config.Paths
}

type Identity struct {
	AgentName           string
	SlackAppName        string
	SlackBotDisplayName string
}

type AccessControl struct {
	AllowAllUsers     bool
	AllowedUserIDs    []string
	AllowedTeamIDs    []string
	AllowedChannelIDs []string
	ContextEnabled    bool
}

type Secrets struct {
	ModelAPIKey   string
	SlackBotToken string
	SlackAppToken string
}

// EnsureBaseArtifacts is phase one. It creates only missing defaults, never
// creates .env, and initializes rather than replaces the configured database.
func (s *Service) EnsureBaseArtifacts(ctx context.Context, projectRoot string) (Snapshot, error) {
	root, err := s.files.CanonicalRoot(projectRoot)
	if err != nil {
		return Snapshot{}, err
	}
	if err := checkContext(ctx); err != nil {
		return Snapshot{}, err
	}

	configPath, err := config.ConfigPath(root)
	if err != nil {
		return Snapshot{}, err
	}
	if err := s.files.EnsureDirectory(ctx, filepath.Dir(configPath), 0o755); err != nil {
		return Snapshot{}, fmt.Errorf("create local-agent artifact directory: %w", err)
	}

	cfg, err := s.loadOrCreateConfig(ctx, configPath)
	if err != nil {
		return Snapshot{}, err
	}
	paths, err := cfg.ResolvePaths(root)
	if err != nil {
		return Snapshot{}, fmt.Errorf("resolve bootstrap paths: %w", err)
	}

	if err := s.files.EnsureDirectory(ctx, paths.StateDir, 0o755); err != nil {
		return Snapshot{}, fmt.Errorf("create configured state directory: %w", err)
	}
	if err := s.files.EnsureDirectory(ctx, filepath.Dir(paths.DatabaseFile), 0o755); err != nil {
		return Snapshot{}, fmt.Errorf("create database directory: %w", err)
	}

	renderedManifest, err := manifest.Render(manifest.Identity{
		AppName: cfg.Slack.AppName, BotDisplayName: cfg.Slack.BotDisplayName,
	})
	if err != nil {
		return Snapshot{}, err
	}
	if _, err := s.files.CreateFile(ctx, paths.ManifestFile, []byte(renderedManifest), 0o644); err != nil {
		return Snapshot{}, fmt.Errorf("create Slack manifest: %w", err)
	}
	if _, err := s.files.CreateFile(ctx, paths.EnvExampleFile, renderEnvExample(cfg.Model.APIKeyEnv), 0o644); err != nil {
		return Snapshot{}, fmt.Errorf("create local environment example: %w", err)
	}

	if err := s.files.CheckRegularFileOrMissing(ctx, paths.DatabaseFile); err != nil {
		return Snapshot{}, fmt.Errorf("validate SQLite path: %w", err)
	}
	if err := s.database.Initialize(ctx, paths.DatabaseFile); err != nil {
		return Snapshot{}, fmt.Errorf("initialize SQLite database: %w", err)
	}
	if err := checkContext(ctx); err != nil {
		return Snapshot{}, err
	}

	return Snapshot{ProjectRoot: root, Config: cfg, Paths: paths}, nil
}

func (s *Service) loadOrCreateConfig(ctx context.Context, path string) (config.Config, error) {
	data, err := s.files.ReadFile(ctx, path)
	if err == nil {
		cfg, parseErr := config.Parse(data)
		if parseErr != nil {
			return config.Config{}, fmt.Errorf("parse existing configuration: %w", parseErr)
		}
		return cfg, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return config.Config{}, err
	}

	cfg := config.Default()
	data, err = config.Marshal(cfg)
	if err != nil {
		return config.Config{}, fmt.Errorf("render default configuration: %w", err)
	}
	created, err := s.files.CreateFile(ctx, path, data, 0o644)
	if err != nil {
		return config.Config{}, fmt.Errorf("create default configuration: %w", err)
	}
	if created {
		return cfg, nil
	}
	// A concurrent initializer created the file after the initial read.
	data, err = s.files.ReadFile(ctx, path)
	if err != nil {
		return config.Config{}, err
	}
	cfg, err = config.Parse(data)
	if err != nil {
		return config.Config{}, fmt.Errorf("parse concurrently created configuration: %w", err)
	}
	return cfg, nil
}

// ApplyConfirmedUpdates is phase two. All values are validated and all output
// is staged before the first managed project file is replaced.
func (s *Service) ApplyConfirmedUpdates(
	ctx context.Context,
	snapshot Snapshot,
	identity Identity,
	access AccessControl,
	secrets Secrets,
) (Snapshot, error) {
	root, err := s.files.CanonicalRoot(snapshot.ProjectRoot)
	if err != nil {
		return Snapshot{}, err
	}
	if filepath.Clean(root) != filepath.Clean(snapshot.ProjectRoot) {
		return Snapshot{}, errors.New("bootstrap snapshot project root changed")
	}
	if err := checkContext(ctx); err != nil {
		return Snapshot{}, err
	}
	if err := validateIdentity(identity); err != nil {
		return Snapshot{}, err
	}
	if err := validateSecrets(secrets); err != nil {
		return Snapshot{}, err
	}

	configPath, err := config.ConfigPath(root)
	if err != nil {
		return Snapshot{}, err
	}
	existingConfig, err := s.files.ReadFile(ctx, configPath)
	if err != nil {
		return Snapshot{}, fmt.Errorf("reload configuration before confirmed update: %w", err)
	}
	cfg, err := config.Parse(existingConfig)
	if err != nil {
		return Snapshot{}, fmt.Errorf("parse configuration before confirmed update: %w", err)
	}

	cfg.Agent.Name = identity.AgentName
	cfg.Slack.AppName = identity.SlackAppName
	cfg.Slack.BotDisplayName = identity.SlackBotDisplayName
	cfg.Slack.AllowAllUsers = access.AllowAllUsers
	cfg.Slack.AllowedUserIDs = slices.Clone(access.AllowedUserIDs)
	cfg.Slack.AllowedTeamIDs = slices.Clone(access.AllowedTeamIDs)
	cfg.Slack.AllowedChannelIDs = slices.Clone(access.AllowedChannelIDs)
	cfg.Slack.Context.Enabled = access.ContextEnabled
	if err := cfg.Validate(); err != nil {
		return Snapshot{}, err
	}
	paths, err := cfg.ResolvePaths(root)
	if err != nil {
		return Snapshot{}, err
	}

	configContent, err := config.Marshal(cfg)
	if err != nil {
		return Snapshot{}, fmt.Errorf("render confirmed configuration: %w", err)
	}
	manifestContent, err := manifest.Render(manifest.Identity{
		AppName: cfg.Slack.AppName, BotDisplayName: cfg.Slack.BotDisplayName,
	})
	if err != nil {
		return Snapshot{}, err
	}
	allowedSecretKeys := []string{cfg.Model.APIKeyEnv, SlackBotTokenEnv, SlackAppTokenEnv}
	secretUpdates := map[string]string{
		cfg.Model.APIKeyEnv: secrets.ModelAPIKey,
		SlackBotTokenEnv:    secrets.SlackBotToken,
		SlackAppTokenEnv:    secrets.SlackAppToken,
	}
	existingEnv, err := s.files.ReadFile(ctx, paths.EnvFile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Snapshot{}, fmt.Errorf("read existing secret file: %w", err)
	}
	envContent, err := s.secrets.Render(existingEnv, allowedSecretKeys, secretUpdates)
	if err != nil {
		return Snapshot{}, fmt.Errorf("prepare confirmed secret update: %w", err)
	}
	gitIgnorePath, gitIgnoreContent, gitIgnoreChanged, err := s.files.PrepareGitIgnore(ctx, root)
	if err != nil {
		return Snapshot{}, fmt.Errorf("prepare Git ignore update: %w", err)
	}

	contents := map[string][]byte{
		paths.ConfigFile:   configContent,
		paths.ManifestFile: []byte(manifestContent),
		paths.EnvFile:      envContent,
	}
	modes := map[string]fs.FileMode{
		paths.ConfigFile:   0o644,
		paths.ManifestFile: 0o644,
		paths.EnvFile:      0o600,
	}
	forceModes := map[string]bool{paths.EnvFile: true}
	if gitIgnoreChanged {
		contents[gitIgnorePath] = gitIgnoreContent
		modes[gitIgnorePath] = 0o644
	}
	if err := s.files.WriteBatch(ctx, contents, modes, forceModes); err != nil {
		return Snapshot{}, fmt.Errorf("apply confirmed bootstrap updates: %w", err)
	}

	return Snapshot{ProjectRoot: root, Config: cfg, Paths: paths}, nil
}

func renderEnvExample(apiKeyEnv string) []byte {
	return []byte(fmt.Sprintf(`# Sensitive values only. Copy these keys to the project .env and replace the placeholders.
%s=...
%s=xoxb-...
%s=xapp-...
`, apiKeyEnv, SlackBotTokenEnv, SlackAppTokenEnv))
}

func validateIdentity(identity Identity) error {
	values := map[string]string{
		"agent name":             identity.AgentName,
		"Slack app name":         identity.SlackAppName,
		"Slack bot display name": identity.SlackBotDisplayName,
	}
	for name, value := range values {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", name)
		}
		if strings.ContainsAny(value, "\r\n\x00") {
			return fmt.Errorf("%s must be a single line", name)
		}
	}
	return nil
}

func validateSecrets(secrets Secrets) error {
	values := map[string]string{
		"model API key":   secrets.ModelAPIKey,
		"Slack bot token": secrets.SlackBotToken,
		"Slack app token": secrets.SlackAppToken,
	}
	for name, value := range values {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", name)
		}
		if strings.ContainsAny(value, "\r\n\x00") {
			return fmt.Errorf("%s must be a single line", name)
		}
	}
	if !strings.HasPrefix(secrets.SlackBotToken, "xoxb-") || len(secrets.SlackBotToken) == len("xoxb-") {
		return errors.New("Slack bot token must start with xoxb-")
	}
	if !strings.HasPrefix(secrets.SlackAppToken, "xapp-") || len(secrets.SlackAppToken) == len("xapp-") {
		return errors.New("Slack app token must start with xapp-")
	}
	return nil
}

func checkContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("context is required")
	}
	return ctx.Err()
}
