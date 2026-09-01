package port

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
)

func TestAgentTurnOriginValidation(t *testing.T) {
	cases := []struct {
		name   string
		origin AgentTurnOrigin
		valid  bool
	}{
		{name: "user", origin: AgentTurnOrigin{Kind: AgentTurnOriginUser}, valid: true},
		{
			name:   "job completion",
			origin: AgentTurnOrigin{Kind: AgentTurnOriginJobCompletion, Actor: "U12345678", ActivationID: "activation-1", ActivationScope: domain.ExternalAgentActivationConversation},
			valid:  true,
		},
		{name: "user activation metadata", origin: AgentTurnOrigin{Kind: AgentTurnOriginUser, ActivationID: "activation-1"}},
		{name: "job missing actor", origin: AgentTurnOrigin{Kind: AgentTurnOriginJobCompletion, ActivationID: "activation-1", ActivationScope: domain.ExternalAgentActivationConversation}},
		{name: "job missing activation", origin: AgentTurnOrigin{Kind: AgentTurnOriginJobCompletion, Actor: "U12345678", ActivationScope: domain.ExternalAgentActivationConversation}},
		{
			name:   "job legacy scope",
			origin: AgentTurnOrigin{Kind: AgentTurnOriginJobCompletion, Actor: "U12345678", ActivationID: "activation-1", ActivationScope: domain.ExternalAgentActivationLegacy},
		},
		{name: "unknown kind", origin: AgentTurnOrigin{Kind: "unknown"}},
		{name: "control character", origin: AgentTurnOrigin{Kind: AgentTurnOriginUser, Actor: "U123\n45678"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.origin.Validate()
			if (err == nil) != tc.valid {
				t.Fatalf("origin = %+v, err = %v, valid = %t", tc.origin, err, tc.valid)
			}
		})
	}
}

func TestAgentTurnContextRoundTripAndNilContext(t *testing.T) {
	turn := AgentTurnContext{
		ConversationKey: "slack:T12345678:dm:D12345678",
		Origin:          AgentTurnOrigin{Kind: AgentTurnOriginUser},
	}
	var nilContext context.Context
	ctx := WithAgentTurnContext(nilContext, turn)
	got, ok := AgentTurnContextFromContext(ctx)
	if !ok || got != turn {
		t.Fatalf("context value = %+v, ok=%t", got, ok)
	}
	if _, ok := AgentTurnContextFromContext(nilContext); ok {
		t.Fatal("nil context unexpectedly contained a turn")
	}
}

func TestPortWrappedErrorsPreserveCause(t *testing.T) {
	cause := errors.New("cause")
	canvas := &CanvasCreateError{Err: cause, Ambiguous: true}
	if canvas.Error() != cause.Error() || !errors.Is(canvas, cause) || !canvas.Ambiguous {
		t.Fatalf("canvas error = %v, ambiguous=%t", canvas, canvas.Ambiguous)
	}
	file := &GeneratedFileUploadError{Err: cause, Ambiguous: true}
	if file.Error() != cause.Error() || !errors.Is(file, cause) || !file.Ambiguous {
		t.Fatalf("file error = %v, ambiguous=%t", file, file.Ambiguous)
	}

	activation := NewActivationProcessError("activation_failed", true, cause)
	var activationError *ActivationProcessError
	if !errors.As(activation, &activationError) || activationError.Error() != "activation_failed" || !errors.Is(activation, cause) || !activationError.Retryable {
		t.Fatalf("activation error = %v", activation)
	}
	if fallback := NewActivationProcessError("", false, nil); fallback.Error() == "" {
		t.Fatal("activation fallback error is empty")
	}
	notification := NewNotificationPublishError("publish_failed", true, true, cause)
	var notificationError *NotificationPublishError
	if !errors.As(notification, &notificationError) || notificationError.Error() != "publish_failed" || !errors.Is(notification, cause) || !notificationError.Ambiguous ||
		!notificationError.Retryable {
		t.Fatalf("notification error = %v", notification)
	}
	if fallback := NewNotificationPublishError("", false, false, nil); fallback.Error() == "" {
		t.Fatal("notification fallback error is empty")
	}
}

func TestConfirmationDigestAndSystemClock(t *testing.T) {
	delivery := ConfirmationDelivery{
		WrapperCallID: "wrapper-1", OriginalCallID: "original-1", Actor: "U12345678", TeamID: "T12345678",
		ChannelID: "D12345678", ThreadTS: "1710000000.000001", Summary: "approve", Payload: "{}",
		ParameterHash: "parameters", RendererMode: "confirmation_v2", Expiry: time.Unix(1710000000, 0).UTC(),
	}
	first := ConfirmationContentDigest(delivery, strings.Repeat("a", 64))
	if first == "" || first != ConfirmationContentDigest(delivery, strings.Repeat("a", 64)) {
		t.Fatalf("digest is not deterministic: %q", first)
	}
	if first == ConfirmationContentDigest(delivery, strings.Repeat("b", 64)) {
		t.Fatal("layout change did not change confirmation digest")
	}

	before := time.Now()
	now := SystemClock{}.Now()
	after := time.Now()
	if now.Before(before) || now.After(after) {
		t.Fatalf("system clock returned %s outside [%s, %s]", now, before, after)
	}
}

func TestKnowledgeSnapshotMetricLabelsAndBindingValidation(t *testing.T) {
	if (KnowledgeSnapshot{}).Present() {
		t.Fatal("empty knowledge snapshot is present")
	}
	if !(KnowledgeSnapshot{Evidence: []KnowledgeEvidenceRef{{}}}).Present() {
		t.Fatal("knowledge evidence was not detected")
	}

	labels := MetricLabels{"project": "workspace"}
	clone := CloneMetricLabels(labels)
	clone["project"] = "other"
	if labels["project"] != "workspace" || CloneMetricLabels(nil) != nil || CloneMetricLabels(MetricLabels{}) != nil {
		t.Fatalf("metric label clone = %#v, source = %#v", clone, labels)
	}
	var recorder MetricRecorder = NoopMetricRecorder{}
	recorder.AddCounter("count", 1, labels)
	recorder.SetGauge("gauge", 1, labels)
	recorder.Observe("observation", 1, labels)
	if recorder.Snapshot() != nil {
		t.Fatal("noop metric recorder returned samples")
	}

	valid := WorkstreamBinding{Actor: "U12345678", ConversationKey: "slack:T12345678:dm:D12345678", Project: "workspace"}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []WorkstreamBinding{{}, {Actor: "U12345678"}, {ConversationKey: valid.ConversationKey}, {Project: "workspace"}} {
		if err := invalid.Validate(); err == nil {
			t.Fatalf("invalid binding accepted: %+v", invalid)
		}
	}
}

func TestAgentTurnContextWithBackgroundContext(t *testing.T) {
	turn := AgentTurnContext{Origin: AgentTurnOrigin{Kind: AgentTurnOriginUser}}
	if got, ok := AgentTurnContextFromContext(WithAgentTurnContext(context.Background(), turn)); !ok || got != turn {
		t.Fatalf("background context value = %+v, ok=%t", got, ok)
	}
}
