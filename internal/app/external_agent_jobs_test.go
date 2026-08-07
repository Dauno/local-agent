package app

import (
	"context"
	"crypto/sha256"
	"errors"
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

func TestDurableACPDispatcherNormalizesForegroundResultIdentity(t *testing.T) {
	workspace := t.TempDir()
	policy := domain.ResultDeliveryPolicy{
		MaxMarkdownParts: 6, MaxFileBytes: 1024 * 1024, MaxInlineResultBytes: 64 * 1024, MaxResultArtifactBytes: 1024 * 1024,
	}
	cases := []struct {
		name   string
		raw    string
		redact func(string) string
		final  string
	}{
		{
			name: "redaction shortens before identity",
			raw:  "token=long-secret-value-0123456789",
			redact: func(value string) string {
				return strings.ReplaceAll(value, "long-secret-value-0123456789", "[REDACTED]")
			},
			final: "token=[REDACTED]",
		},
		{
			name: "redaction lengthens before identity",
			raw:  "value=tok",
			redact: func(value string) string {
				return strings.ReplaceAll(value, "tok", "token-value-0123456789")
			},
			final: "value=token-value-0123456789",
		},
		{
			name:   "angle bracket and control characters are sanitized",
			raw:    "a< b\x00c\x07d",
			redact: func(value string) string { return value },
			final:  "a&lt; bcd",
		},
		{
			name:   "multibyte UTF-8 counts bytes not runes",
			raw:    strings.Repeat("界", 10),
			redact: func(value string) string { return value },
			final:  strings.Repeat("界", 10),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			child := preparedAgentTool{
				definition:   agentdef.AgentDef{Name: "worker", Runtime: "opencode/build", ExecutionMode: agentdef.ExecutionModeDurableJob},
				acpRuntime:   &fakeExternalRuntime{result: domain.AcpInvocationResult{Text: tc.raw}},
				acpResolved:  &agentdef.ResolvedModel{Provider: agentdef.Provider{Name: "opencode", Type: agentdef.ProviderTypeACP}},
				projectRoots: map[string]string{"workspace": workspace}, registryRevision: "rev-1",
			}
			dispatcher := &acpJobDispatcher{children: []preparedAgentTool{child}, sanitize: tc.redact, policy: policy}
			result, err := dispatcher.Run(context.Background(), domain.ExternalAgentJob{
				ID: "job_fg", Mode: domain.JobForeground, Provider: "opencode", Profile: "opencode/build",
				PrimaryProject: "workspace", RegistryRevision: "rev-1", Task: "task", TimeoutAt: time.Now().Add(time.Minute),
			})
			if err != nil {
				t.Fatal(err)
			}
			wantBytes := int64(len([]byte(tc.final)))
			digest := sha256.Sum256([]byte(tc.final))
			wantSHA := fmt.Sprintf("%x", digest)
			if result.Text != tc.final || result.ResultBytes != wantBytes || result.ResultSHA256 != wantSHA {
				t.Fatalf("foreground identity = %+v, want text %q bytes %d sha %s", result, tc.final, wantBytes, wantSHA)
			}
			if result.DeliveryMode != "" || result.DeliveryCanonicalMarkdown != "" || result.DeliveryPolicyVersion != "" || result.DeliveryArtifactRef != "" || result.Inline || result.ArtifactRef != "" {
				t.Fatalf("foreground result carried detached delivery metadata: %+v", result)
			}
			if wantBytes != int64(len([]rune(tc.final))) && result.ResultBytes == int64(len([]rune(tc.final))) {
				t.Fatalf("result bytes counted runes instead of UTF-8 bytes: %d", result.ResultBytes)
			}
		})
	}
}

func TestDurableACPDispatcherForegroundResultPersistsCompleteIdentity(t *testing.T) {
	secret := "xoxb-foreground-secret-value-42"
	workspace := t.TempDir()
	redact := func(value string) string { return strings.ReplaceAll(value, secret, "[REDACTED]") }
	child := preparedAgentTool{
		definition:   agentdef.AgentDef{Name: "worker", Runtime: "opencode/build", ExecutionMode: agentdef.ExecutionModeDurableJob},
		acpRuntime:   &fakeExternalRuntime{result: domain.AcpInvocationResult{Text: "result=" + secret + " <raw>"}},
		acpResolved:  &agentdef.ResolvedModel{Provider: agentdef.Provider{Name: "opencode", Type: agentdef.ProviderTypeACP}},
		projectRoots: map[string]string{"workspace": workspace}, registryRevision: "rev-1",
	}
	artifacts := &recordingResultArtifacts{}
	dispatcher := &acpJobDispatcher{children: []preparedAgentTool{child}, artifacts: artifacts, sanitize: redact, policy: domain.ResultDeliveryPolicy{
		MaxMarkdownParts: 6, MaxFileBytes: 1024 * 1024, MaxInlineResultBytes: 64 * 1024, MaxResultArtifactBytes: 1024 * 1024,
	}}
	result, err := dispatcher.Run(context.Background(), domain.ExternalAgentJob{
		ID: "job_fg", Mode: domain.JobForeground, Provider: "opencode", Profile: "opencode/build",
		PrimaryProject: "workspace", RegistryRevision: "rev-1", Task: "task", Actor: "U12345678", TeamID: "T12345678",
		ConversationKey: "slack:T12345678:dm:D12345678", TimeoutAt: time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	final := "result=[REDACTED] &lt;raw>"
	digest := sha256.Sum256([]byte(final))
	if result.Text != final || result.ResultBytes != int64(len([]byte(final))) || result.ResultSHA256 != fmt.Sprintf("%x", digest) {
		t.Fatalf("foreground result = %+v, want %q with exact identity", result, final)
	}
	if result.DeliveryMode != "" || len(artifacts.contents) != 0 {
		t.Fatalf("foreground run produced detached delivery: result=%+v artifacts=%#v", result, artifacts.contents)
	}
	store, err := adaptersqlite.Initialize(context.Background(), filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	jobs := adaptersqlite.NewExternalAgentJobStore(store)
	job := domain.ExternalAgentJob{ID: "job_fg", Mode: domain.JobForeground, Provider: "opencode", Profile: "build", PrimaryProject: "workspace", RegistryRevision: "rev-1", Task: "task", Actor: "U12345678", TeamID: "T12345678", ConversationKey: "slack:T12345678:dm:D12345678", Status: domain.JobQueued, TimeoutAt: time.Now().Add(time.Minute), CreatedAt: time.Now(), UpdatedAt: time.Now(), OriginalCallID: "fg-call", RequestSHA256: "request"}
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
	if stored.ResultSummary != final || stored.ResultSHA256 != result.ResultSHA256 || stored.ResultBytes != result.ResultBytes {
		t.Fatalf("persisted foreground identity = %q sha %q bytes %d, want %q sha %q bytes %d",
			stored.ResultSummary, stored.ResultSHA256, stored.ResultBytes, final, result.ResultSHA256, result.ResultBytes)
	}
	if strings.Contains(stored.ResultSummary, secret) {
		t.Fatal("secret survived foreground SQLite persistence")
	}
	notification, err := jobs.ClaimNextNotification(t.Context(), time.Now().Add(time.Second), "publisher", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if notification == nil || notification.ResultSHA256 != result.ResultSHA256 || notification.ResultBytes != result.ResultBytes ||
		strings.Contains(notification.CanonicalMarkdown, secret) {
		t.Fatalf("notification leaked secret or identity: %#v", notification)
	}
}

func TestDurableACPDispatcherForegroundRecoveryNormalizesWithoutDetachedMetadata(t *testing.T) {
	secret := "xoxb-recovery-secret-value-7"
	workspace := t.TempDir()
	artifacts := &recordingResultArtifacts{}
	redact := func(value string) string { return strings.ReplaceAll(value, secret, "[REDACTED]") }
	child := preparedAgentTool{
		definition:   agentdef.AgentDef{Name: "worker", Runtime: "opencode/build", ExecutionMode: agentdef.ExecutionModeDurableJob},
		acpRuntime:   &fakeExternalRuntime{result: domain.AcpInvocationResult{Text: "raw=" + secret + " <safe>"}},
		acpResolved:  &agentdef.ResolvedModel{Provider: agentdef.Provider{Name: "opencode", Type: agentdef.ProviderTypeACP}},
		projectRoots: map[string]string{"workspace": workspace}, registryRevision: "rev-1",
	}
	dispatcher := &acpJobDispatcher{children: []preparedAgentTool{child}, artifacts: artifacts, sanitize: redact, policy: domain.ResultDeliveryPolicy{
		MaxMarkdownParts: 6, MaxFileBytes: 1024 * 1024, MaxInlineResultBytes: 64 * 1024, MaxResultArtifactBytes: 1024 * 1024,
	}}
	result, err := dispatcher.Reconcile(context.Background(), domain.ExternalAgentJob{
		ID: "job_rec", Mode: domain.JobForeground, Provider: "opencode", Profile: "opencode/build",
		PrimaryProject: "workspace", RegistryRevision: "rev-1", Task: "task", ACPSessionID: "session-1", TimeoutAt: time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	final := "raw=[REDACTED] &lt;safe>"
	digest := sha256.Sum256([]byte(final))
	if result.Text != final || result.ResultBytes != int64(len([]byte(final))) || result.ResultSHA256 != fmt.Sprintf("%x", digest) {
		t.Fatalf("recovered foreground result = %+v, want %q with exact identity", result, final)
	}
	if result.DeliveryMode != "" || result.DeliveryCanonicalMarkdown != "" || result.DeliveryPolicyVersion != "" || result.Inline || result.ArtifactRef != "" || len(artifacts.contents) != 0 {
		t.Fatalf("foreground recovery produced detached delivery metadata: result=%+v artifacts=%#v", result, artifacts.contents)
	}
}

func TestDurableACPDispatcherForegroundNormalizationFailsClosed(t *testing.T) {
	workspace := t.TempDir()
	newDispatcher := func(raw string, redact func(string) string, inlineBytes int64) *acpJobDispatcher {
		child := preparedAgentTool{
			definition:   agentdef.AgentDef{Name: "worker", Runtime: "opencode/build", ExecutionMode: agentdef.ExecutionModeDurableJob},
			acpRuntime:   &fakeExternalRuntime{result: domain.AcpInvocationResult{Text: raw}},
			acpResolved:  &agentdef.ResolvedModel{Provider: agentdef.Provider{Name: "opencode", Type: agentdef.ProviderTypeACP}},
			projectRoots: map[string]string{"workspace": workspace}, registryRevision: "rev-1",
		}
		policy := domain.ResultDeliveryPolicy{
			MaxMarkdownParts: 6, MaxFileBytes: 1024 * 1024, MaxInlineResultBytes: inlineBytes, MaxResultArtifactBytes: 1024 * 1024,
		}
		return &acpJobDispatcher{children: []preparedAgentTool{child}, sanitize: redact, policy: policy}
	}
	runForeground := func(dispatcher *acpJobDispatcher) error {
		_, err := dispatcher.Run(context.Background(), domain.ExternalAgentJob{
			ID: "job_fg", Mode: domain.JobForeground, Provider: "opencode", Profile: "opencode/build",
			PrimaryProject: "workspace", RegistryRevision: "rev-1", Task: "task", TimeoutAt: time.Now().Add(time.Minute),
		})
		return err
	}
	t.Run("empty after sanitization", func(t *testing.T) {
		err := runForeground(newDispatcher("control text", func(string) string { return "" }, 64*1024))
		var acpErr *domain.ACPError
		if !errors.As(err, &acpErr) || acpErr.Code != domain.ACPErrorResultArtifactInvalid {
			t.Fatalf("err = %v, want typed %s", err, domain.ACPErrorResultArtifactInvalid)
		}
		if strings.Contains(err.Error(), "control text") {
			t.Fatal("error leaked result content")
		}
	})
	t.Run("exceeds max result bytes", func(t *testing.T) {
		err := runForeground(newDispatcher(strings.Repeat("x", 65), func(value string) string { return value }, 64))
		var acpErr *domain.ACPError
		if !errors.As(err, &acpErr) || acpErr.Code != domain.ACPErrorResultTooLarge {
			t.Fatalf("err = %v, want typed %s", err, domain.ACPErrorResultTooLarge)
		}
	})
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
