package domain_test

import (
	"strings"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

func TestExternalAgentJobAllowsOnlyDeclaredTransitions(t *testing.T) {
	job := domain.ExternalAgentJob{ID: "job_1", Status: domain.JobQueued}
	for _, next := range []domain.ExternalAgentJobStatus{domain.JobRunning, domain.JobCancelled} {
		if err := job.Transition(next); err != nil {
			t.Fatalf("queued -> %s: %v", next, err)
		}
		job.Status = domain.JobQueued
	}
	if err := job.Transition(domain.JobCompleted); err == nil {
		t.Fatal("queued -> completed was accepted")
	}
}

func TestExternalAgentJobRequestDigestExcludesHostPaths(t *testing.T) {
	request := domain.ExternalAgentJobRequest{
		Provider: "opencode", Profile: "build", PrimaryProject: "workspace",
		AdditionalProjects: []string{"docs"}, RegistryRevision: "rev-1",
		Task: "create a document", Mode: domain.JobDetached,
		PermissionOptionKind: domain.ACPPermissionAllowOnce,
		Timeout:              2 * time.Hour,
		PrimaryPath:          "/private/one", AdditionalPaths: []string{"/private/two"},
	}
	left := domain.ExternalAgentJobRequestDigest(request)
	request.PrimaryPath = "/another/private/path"
	request.AdditionalPaths = []string{"/different"}
	right := domain.ExternalAgentJobRequestDigest(request)
	if left == "" || left != right || len(left) != 64 || strings.Contains(left, "private") {
		t.Fatalf("digest = %q / %q", left, right)
	}
}

func TestConversationReplyTargetAcceptsThreadedDM(t *testing.T) {
	target, err := domain.ConversationReplyTarget("slack:T12345678:dm:D12345678:thread:1700000000.000001")
	if err != nil {
		t.Fatal(err)
	}
	if target.ChannelID != "D12345678" || target.ThreadTS != "1700000000.000001" {
		t.Fatalf("target = %#v", target)
	}
}

func TestExternalAgentJobNotificationUsesCanonicalBoundForSummary(t *testing.T) {
	job := domain.ExternalAgentJob{
		ID: "job_1", Status: domain.JobCompleted, StatusRevision: 1,
		ResultSummary:   strings.Repeat("x", 3000),
		ConversationKey: "slack:T12345678:dm:D12345678",
	}
	notification, err := domain.NewExternalAgentJobNotification(job)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(notification.CanonicalMarkdown, job.ResultSummary) {
		t.Fatal("notification truncated a summary below the canonical Markdown bound")
	}

	job.ResultSummary = strings.Repeat("x", 9000)
	notification, err = domain.NewExternalAgentJobNotification(job)
	if err != nil {
		t.Fatal(err)
	}
	if got := len([]rune(notification.CanonicalMarkdown)); got != 8000 || !strings.HasSuffix(notification.CanonicalMarkdown, "…") {
		t.Fatalf("bounded notification length = %d, suffix = %q", got, notification.CanonicalMarkdown[len(notification.CanonicalMarkdown)-3:])
	}
}

func TestExternalAgentJobDeliveryKeepsCompleteMarkdownAndFileIdentity(t *testing.T) {
	job := domain.ExternalAgentJob{ID: "job_1", Status: domain.JobCompleted, StatusRevision: 2, ConversationKey: "slack:T12345678:dm:D12345678"}
	resultText := strings.Repeat("result-", 4000)
	notification, err := domain.NewExternalAgentJobDelivery(job, domain.AcpInvocationResult{
		Text: resultText, DeliveryMode: domain.JobResultDeliveryMarkdown, DeliveryCanonicalMarkdown: "OpenCode job `job_1` completed.\n\n" + resultText,
		DeliveryPolicyVersion: domain.JobDeliveryPolicyV1, DeliveryMaxMarkdownParts: 6, DeliveryContentBytes: int64(len([]byte(resultText))),
	})
	if err != nil {
		t.Fatal(err)
	}
	if notification.DeliveryMode != domain.JobResultDeliveryMarkdown || !strings.HasSuffix(notification.CanonicalMarkdown, resultText) || strings.Contains(notification.CanonicalMarkdown, "…") {
		t.Fatalf("notification = %+v", notification)
	}

	file, err := domain.NewExternalAgentJobDelivery(job, domain.AcpInvocationResult{
		Text: "", DeliveryMode: domain.JobResultDeliveryFile, DeliveryArtifactRef: "job_1-delivery.result", DeliveryContentSHA256: strings.Repeat("a", 64), DeliveryContentBytes: 20000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if file.DeliveryMode != domain.JobResultDeliveryFile || file.ArtifactRef == "" || file.ContentBytes != 20000 || strings.Contains(file.CanonicalMarkdown, resultText) {
		t.Fatalf("file notification = %+v", file)
	}
}

func TestResultDeliveryPolicyAcceptsConfiguredPartBoundsOnly(t *testing.T) {
	for parts := 1; parts <= 8; parts++ {
		policy := domain.ResultDeliveryPolicy{
			MaxMarkdownParts: parts, MaxFileBytes: 1024, MaxInlineResultBytes: int64(parts * domain.SlackMarkdownChunkRunes), MaxResultArtifactBytes: 1024,
		}
		if err := policy.Validate(); err != nil {
			t.Fatalf("parts=%d policy rejected at upper bound: %v", parts, err)
		}
	}
	for _, policy := range []domain.ResultDeliveryPolicy{
		{MaxMarkdownParts: 0, MaxFileBytes: 1, MaxInlineResultBytes: 1, MaxResultArtifactBytes: 1},
		{MaxMarkdownParts: 9, MaxFileBytes: 1, MaxInlineResultBytes: 1, MaxResultArtifactBytes: 1},
		{MaxMarkdownParts: 1, MaxFileBytes: 0, MaxInlineResultBytes: 1, MaxResultArtifactBytes: 1},
		{MaxMarkdownParts: 1, MaxFileBytes: 1025, MaxInlineResultBytes: 1, MaxResultArtifactBytes: 1024},
		{MaxMarkdownParts: 1, MaxFileBytes: 1, MaxInlineResultBytes: 0, MaxResultArtifactBytes: 1},
		{MaxMarkdownParts: 1, MaxFileBytes: 1, MaxInlineResultBytes: domain.SlackMarkdownChunkRunes + 1, MaxResultArtifactBytes: 1},
	} {
		if err := policy.Validate(); err == nil {
			t.Fatalf("invalid policy was accepted: %+v", policy)
		}
	}
}

func TestMessageSourceMatchesTechnicalRole(t *testing.T) {
	valid := []domain.Message{
		{Role: domain.RoleUser, Source: domain.MessageSourceHuman},
		{Role: domain.RoleUser, Source: domain.MessageSourceJobCompletion},
		{Role: domain.RoleAssistant, Source: domain.MessageSourceAssistant},
	}
	for _, message := range valid {
		if err := message.Validate(); err != nil {
			t.Fatalf("valid message rejected: %+v: %v", message, err)
		}
	}
	for _, message := range []domain.Message{
		{Role: domain.RoleAssistant, Source: domain.MessageSourceHuman},
		{Role: domain.RoleAssistant, Source: domain.MessageSourceJobCompletion},
		{Role: domain.RoleUser, Source: domain.MessageSourceAssistant},
	} {
		if err := message.Validate(); err == nil {
			t.Fatalf("invalid message accepted: %+v", message)
		}
	}
}

func TestExternalAgentJobActivationTransitionsDoNotReplayModel(t *testing.T) {
	activation := domain.ExternalAgentJobActivation{State: domain.ActivationPending}
	for _, next := range []domain.ExternalAgentJobActivationState{
		domain.ActivationProcessing,
		domain.ActivationModelStarted,
		domain.ActivationResponsePrepared,
		domain.ActivationCompleted,
	} {
		if err := activation.Transition(next); err != nil {
			t.Fatalf("transition to %s: %v", next, err)
		}
	}
	if err := activation.Transition(domain.ActivationProcessing); err == nil {
		t.Fatal("completed activation was replayable")
	}
	activation.State = domain.ActivationModelStarted
	if err := activation.Transition(domain.ActivationCompletionUnknown); err != nil {
		t.Fatal(err)
	}
	if err := activation.Transition(domain.ActivationProcessing); err == nil {
		t.Fatal("completion_unknown activation was replayable")
	}
}

func TestExternalAgentJobActivationIDIsStablePerTerminalIdentity(t *testing.T) {
	left := domain.ExternalAgentJobActivationID("job_1", 4, domain.JobNotificationTerminal)
	right := domain.ExternalAgentJobActivationID("job_1", 4, domain.JobNotificationTerminal)
	if left == "" || left != right {
		t.Fatalf("activation ID is not stable: %q / %q", left, right)
	}
	if left == domain.ExternalAgentJobActivationID("job_1", 5, domain.JobNotificationTerminal) {
		t.Fatal("different terminal revision reused activation ID")
	}
}
