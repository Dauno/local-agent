package app

import (
	"context"
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	adaptersqlite "github.com/Dauno/slack-local-agent/internal/adapter/sqlite"
	"github.com/Dauno/slack-local-agent/internal/agentdef"
	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/secure"
)

func TestDurableACPDispatcherRejectsScopeRevisionDrift(t *testing.T) {
	workspace := t.TempDir()
	runtime := &fakeExternalRuntime{result: domain.AcpInvocationResult{Text: "must not run"}}
	child := preparedAgentTool{
		definition:   agentdef.AgentDef{Name: "worker", Runtime: "opencode/build", ExecutionMode: agentdef.ExecutionModeDurableJob},
		acpRuntime:   runtime,
		acpResolved:  &agentdef.ResolvedModel{Provider: agentdef.Provider{Name: "opencode", Type: agentdef.ProviderTypeACP}},
		projectRoots: map[string]string{"workspace": workspace}, registryRevision: "sha256:current",
	}
	_, err := (&acpJobDispatcher{children: []preparedAgentTool{child}}).Run(context.Background(), domain.ExternalAgentJob{
		ID: "job-1", Provider: "opencode", Profile: "opencode/build", PrimaryProject: "workspace", RegistryRevision: "sha256:approved", Task: "task",
	})
	if err == nil || !strings.Contains(err.Error(), "scope revision") || runtime.runs != 0 {
		t.Fatalf("err = %v, runtime runs = %d", err, runtime.runs)
	}
}

func TestNoACPConfigurationReturnsNilJobService(t *testing.T) {
	service, worker, err := newExternalAgentJobService(config.Default(), newRuntimeModels(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if service != nil || worker != nil {
		t.Fatalf("no-ACP configuration returned service=%v worker=%v", service, worker)
	}
}

func TestDurableACPDispatcherDisambiguatesSharedRuntimeByScopeRevision(t *testing.T) {
	workspace := t.TempDir()
	wrongRuntime := &fakeExternalRuntime{result: domain.AcpInvocationResult{Text: "wrong agent"}}
	rightRuntime := &fakeExternalRuntime{result: domain.AcpInvocationResult{Text: "right agent"}}
	resolved := &agentdef.ResolvedModel{Provider: agentdef.Provider{Name: "opencode", Type: agentdef.ProviderTypeACP}}
	children := []preparedAgentTool{
		{
			definition: agentdef.AgentDef{Name: "improve_agent", Runtime: "opencode/sol-high"},
			acpRuntime: wrongRuntime, acpResolved: resolved,
			projectRoots: map[string]string{"workspace": workspace}, registryRevision: "sha256:improve",
		},
		{
			definition: agentdef.AgentDef{Name: "sol-advisor", Runtime: "opencode/sol-high"},
			acpRuntime: rightRuntime, acpResolved: resolved,
			projectRoots: map[string]string{"workspace": workspace}, registryRevision: "sha256:advisor",
		},
	}
	result, err := (&acpJobDispatcher{children: children}).Run(context.Background(), domain.ExternalAgentJob{
		ID: "job-1", Provider: "opencode", Profile: "opencode/sol-high", PrimaryProject: "workspace", RegistryRevision: "sha256:advisor", Task: "review",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "right agent" || wrongRuntime.runs != 0 || rightRuntime.runs != 1 {
		t.Fatalf("result = %q, wrong runs = %d, right runs = %d", result.Text, wrongRuntime.runs, rightRuntime.runs)
	}
}

func TestDetachedACPTimeoutFallbackDoesNotUseRootModelTimeout(t *testing.T) {
	cfg := config.Default()
	cfg.Runtime.ModelTimeoutSeconds = 5
	cfg.ACP.DefaultJobTimeoutSeconds = 7200
	definition := agentdef.AgentDef{ExecutionMode: agentdef.ExecutionModeDurableJob}
	if got, want := acpAgentFallback(definition, cfg), 2*time.Hour; got != want {
		t.Fatalf("detached fallback = %s, want %s", got, want)
	}
}

func TestAgentExecutionFingerprintChangesWithScopeInputs(t *testing.T) {
	definition := agentdef.AgentDef{Name: "worker", Runtime: "opencode/build"}
	resolved := &agentdef.ResolvedModel{Provider: agentdef.Provider{Name: "opencode", Type: agentdef.ProviderTypeACP}}
	cfg := config.Default()
	left, err := agentExecutionFingerprint(definition, resolved, map[string]string{"workspace": "/workspace"}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ACP.DefaultJobTimeoutSeconds++
	right, err := agentExecutionFingerprint(definition, resolved, map[string]string{"workspace": "/workspace"}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if left == "" || left == right || !strings.HasPrefix(left, "sha256:") {
		t.Fatalf("fingerprints = %q, %q", left, right)
	}
}

func TestDurableACPDispatcherMaterializesSanitizedCompleteResult(t *testing.T) {
	workspace := t.TempDir()
	artifacts := &recordingResultArtifacts{}
	child := preparedAgentTool{
		definition:   agentdef.AgentDef{Name: "worker", Runtime: "opencode/build", ExecutionMode: agentdef.ExecutionModeDurableJob},
		acpRuntime:   &fakeExternalRuntime{result: domain.AcpInvocationResult{Text: "safe <result>"}},
		acpResolved:  &agentdef.ResolvedModel{Provider: agentdef.Provider{Name: "opencode", Type: agentdef.ProviderTypeACP}},
		projectRoots: map[string]string{"workspace": workspace}, registryRevision: "rev-1",
	}
	dispatcher := &acpJobDispatcher{children: []preparedAgentTool{child}, artifacts: artifacts, sanitize: func(value string) string { return value }, policy: domain.ResultDeliveryPolicy{
		MaxMarkdownParts: 6, MaxFileBytes: 1024 * 1024, MaxInlineResultBytes: 64 * 1024, MaxResultArtifactBytes: 1024 * 1024,
	}}
	result, err := dispatcher.Run(context.Background(), domain.ExternalAgentJob{ID: "job_1", Mode: domain.JobDetached, Provider: "opencode", Profile: "opencode/build", PrimaryProject: "workspace", RegistryRevision: "rev-1", Task: "task", TimeoutAt: time.Now().Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if result.DeliveryMode != domain.JobResultDeliveryMarkdown || result.Text != "safe &lt;result>" || result.DeliveryContentSHA256 == "" || len(artifacts.contents) != 0 {
		t.Fatalf("materialized result = %+v, artifacts = %#v", result, artifacts.contents)
	}
	largeArtifacts := &recordingResultArtifacts{}
	dispatcher.artifacts = largeArtifacts
	child.acpRuntime = &fakeExternalRuntime{result: domain.AcpInvocationResult{Text: strings.Repeat("x", domain.SlackMarkdownChunkRunes*6+1)}}
	dispatcher.children = []preparedAgentTool{child}
	large, err := dispatcher.Run(context.Background(), domain.ExternalAgentJob{ID: "job_2", Mode: domain.JobDetached, Provider: "opencode", Profile: "opencode/build", PrimaryProject: "workspace", RegistryRevision: "rev-1", Task: "task", TimeoutAt: time.Now().Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if large.DeliveryMode != domain.JobResultDeliveryFile || large.Text != "" || len(largeArtifacts.contents) != 1 {
		t.Fatalf("large materialized result = %+v, artifacts = %#v", large, largeArtifacts.contents)
	}
}

func TestDurableACPDispatcherFallsBackForUnicodeTwentyThousandCharacters(t *testing.T) {
	artifacts := &recordingResultArtifacts{}
	workspace := t.TempDir()
	child := preparedAgentTool{
		definition:   agentdef.AgentDef{Name: "worker", Runtime: "opencode/build", ExecutionMode: agentdef.ExecutionModeDurableJob},
		acpRuntime:   &fakeExternalRuntime{result: domain.AcpInvocationResult{Text: strings.Repeat("界", 20000)}},
		acpResolved:  &agentdef.ResolvedModel{Provider: agentdef.Provider{Name: "opencode", Type: agentdef.ProviderTypeACP}},
		projectRoots: map[string]string{"workspace": workspace}, registryRevision: "rev-1",
	}
	dispatcher := &acpJobDispatcher{children: []preparedAgentTool{child}, artifacts: artifacts, policy: domain.ResultDeliveryPolicy{
		MaxMarkdownParts: 1, MaxFileBytes: 1024 * 1024, MaxInlineResultBytes: domain.SlackMarkdownChunkRunes, MaxResultArtifactBytes: 1024 * 1024,
	}}
	result, err := dispatcher.Run(context.Background(), domain.ExternalAgentJob{ID: "job_unicode", Mode: domain.JobDetached, Provider: "opencode", Profile: "opencode/build", PrimaryProject: "workspace", RegistryRevision: "rev-1", Task: "task", TimeoutAt: time.Now().Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if result.DeliveryMode != domain.JobResultDeliveryFile || result.Text != "" || len(artifacts.contents) != 1 || len([]rune(artifacts.contents["job_unicode-delivery"])) != 20000 {
		t.Fatalf("Unicode fallback result=%+v artifacts=%#v", result, artifacts.contents)
	}
}

func TestDurableACPMaterializationRedactsBeforeSQLiteDelivery(t *testing.T) {
	secret := "xoxb-super-secret-value-12345"
	workspace := t.TempDir()
	child := preparedAgentTool{
		definition:   agentdef.AgentDef{Name: "worker", Runtime: "opencode/build", ExecutionMode: agentdef.ExecutionModeDurableJob},
		acpRuntime:   &fakeExternalRuntime{result: domain.AcpInvocationResult{Text: "result=" + secret}},
		acpResolved:  &agentdef.ResolvedModel{Provider: agentdef.Provider{Name: "opencode", Type: agentdef.ProviderTypeACP}},
		projectRoots: map[string]string{"workspace": workspace}, registryRevision: "rev-1",
	}
	dispatcher := &acpJobDispatcher{children: []preparedAgentTool{child}, sanitize: secure.NewRedactor(secret).String, policy: domain.ResultDeliveryPolicy{
		MaxMarkdownParts: 6, MaxFileBytes: 1024 * 1024, MaxInlineResultBytes: 64 * 1024, MaxResultArtifactBytes: 1024 * 1024,
	}}
	result, err := dispatcher.Run(context.Background(), domain.ExternalAgentJob{ID: "job_secret", Mode: domain.JobDetached, Provider: "opencode", Profile: "opencode/build", PrimaryProject: "workspace", RegistryRevision: "rev-1", Task: "task", ConversationKey: "slack:T12345678:dm:D12345678", Actor: "U12345678", TimeoutAt: time.Now().Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result.Text, secret) || strings.Contains(result.DeliveryCanonicalMarkdown, secret) {
		t.Fatalf("secret survived materialization: %+v", result)
	}
	store, err := adaptersqlite.Initialize(context.Background(), filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobs := adaptersqlite.NewExternalAgentJobStore(store)
	job := domain.ExternalAgentJob{ID: "job_secret", Mode: domain.JobDetached, Provider: "opencode", Profile: "build", PrimaryProject: "workspace", RegistryRevision: "rev-1", Task: "task", Actor: "U12345678", TeamID: "T12345678", ConversationKey: "slack:T12345678:dm:D12345678", Status: domain.JobQueued, TimeoutAt: time.Now().Add(time.Minute), CreatedAt: time.Now(), UpdatedAt: time.Now(), OriginalCallID: "secret-call", RequestSHA256: "request"}
	if created, _, err := jobs.CreateIfAbsent(t.Context(), job); err != nil || !created {
		t.Fatalf("create job = %v, err = %v", created, err)
	}
	claimed, err := jobs.ClaimNext(t.Context(), time.Now().UTC(), "worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := jobs.Transition(t.Context(), job.ID, claimed.LeaseOwner, claimed.Attempt, domain.JobCompleted, &result, "", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	stored, err := jobs.GetJob(t.Context(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored.ResultSummary, secret) {
		t.Fatal("secret survived SQLite job persistence")
	}
	notification, err := jobs.ClaimNextNotification(t.Context(), time.Now().Add(time.Second), "publisher", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if notification == nil || strings.Contains(notification.CanonicalMarkdown, secret) {
		t.Fatalf("notification leaked secret: %#v", notification)
	}
}

type recordingResultArtifacts struct{ contents map[string]string }

func (s *recordingResultArtifacts) Put(_ context.Context, ownerID, content string) (domain.ResultArtifact, error) {
	if s.contents == nil {
		s.contents = make(map[string]string)
	}
	s.contents[ownerID] = content
	digest := sha256.Sum256([]byte(content))
	return domain.ResultArtifact{Reference: ownerID + ".result", SHA256: fmt.Sprintf("%x", digest), Bytes: int64(len([]byte(content)))}, nil
}

func (*recordingResultArtifacts) Get(context.Context, string, string, string, int64) ([]byte, error) {
	return nil, fmt.Errorf("unexpected artifact read")
}

var _ port.ResultArtifactStore = (*recordingResultArtifacts)(nil)
