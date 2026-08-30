package domain_test

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

func TestCompletionPolicyControlsRootActivationDisposition(t *testing.T) {
	base := domain.ExternalAgentJob{
		ID: "job-v45", Mode: domain.JobDetached, Provider: "provider", Profile: "profile",
		PrimaryProject: "project", Task: "task", Status: domain.JobCompleted,
		ConversationKey: "slack:T12345678:dm:D12345678", Actor: "U12345678", TeamID: "T12345678",
		StatusRevision: 1,
	}
	for _, test := range []struct {
		name, policy string
		workstream   bool
		wantMarker   bool
	}{
		{"automatic root without binding", string(domain.ExternalAgentCompletionAutomaticRoot), false, true},
		{"automatic root with binding", string(domain.ExternalAgentCompletionAutomaticRoot), true, true},
		{"workstream only with binding", string(domain.ExternalAgentCompletionWorkstream), true, true},
		{"workstream only without binding", string(domain.ExternalAgentCompletionWorkstream), false, false},
		{"delivery only", string(domain.ExternalAgentCompletionDeliveryOnly), true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			job := base
			job.CompletionPolicy = domain.ExternalAgentCompletionPolicy(test.policy)
			if test.workstream {
				job.WorkstreamID, job.TaskID, job.ExecutionIdentity, job.AdmissionRevision = "ws", "task", "exec", 2
			}
			notification, err := domain.NewExternalAgentJobNotification(job)
			if err != nil {
				t.Fatal(err)
			}
			if notification.RootActivationRequired != test.wantMarker {
				t.Fatalf("root activation required = %t, want %t", notification.RootActivationRequired, test.wantMarker)
			}
		})
	}
}

func TestBuildDelegatedTaskExcerptUsesCompleteTaskDigest(t *testing.T) {
	task := strings.Repeat("界", domain.MaxDelegatedTaskExcerptRunes+100)
	excerpt, digest, truncated, err := domain.BuildDelegatedTaskExcerpt(task)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated || !strings.Contains(excerpt, domain.ActivationTaskExcerptTruncation) || len([]rune(excerpt)) > domain.MaxDelegatedTaskExcerptRunes {
		t.Fatalf("excerpt length=%d truncated=%t excerpt suffix=%q", len([]rune(excerpt)), truncated, excerpt[len(excerpt)-len(domain.ActivationTaskExcerptTruncation):])
	}
	want := fmt.Sprintf("%x", sha256.Sum256([]byte(task)))
	if digest != want {
		t.Fatalf("task digest = %q, want %q", digest, want)
	}
}

func TestActivationScopeValidationRequiresBindingOnlyForWorkstream(t *testing.T) {
	base := validActivationForV45()
	base.WorkstreamID, base.TaskID, base.ExecutionIdentity, base.AdmissionRevision = "ws", "task", "exec", 2
	base.ActivationScope = domain.ExternalAgentActivationConversation
	if err := base.Validate(); err != nil {
		t.Fatalf("conversation activation with provenance tuple validation failed: %v", err)
	}
	base.ActivationScope = domain.ExternalAgentActivationWorkstream
	base.WorkstreamID, base.TaskID, base.ExecutionIdentity, base.AdmissionRevision = "", "", "", 0
	if err := base.Validate(); err == nil {
		t.Fatal("workstream activation without binding was accepted")
	}
}

func validActivationForV45() domain.ExternalAgentJobActivation {
	return domain.ExternalAgentJobActivation{
		ActivationID: "activation-v45", JobID: "job-v45", ActivationScope: domain.ExternalAgentActivationConversation,
		StatusRevision: 1, Kind: domain.JobNotificationTerminal, TerminalStatus: domain.JobCompleted,
		NotificationSHA256: strings.Repeat("a", 64), ResultSHA256: strings.Repeat("b", 64),
		Actor: "U12345678", TeamID: "T12345678", ConversationKey: "slack:T12345678:dm:D12345678",
		OriginalCallID: "call", DeliveryMode: domain.JobResultDeliveryMarkdown, ContentBytes: 1,
		SlackMessageTS: "1710000000.000001", PublishedAt: unixTimeV45(), State: domain.ActivationPending,
	}
}

func unixTimeV45() (value time.Time) { return time.Unix(1710000000, 0).UTC() }
