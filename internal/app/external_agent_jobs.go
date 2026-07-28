package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	slackadapter "github.com/Dauno/slack-local-agent/internal/adapter/slack"
	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	externalagent "github.com/Dauno/slack-local-agent/internal/usecase/externalagent"
)

type acpJobDispatcher struct {
	children []preparedAgentTool
	global   string
	store    port.ExternalAgentJobStore
	sanitize func(string) string
}

type acpInvocationRecoverer interface {
	ReconcileInvocation(context.Context, domain.AcpInvocationRequest, string) (domain.AcpInvocationResult, error)
}

func (d *acpJobDispatcher) Run(ctx context.Context, job domain.ExternalAgentJob) (domain.AcpInvocationResult, error) {
	profileMatched := false
	for _, child := range d.children {
		if child.acpRuntime == nil || child.acpResolved == nil {
			continue
		}
		if job.Provider != child.acpResolved.Provider.Name || job.Profile != child.definition.Runtime {
			continue
		}
		profileMatched = true
		if job.RegistryRevision == "" || job.RegistryRevision != child.registryRevision {
			continue
		}
		primary, additional, err := resolveACPProjects(child.projectRoots, job.PrimaryProject, job.AdditionalProjects)
		if err != nil {
			return domain.AcpInvocationResult{}, err
		}
		options := make([]domain.ACPConfigOption, 0, len(child.acpResolved.ConfigOptions))
		for _, option := range child.acpResolved.ConfigOptions {
			options = append(options, domain.ACPConfigOption{ID: option.ID, Value: option.Value})
		}
		result, runErr := child.acpRuntime.Run(ctx, domain.AcpInvocationRequest{
			JobID: job.ID, PrimaryProject: job.PrimaryProject, PrimaryPath: primary,
			AdditionalProjects: append([]string(nil), job.AdditionalProjects...), AdditionalPaths: additional,
			ProfileName: job.Profile, ConfigOptions: options, PermissionOptionKind: child.acpResolved.PermissionOptionKind,
			GlobalInstruction: d.global, AgentInstruction: child.definition.Instruction, Task: job.Task,
			Timeout: time.Until(job.TimeoutAt),
			OnSessionCreated: func(sessionID string) error {
				if d.store == nil {
					return errors.New("durable ACP job store is unavailable")
				}
				return d.store.AssignACPSession(ctx, job.ID, job.LeaseOwner, job.Attempt, sessionID)
			},
			BeforePermission: func() error {
				if d.store == nil {
					return errors.New("durable ACP job store is unavailable")
				}
				return d.store.MarkSideEffectsPossible(ctx, job.ID, job.LeaseOwner, job.Attempt)
			},
			OnSideEffectsPossible: func() error {
				if d.store == nil {
					return errors.New("durable ACP job store is unavailable")
				}
				return d.store.MarkSideEffectsPossible(ctx, job.ID, job.LeaseOwner, job.Attempt)
			},
		})
		if runErr == nil && d.sanitize != nil {
			result.Text = d.sanitize(result.Text)
		}
		return result, runErr
	}
	if profileMatched {
		return domain.AcpInvocationResult{}, errors.New("durable ACP job scope revision does not match current configuration")
	}
	return domain.AcpInvocationResult{}, errors.New("durable ACP job provider/profile is unavailable")
}

func (d *acpJobDispatcher) Reconcile(ctx context.Context, job domain.ExternalAgentJob) (domain.AcpInvocationResult, error) {
	profileMatched := false
	for _, child := range d.children {
		if child.acpRuntime == nil || child.acpResolved == nil || job.Provider != child.acpResolved.Provider.Name || job.Profile != child.definition.Runtime {
			continue
		}
		profileMatched = true
		if job.RegistryRevision == "" || job.RegistryRevision != child.registryRevision {
			continue
		}
		primary, additional, err := resolveACPProjects(child.projectRoots, job.PrimaryProject, job.AdditionalProjects)
		if err != nil {
			return domain.AcpInvocationResult{}, err
		}
		recoverer, ok := child.acpRuntime.(acpInvocationRecoverer)
		if !ok {
			return domain.AcpInvocationResult{}, errors.New("session recovery is unsupported by the composed ACP runtime")
		}
		options := make([]domain.ACPConfigOption, 0, len(child.acpResolved.ConfigOptions))
		for _, option := range child.acpResolved.ConfigOptions {
			options = append(options, domain.ACPConfigOption{ID: option.ID, Value: option.Value})
		}
		result, runErr := recoverer.ReconcileInvocation(ctx, domain.AcpInvocationRequest{
			JobID: job.ID, PrimaryProject: job.PrimaryProject, PrimaryPath: primary,
			AdditionalProjects: append([]string(nil), job.AdditionalProjects...), AdditionalPaths: additional,
			ProfileName: job.Profile, ProviderName: job.Provider, ConfigOptions: options,
			GlobalInstruction: d.global, AgentInstruction: child.definition.Instruction,
			PermissionOptionKind: child.acpResolved.PermissionOptionKind, Timeout: time.Until(job.TimeoutAt),
		}, job.ACPSessionID)
		if runErr == nil && d.sanitize != nil {
			result.Text = d.sanitize(result.Text)
		}
		return result, runErr
	}
	if profileMatched {
		return domain.AcpInvocationResult{}, errors.New("durable ACP recovery scope revision does not match current configuration")
	}
	return domain.AcpInvocationResult{}, errors.New("durable ACP job provider/profile is unavailable")
}

type slackJobPublisher struct {
	publisher port.ResponsePublisher
	sanitize  func(string) string
}

func (p *slackJobPublisher) PublishJobTerminal(ctx context.Context, job domain.ExternalAgentJob) error {
	if p == nil || p.publisher == nil {
		return errors.New("job terminal publisher is not configured")
	}
	switch job.Status {
	case domain.JobCompletionUnknown, domain.JobCompleted, domain.JobFailed, domain.JobCancelled, domain.JobAbandoned:
	default:
		return errors.New("cannot publish a non-terminal external-agent job status")
	}
	target, err := replyTargetForConversation(job.ConversationKey)
	if err != nil {
		return err
	}
	status := fmt.Sprintf("OpenCode job `%s` %s.", job.ID, job.Status)
	switch job.Status {
	case domain.JobCompleted:
		if summary := boundedSanitized(p.sanitize, job.ResultSummary, 2000); summary != "" {
			status += "\n\nSummary: " + summary
		}
	case domain.JobCompletionUnknown:
		status = fmt.Sprintf("OpenCode job `%s` was interrupted after external actions may have occurred. It was not retried; reconciliation is required.", job.ID)
	case domain.JobFailed:
		status += " The operation failed with a host-owned error code."
	}
	_, err = p.publisher.Publish(ctx, target, status)
	return err
}

func newExternalAgentJobService(cfg config.Config, models runtimeModels, infra *runtimeInfrastructure) (*externalagent.Service, *externalagent.NotificationWorker, error) {
	var children []preparedAgentTool
	for _, child := range models.preparedAgentTools {
		if child.acpRuntime != nil {
			children = append(children, child)
		}
	}
	if len(children) == 0 {
		return nil, nil, nil
	}
	store := adaptersqlite.NewExternalAgentJobStore(infra.store)
	if store == nil {
		return nil, nil, errors.New("initialize external-agent job store")
	}
	global := ""
	if models.rootDef != nil {
		global = models.rootDef.EffectiveDelegatedGlobalInstruction()
	}
	service, err := externalagent.New(externalagent.Config{
		DefaultTimeout: time.Duration(cfg.ACP.DefaultJobTimeoutSeconds) * time.Second,
		MaxTimeout:     time.Duration(cfg.ACP.MaxJobTimeoutSeconds) * time.Second,
		LeaseTTL:       30 * time.Second, PollInterval: time.Second, Concurrency: cfg.ACP.WorkerConcurrency, MaxAttempts: 2,
	}, externalagent.Dependencies{
		Store: store, Runtime: &acpJobDispatcher{children: children, global: global, store: store, sanitize: models.redactor.String},
		Publisher: nil,
	})
	if err != nil {
		return nil, nil, err
	}
	notificationPublisher := slackadapter.NewJobNotificationPublisher(infra.publisher, infra.history)
	notificationWorker, err := externalagent.NewNotificationWorker(externalagent.NotificationConfig{PollInterval: time.Second, LeaseTTL: 30 * time.Second}, externalagent.NotificationDependencies{Store: store, Publisher: notificationPublisher})
	if err != nil {
		return nil, nil, err
	}
	return service, notificationWorker, nil
}

func replyTargetForConversation(key domain.ConversationKey) (domain.ReplyTarget, error) {
	parts := strings.Split(string(key), ":")
	if len(parts) < 4 || parts[0] != "slack" {
		return domain.ReplyTarget{}, errors.New("job conversation key is malformed")
	}
	target := domain.ReplyTarget{ChannelID: parts[3]}
	for index := 4; index+1 < len(parts); index++ {
		if parts[index] == "thread" {
			target.ThreadTS = parts[index+1]
			break
		}
	}
	return target, nil
}

func boundedSanitized(sanitize func(string) string, value string, max int) string {
	if sanitize != nil {
		value = sanitize(value)
	}
	if len([]rune(value)) <= max {
		return value
	}
	runes := []rune(value)
	return string(runes[:max]) + "…"
}
