package domain_test

import (
	"crypto/sha256"
	"fmt"
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
		RegistryRevision: "rev-1",
		Task:             "create a document", Mode: domain.JobDetached,
		PermissionOptionKind: "allow_once",
		Timeout:              2 * time.Hour,
		PrimaryPath:          "/private/one",
	}
	left := domain.ExternalAgentJobRequestDigest(request)
	request.PrimaryPath = "/another/private/path"
	request.PrimaryPath = "/different"
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
	notification, err := domain.NewExternalAgentJobDelivery(job, domain.ExternalAgentInvocationResult{
		Text: resultText, DeliveryMode: domain.JobResultDeliveryMarkdown, DeliveryCanonicalMarkdown: "OpenCode job `job_1` completed.\n\n" + resultText,
		DeliveryPolicyVersion: domain.JobDeliveryPolicyV1, DeliveryMaxMarkdownParts: 6, DeliveryContentBytes: int64(len([]byte(resultText))),
	})
	if err != nil {
		t.Fatal(err)
	}
	if notification.DeliveryMode != domain.JobResultDeliveryMarkdown || !strings.HasSuffix(notification.CanonicalMarkdown, resultText) || strings.Contains(notification.CanonicalMarkdown, "…") {
		t.Fatalf("notification = %+v", notification)
	}

	file, err := domain.NewExternalAgentJobDelivery(job, domain.ExternalAgentInvocationResult{
		Text: "", DeliveryMode: domain.JobResultDeliveryFile, DeliveryArtifactRef: "job_1-delivery.result", DeliveryContentSHA256: strings.Repeat("a", 64), DeliveryContentBytes: 20000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if file.DeliveryMode != domain.JobResultDeliveryFile || file.ArtifactRef == "" || file.ContentBytes != 20000 || strings.Contains(file.CanonicalMarkdown, resultText) {
		t.Fatalf("file notification = %+v", file)
	}
}

func TestExternalAgentJobDeliverySeparatesNotificationAndResultIdentity(t *testing.T) {
	job := domain.ExternalAgentJob{
		ID:                "job_1",
		Mode:              domain.JobDetached,
		Status:            domain.JobCompleted,
		StatusRevision:    2,
		ConversationKey:   "slack:T12345678:dm:D12345678",
		WorkstreamID:      "ws-1",
		TaskID:            "task-1",
		ExecutionIdentity: "exec-1",
	}
	resultText := "safe &lt;result>"
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(resultText)))
	notification, err := domain.NewExternalAgentJobDelivery(job, domain.ExternalAgentInvocationResult{
		Text: resultText, ResultSHA256: digest, ResultBytes: int64(len([]byte(resultText))),
		DeliveryMode: domain.JobResultDeliveryMarkdown, DeliveryPolicyVersion: domain.JobDeliveryPolicyV1,
		DeliveryContentSHA256: digest, DeliveryContentBytes: int64(len([]byte(resultText))),
		DeliveryCanonicalMarkdown: "OpenCode job `job_1` completed.\n\n" + resultText, DeliveryMaxMarkdownParts: 6,
	})
	if err != nil {
		t.Fatal(err)
	}
	expectedNotification := domain.NotificationIdentitySHA256(notification.CanonicalMarkdown)
	if notification.NotificationSHA256 != expectedNotification || notification.NotificationBytes != int64(len([]byte(notification.CanonicalMarkdown))) {
		t.Fatalf("notification identity = %q/%d, want %q/%d", notification.NotificationSHA256, notification.NotificationBytes, expectedNotification, len([]byte(notification.CanonicalMarkdown)))
	}
	if notification.NotificationSHA256 == notification.ResultSHA256 {
		t.Fatal("notification and result identity collide")
	}
	if notification.CanonicalMarkdown != "OpenCode job `job_1` completed. Root integration is pending." {
		t.Fatalf("activation notification repeated result prose: %q", notification.CanonicalMarkdown)
	}
	if notification.ResultSHA256 != digest || notification.ResultBytes != int64(len([]byte(resultText))) {
		t.Fatalf("result identity = %q/%d, want %q/%d", notification.ResultSHA256, notification.ResultBytes, digest, len([]byte(resultText)))
	}
	if !notification.RootActivationRequired {
		t.Fatal("detached delivery is not marked activation-required")
	}

	foreground := job
	foreground.Mode = domain.JobForeground
	foregroundNotification, err := domain.NewExternalAgentJobDelivery(foreground, domain.ExternalAgentInvocationResult{
		Text: resultText, ResultSHA256: digest, ResultBytes: int64(len([]byte(resultText))),
		DeliveryMode: domain.JobResultDeliveryMarkdown, DeliveryPolicyVersion: domain.JobDeliveryPolicyV1,
		DeliveryContentSHA256: digest, DeliveryContentBytes: int64(len([]byte(resultText))),
		DeliveryCanonicalMarkdown: "OpenCode job `job_1` completed.\n\n" + resultText, DeliveryMaxMarkdownParts: 6,
	})
	if err != nil {
		t.Fatal(err)
	}
	if foregroundNotification.RootActivationRequired {
		t.Fatal("foreground delivery is marked activation-required")
	}
}

func TestDetachedActivationLegacyNotificationUsesTerminalMarker(t *testing.T) {
	job := domain.ExternalAgentJob{
		ID: "job_1", Mode: domain.JobDetached, Status: domain.JobCompleted, StatusRevision: 2,
		ResultSummary: "full result must not be repeated", ConversationKey: "slack:T12345678:dm:D12345678",
		WorkstreamID: "ws-1", TaskID: "task-1", ExecutionIdentity: "exec-1",
	}
	notification, err := domain.NewExternalAgentJobNotification(job)
	if err != nil {
		t.Fatal(err)
	}
	if !notification.RootActivationRequired || notification.CanonicalMarkdown != "OpenCode job `job_1` completed. Root integration is pending." {
		t.Fatalf("activation notification = %+v", notification)
	}
}

func TestLegacyNotificationConstructorSetsRouteAndIdentityByMode(t *testing.T) {
	now := time.Now().UTC()
	base := domain.ExternalAgentJob{
		ID:                "job_1",
		Status:            domain.JobFailed,
		StatusRevision:    1,
		ConversationKey:   "slack:T12345678:dm:D12345678",
		WorkstreamID:      "ws-1",
		TaskID:            "task-1",
		ExecutionIdentity: "exec-1",
		ErrorCode:         "acp_process_exit",
		CreatedAt:         now,
		UpdatedAt:         now,
	}

	foreground := base
	foreground.Mode = domain.JobForeground
	foregroundNotification, err := domain.NewExternalAgentJobNotification(foreground)
	if err != nil {
		t.Fatal(err)
	}
	if foregroundNotification.RootActivationRequired {
		t.Fatal("foreground notification is marked activation-required")
	}
	if foregroundNotification.NotificationSHA256 != domain.NotificationIdentitySHA256(foregroundNotification.CanonicalMarkdown) ||
		foregroundNotification.NotificationBytes != int64(len([]byte(foregroundNotification.CanonicalMarkdown))) {
		t.Fatalf("foreground notification identity = %q/%d", foregroundNotification.NotificationSHA256, foregroundNotification.NotificationBytes)
	}
	if foregroundNotification.ResultSHA256 != "" || foregroundNotification.ResultBytes != 0 {
		t.Fatalf("failure notification carries a result identity: %q/%d", foregroundNotification.ResultSHA256, foregroundNotification.ResultBytes)
	}

	detached := base
	detached.Mode = domain.JobDetached
	detachedNotification, err := domain.NewExternalAgentJobNotification(detached)
	if err != nil {
		t.Fatal(err)
	}
	if detachedNotification.RootActivationRequired {
		t.Fatal("failed detached notification is marked activation-required")
	}

	completed := base
	completed.Mode = domain.JobForeground
	completed.Status = domain.JobCompleted
	completed.ResultSummary = "summary"
	completed.ResultSHA256 = fmt.Sprintf("%x", sha256.Sum256([]byte("summary")))
	completed.ResultBytes = int64(len([]byte("summary")))
	completedNotification, err := domain.NewExternalAgentJobNotification(completed)
	if err != nil {
		t.Fatal(err)
	}
	if completedNotification.ResultSHA256 != completed.ResultSHA256 || completedNotification.ResultBytes != completed.ResultBytes {
		t.Fatalf("completed result identity = %q/%d, want %q/%d", completedNotification.ResultSHA256, completedNotification.ResultBytes, completed.ResultSHA256, completed.ResultBytes)
	}
	completed.Mode = domain.JobDetached
	completedNotification, err = domain.NewExternalAgentJobNotification(completed)
	if err != nil {
		t.Fatal(err)
	}
	if !completedNotification.RootActivationRequired {
		t.Fatal("completed detached notification is not marked activation-required")
	}
}

func TestValidResultIdentityFailsClosedOnIncompleteInput(t *testing.T) {
	valid := fmt.Sprintf("%x", sha256.Sum256([]byte("safe")))
	digest, bytes := domain.ValidResultIdentity(valid, 4)
	if digest != valid || bytes != 4 {
		t.Fatalf("valid identity = %q/%d", digest, bytes)
	}
	for _, candidate := range []struct {
		digest string
		bytes  int64
	}{
		{"", 4},
		{valid, 0},
		{valid, -1},
		{strings.Repeat("a", 63), 4},
		{strings.Repeat("g", 64), 4},
		{" " + valid, 4},
	} {
		digest, bytes := domain.ValidResultIdentity(candidate.digest, candidate.bytes)
		if digest != "" || bytes != 0 {
			t.Fatalf("invalid identity %q/%d = %q/%d", candidate.digest, candidate.bytes, digest, bytes)
		}
	}
}

func TestValidInlineResultFailsClosedOnIncoherentShape(t *testing.T) {
	summary := "safe result"
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(summary)))
	bytes := int64(len([]byte(summary)))
	if !domain.ValidInlineResult(summary, digest, bytes) {
		t.Fatal("coherent inline result was rejected")
	}
	if !domain.ValidInlineResult(summary, strings.ToUpper(digest), bytes) {
		t.Fatal("upper-case digest was rejected")
	}
	for _, candidate := range []struct {
		name    string
		summary string
		digest  string
		bytes   int64
	}{
		{"empty summary", "", digest, bytes},
		{"invalid UTF-8 summary", string([]byte{0xff, 0xfe}), digest, bytes},
		{"empty digest", summary, "", bytes},
		{"malformed digest", summary, "digest", bytes},
		{"short digest", summary, strings.Repeat("a", 63), bytes},
		{"non-hex digest", summary, strings.Repeat("g", 64), bytes},
		{"wrong digest value", summary, fmt.Sprintf("%x", sha256.Sum256([]byte("other"))), bytes},
		{"zero bytes", summary, digest, 0},
		{"negative bytes", summary, digest, -1},
		{"mismatched byte count", summary, digest, bytes + 1},
	} {
		if domain.ValidInlineResult(candidate.summary, candidate.digest, candidate.bytes) {
			t.Fatalf("incoherent inline result was accepted: %s", candidate.name)
		}
	}
}

func TestValidArtifactResultFailsClosedOnIncoherentShape(t *testing.T) {
	digest := strings.Repeat("a", 64)
	if !domain.ValidArtifactResult("job_1-delivery.result", digest, 1024) {
		t.Fatal("coherent artifact result was rejected")
	}
	for _, candidate := range []struct {
		name string
		ref  string
	}{
		{"empty reference", ""},
		{"path-like reference", "dir/job_1-delivery.result"},
		{"backslash reference", "dir\\job_1-delivery.result"},
		{"newline reference", "job_1-delivery.result\n"},
	} {
		if domain.ValidArtifactResult(candidate.ref, digest, 1024) {
			t.Fatalf("incoherent artifact reference was accepted: %s", candidate.name)
		}
	}
	for _, candidate := range []struct {
		name   string
		digest string
		bytes  int64
	}{
		{"empty digest", "", 1024},
		{"malformed digest", "digest", 1024},
		{"non-hex digest", strings.Repeat("g", 64), 1024},
		{"zero bytes", digest, 0},
		{"negative bytes", digest, -1},
	} {
		if domain.ValidArtifactResult("job_1-delivery.result", candidate.digest, candidate.bytes) {
			t.Fatalf("incoherent artifact identity was accepted: %s", candidate.name)
		}
	}
}

func TestValidArtifactResultForJobBindsReferenceToJob(t *testing.T) {
	digest := strings.Repeat("a", 64)
	if got := domain.CanonicalArtifactReference("job_1"); got != "job_1-delivery.result" {
		t.Fatalf("canonical reference = %q, want %q", got, "job_1-delivery.result")
	}
	tests := []struct {
		name string
		ref  string
		want bool
	}{
		{name: "canonical reference of this job", ref: "job_1-delivery.result", want: true},
		{name: "foreign job reference", ref: "job_2-delivery.result", want: false},
		{name: "safe but non-canonical reference", ref: "job_1.result", want: false},
		{name: "safe but non-canonical suffix reference", ref: "job_1-delivery.result.txt", want: false},
		{name: "path-like reference", ref: "dir/job_1-delivery.result", want: false},
		{name: "empty reference", ref: "", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := domain.ValidArtifactResultForJob("job_1", tt.ref, digest, 1024); got != tt.want {
				t.Fatalf("ValidArtifactResultForJob = %v, want %v", got, tt.want)
			}
		})
	}
	if domain.ValidArtifactResultForJob("job_1", "job_1-delivery.result", "digest", 1024) {
		t.Fatal("incoherent identity on the canonical reference was accepted")
	}
}

func TestStatusViewPromisesResultOnlyForStrictIdentity(t *testing.T) {
	summary := "safe result"
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(summary)))
	bytes := int64(len([]byte(summary)))
	artifactDigest := strings.Repeat("a", 64)
	tests := []struct {
		name         string
		job          domain.ExternalAgentJob
		want         bool
		wantMode     domain.JobResultDeliveryMode
		wantIdentity bool
	}{
		{
			name: "completed inline with complete identity",
			job:  domain.ExternalAgentJob{ID: "job_1", Status: domain.JobCompleted, StatusRevision: 4, ResultSummary: summary, ResultSHA256: digest, ResultBytes: bytes},
			want: true, wantMode: domain.JobResultDeliveryMarkdown, wantIdentity: true,
		},
		{
			name: "completed file mode with complete identity",
			job:  domain.ExternalAgentJob{ID: "job_1", Status: domain.JobCompleted, StatusRevision: 4, ResultArtifact: "job_1-delivery.result", ResultSHA256: artifactDigest, ResultBytes: 1024},
			want: true, wantMode: domain.JobResultDeliveryFile, wantIdentity: true,
		},
		{
			name: "completed with empty summary",
			job:  domain.ExternalAgentJob{ID: "job_1", Status: domain.JobCompleted, StatusRevision: 4, ResultSHA256: digest, ResultBytes: bytes},
			want: false, wantMode: "",
		},
		{
			name: "completed with summary but empty SHA",
			job:  domain.ExternalAgentJob{ID: "job_1", Status: domain.JobCompleted, StatusRevision: 4, ResultSummary: summary, ResultBytes: bytes},
			want: false, wantMode: domain.JobResultDeliveryMarkdown,
		},
		{
			name: "completed with wrong SHA value",
			job: domain.ExternalAgentJob{
				ID:             "job_1",
				Status:         domain.JobCompleted,
				StatusRevision: 4,
				ResultSummary:  summary,
				ResultSHA256:   fmt.Sprintf("%x", sha256.Sum256([]byte("other"))),
				ResultBytes:    bytes,
			},
			want:     false,
			wantMode: domain.JobResultDeliveryMarkdown,
		},
		{
			name: "completed with zero bytes",
			job:  domain.ExternalAgentJob{ID: "job_1", Status: domain.JobCompleted, StatusRevision: 4, ResultSummary: summary, ResultSHA256: digest, ResultBytes: 0},
			want: false, wantMode: domain.JobResultDeliveryMarkdown,
		},
		{
			name: "completed with mismatched byte count",
			job:  domain.ExternalAgentJob{ID: "job_1", Status: domain.JobCompleted, StatusRevision: 4, ResultSummary: summary, ResultSHA256: digest, ResultBytes: bytes + 1},
			want: false, wantMode: domain.JobResultDeliveryMarkdown,
		},
		{
			name: "completed artifact with path-like reference",
			job:  domain.ExternalAgentJob{ID: "job_1", Status: domain.JobCompleted, StatusRevision: 4, ResultArtifact: "dir/job_1-delivery.result", ResultSHA256: artifactDigest, ResultBytes: 1024},
			want: false, wantMode: domain.JobResultDeliveryFile,
		},
		{
			name: "completed artifact with foreign job reference",
			job:  domain.ExternalAgentJob{ID: "job_1", Status: domain.JobCompleted, StatusRevision: 4, ResultArtifact: "job_2-delivery.result", ResultSHA256: artifactDigest, ResultBytes: 1024},
			want: false, wantMode: domain.JobResultDeliveryFile,
		},
		{
			name: "completed artifact with safe non-canonical reference",
			job:  domain.ExternalAgentJob{ID: "job_1", Status: domain.JobCompleted, StatusRevision: 4, ResultArtifact: "job_1.result", ResultSHA256: artifactDigest, ResultBytes: 1024},
			want: false, wantMode: domain.JobResultDeliveryFile,
		},
		{
			name: "incoherent artifact does not fall back to inline",
			job: domain.ExternalAgentJob{
				ID:             "job_1",
				Status:         domain.JobCompleted,
				StatusRevision: 4,
				ResultSummary:  summary,
				ResultSHA256:   digest,
				ResultBytes:    bytes,
				ResultArtifact: "bad/ref.result",
			},
			want:     false,
			wantMode: domain.JobResultDeliveryFile,
		},
		{
			name: "non-completed status with complete identity",
			job:  domain.ExternalAgentJob{ID: "job_1", Status: domain.JobFailed, StatusRevision: 4, ResultSummary: summary, ResultSHA256: digest, ResultBytes: bytes},
			want: false, wantMode: domain.JobResultDeliveryMarkdown,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			view := tt.job.StatusView()
			if view.ResultAvailable != tt.want {
				t.Fatalf("ResultAvailable = %v, want %v", view.ResultAvailable, tt.want)
			}
			if view.DeliveryMode != tt.wantMode {
				t.Fatalf("DeliveryMode = %q, want %q", view.DeliveryMode, tt.wantMode)
			}
			if tt.wantIdentity {
				if view.ResultSHA256 == "" || view.ResultBytes <= 0 {
					t.Fatalf("complete identity was not projected: %q/%d", view.ResultSHA256, view.ResultBytes)
				}
			}
		})
	}
}

func TestNotificationIdentitySHA256FailsClosedOnInvalidMarkdown(t *testing.T) {
	if digest := domain.NotificationIdentitySHA256(""); digest != "" {
		t.Fatalf("empty Markdown identity = %q", digest)
	}
	if digest := domain.NotificationIdentitySHA256("   \n"); digest != "" {
		t.Fatalf("blank Markdown identity = %q", digest)
	}
	if digest := domain.NotificationIdentitySHA256(string([]byte{0xff, 0xfe})); digest != "" {
		t.Fatalf("invalid UTF-8 Markdown identity = %q", digest)
	}
	expected := fmt.Sprintf("%x", sha256.Sum256([]byte("safe")))
	if digest := domain.NotificationIdentitySHA256("safe"); digest != expected {
		t.Fatalf("Markdown identity = %q, want %q", digest, expected)
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
