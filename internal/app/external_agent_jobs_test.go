package app

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/agentdef"
	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
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
