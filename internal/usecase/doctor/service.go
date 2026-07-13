package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Dauno/slack-local-agent/internal/agentdef"
	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/secure"
)

const (
	SlackBotTokenKey = "SLACK_BOT_TOKEN"
	SlackAppTokenKey = "SLACK_APP_TOKEN"
)

type SecretResolver interface {
	Resolve(keys ...string) (map[string]string, error)
}

type DatabaseChecker interface {
	CheckDatabase(ctx context.Context, path string) error
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
	CheckModel(ctx context.Context, model config.ModelConfig, apiKey string) error
	CheckResolvedModel(ctx context.Context, resolved *agentdef.ResolvedModel, apiKey string) error
}

type Dependencies struct {
	ConfigPath string
	LoadConfig func(path string) (config.Config, error)
	Secrets    SecretResolver
	Database   DatabaseChecker
	Live       LiveChecker
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

func (r Report) Passed() bool { return r.ExitCode() == 0 }

type Service struct {
	deps Dependencies
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
		defs           *agentdef.Definitions
		resolvedModel  *agentdef.ResolvedModel
		defsLoadFailed bool
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
				report.pass("agent definitions", fmt.Sprintf("%d providers, %d agents loaded", len(defs.Providers), len(defs.Agents)))
			}
		}
	}

	modelAPIKeyEnv := cfg.Model.APIKeyEnv
	if resolvedModel != nil {
		modelAPIKeyEnv = resolvedModel.APIKeyEnv
	}
	keys := []string{modelAPIKeyEnv, SlackBotTokenKey, SlackAppTokenKey}
	values, err := s.deps.Secrets.Resolve(keys...)
	if err != nil {
		report.fail("secrets", err.Error(), "Fix .env syntax or process environment values.", false)
		return report
	}
	redactor := secure.NewRedactor(values[modelAPIKeyEnv], values[SlackBotTokenKey], values[SlackAppTokenKey])

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
	checkSecret("model API key", modelAPIKeyEnv, "", "Set "+modelAPIKeyEnv+" in the process environment or .env.")
	checkSecret("Slack bot token", SlackBotTokenKey, "xoxb-", "Set a Bot User OAuth Token beginning with xoxb-.")
	checkSecret("Slack app token", SlackAppTokenKey, "xapp-", "Set an app-level Socket Mode token beginning with xapp- and connections:write.")

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
	return report
}

func checkTimeout(ctx context.Context, seconds int) (context.Context, context.CancelFunc) {
	if seconds <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
}

func (r *Report) pass(name, detail string) {
	r.Results = append(r.Results, Result{Name: name, Status: StatusPass, Detail: detail})
}

func (r *Report) fail(name, detail, remediation string, fatal bool) {
	r.Results = append(r.Results, Result{
		Name: name, Status: StatusFail, Detail: detail, Remediation: remediation, Fatal: fatal,
	})
}
