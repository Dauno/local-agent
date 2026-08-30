package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	slackapi "github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"

	"github.com/Dauno/slack-local-agent/internal/blockkit"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

func newTestCallback(actionID, wrapperCallID, teamID, userID, channelID, ts, threadTS string) slackapi.InteractionCallback {
	cb := slackapi.InteractionCallback{
		Type: slackapi.InteractionTypeBlockActions,
		Team: slackapi.Team{ID: teamID},
		User: slackapi.User{ID: userID},
		Message: slackapi.Message{
			Msg: slackapi.Msg{
				Timestamp: ts, ThreadTimestamp: threadTS,
				Metadata: slackapi.SlackMetadata{
					EventType: confirmationMetadataEventType,
					EventPayload: map[string]any{
						"correlation_id": "confirmation:" + wrapperCallID,
						"render_mode":    confirmationRenderMode,
						"content_sha256": strings.Repeat("a", 64),
					},
				},
			},
		},
		ActionCallback: slackapi.ActionCallbacks{
			BlockActions: []*slackapi.BlockAction{
				{ActionID: actionID, Value: wrapperCallID},
			},
		},
	}
	cb.Channel.ID = channelID
	return cb
}

func newViewCallback() slackapi.InteractionCallback {
	cb := slackapi.InteractionCallback{
		Type: slackapi.InteractionTypeViewSubmission,
		Team: slackapi.Team{ID: "T12345678"},
		User: slackapi.User{ID: "U12345678"},
		Message: slackapi.Message{
			Msg: slackapi.Msg{Timestamp: "1720000001.000001"},
		},
	}
	cb.Channel.ID = "C12345678"
	return cb
}

func TestNormalizeInteractiveActionApprove(t *testing.T) {
	t.Parallel()
	callback := newTestCallback(approveActionID, "wrapper-abc", "T12345678", "U12345678", "C12345678", "1720000001.000001", "1720000000.000000")

	action, ok := normalizeInteractiveAction(&callback)
	if !ok {
		t.Fatal("normalizeInteractiveAction returned false for valid approve")
	}
	if !action.Approved {
		t.Error("expected Approved=true")
	}
	if action.WrapperCallID != "wrapper-abc" {
		t.Errorf("WrapperCallID = %q, want %q", action.WrapperCallID, "wrapper-abc")
	}
	if action.Actor != "U12345678" {
		t.Errorf("Actor = %q, want %q", action.Actor, "U12345678")
	}
}

func TestNormalizeInteractiveActionReject(t *testing.T) {
	t.Parallel()
	callback := newTestCallback(rejectActionID, "wrapper-xyz", "T12345678", "U12345678", "D12345678", "1720000001.000001", "")

	action, ok := normalizeInteractiveAction(&callback)
	if !ok {
		t.Fatal("normalizeInteractiveAction returned false for valid reject")
	}
	if action.Approved {
		t.Error("expected Approved=false for reject")
	}
	if action.WrapperCallID != "wrapper-xyz" {
		t.Errorf("WrapperCallID = %q, want %q", action.WrapperCallID, "wrapper-xyz")
	}
}

func TestNormalizeInteractiveActionThreadedDMKey(t *testing.T) {
	t.Parallel()
	callback := newTestCallback(approveActionID, "wrapper-dm", "T12345678", "U12345678", "D12345678", "1720000001.000001", "1720000000.000000")

	action, ok := normalizeInteractiveAction(&callback)
	if !ok {
		t.Fatal("normalizeInteractiveAction returned false for threaded DM")
	}
	want := domain.ConversationKey("slack:T12345678:dm:D12345678:thread:1720000000.000000")
	if action.ConversationKey != want {
		t.Fatalf("ConversationKey = %q, want %q", action.ConversationKey, want)
	}
}

func TestNormalizeInteractiveActionUnknownActionID(t *testing.T) {
	t.Parallel()
	callback := newTestCallback("unknown.action", "wrapper-abc", "T12345678", "U12345678", "C12345678", "1720000001.000001", "")

	_, ok := normalizeInteractiveAction(&callback)
	if ok {
		t.Error("normalizeInteractiveAction should return false for unknown action ID")
	}
}

func TestNormalizeInteractiveActionEmptyBlockActions(t *testing.T) {
	t.Parallel()
	cb := slackapi.InteractionCallback{
		Type: slackapi.InteractionTypeBlockActions,
		Team: slackapi.Team{ID: "T12345678"},
		User: slackapi.User{ID: "U12345678"},
		Message: slackapi.Message{
			Msg: slackapi.Msg{Timestamp: "1720000001.000001"},
		},
	}
	cb.Channel.ID = "C12345678"

	_, ok := normalizeInteractiveAction(&cb)
	if ok {
		t.Error("normalizeInteractiveAction should return false for nil block actions")
	}
}

func TestNormalizeInteractiveActionEmptyValue(t *testing.T) {
	t.Parallel()
	callback := newTestCallback(approveActionID, "", "T12345678", "U12345678", "C12345678", "1720000001.000001", "")

	_, ok := normalizeInteractiveAction(&callback)
	if ok {
		t.Error("normalizeInteractiveAction should return false for empty value")
	}
}

func TestNormalizeInteractiveActionWrongType(t *testing.T) {
	t.Parallel()
	callback := newViewCallback()

	_, ok := normalizeInteractiveAction(&callback)
	if ok {
		t.Error("normalizeInteractiveAction should return false for non-block-action type")
	}
}

func TestNormalizeInteractiveActionNilCallback(t *testing.T) {
	t.Parallel()
	_, ok := normalizeInteractiveAction(nil)
	if ok {
		t.Error("normalizeInteractiveAction should return false for nil callback")
	}
}

func TestNormalizeInteractiveActionAcceptsDocumentedPayloadWithoutMetadata(t *testing.T) {
	t.Parallel()
	callback := newTestCallback(approveActionID, "wrapper-abc", "T12345678", "U12345678", "C12345678", "1720000001.000001", "")
	callback.Message.Metadata = slackapi.SlackMetadata{}

	action, ok := normalizeInteractiveAction(&callback)
	if !ok {
		t.Fatal("normalizeInteractiveAction rejected Slack's documented payload without message metadata")
	}
	if action.CorrelationID != "" || action.RendererMode != "" || action.ContentSHA256 != "" {
		t.Fatalf("optional metadata fields = %#v", action)
	}
}

func TestNormalizeInteractiveActionRejectsRetiredV1Metadata(t *testing.T) {
	t.Parallel()
	callback := newTestCallback(approveActionID, "wrapper-v1", "T12345678", "U12345678", "C12345678", "1720000001.000001", "")
	callback.Message.Metadata.EventPayload["render_mode"] = "confirmation_v1"
	if _, ok := normalizeInteractiveAction(&callback); ok {
		t.Fatal("normalizeInteractiveAction accepted retired v1 confirmation metadata")
	}
}

func TestNormalizeInteractiveActionRejectsConflictingContainer(t *testing.T) {
	t.Parallel()
	callback := newTestCallback(approveActionID, "wrapper-abc", "T12345678", "U12345678", "C12345678", "1720000001.000001", "")
	callback.Container.MessageTs = "1720000002.000002"

	if _, ok := normalizeInteractiveAction(&callback); ok {
		t.Fatal("normalizeInteractiveAction accepted conflicting message timestamps")
	}
}

func TestConfirmationPromptRendersSemanticValues(t *testing.T) {
	delivery := port.ConfirmationDelivery{
		WrapperCallID: "wrapper-abc", OriginalCallID: "orig-abc", Summary: "Write <@U12345678> & report",
		Payload: `{"project":"repo","task":"Inspect <@U12345678> and report","workstream_id":"ws-1","expected_revision":4,"action":"propose_task","task_id":"task-1","current_phase":"plan"}`,
		Expiry:  time.Date(2026, 7, 21, 15, 30, 0, 0, time.UTC),
	}
	fallback, blocks, err := compileConfirmationMessageV2(mustConfirmationEngine(t), delivery, mustConfirmationLayoutSHA256(t))
	if err != nil {
		t.Fatal(err)
	}
	message := blockkit.Message{FallbackText: fallback, Blocks: blocks}
	for _, value := range []string{delivery.Summary, "orig-abc", "15:30", "repo", "Inspect <@U12345678> and report", "ws-1", "propose_task"} {
		if !blockkit.Reachable(message, value) {
			t.Fatalf("value %q did not reach the rendered tree", value)
		}
	}
	if slot, ok := blockkit.SlotOf(message, delivery.Summary); !ok || slot != slackapi.PlainTextType {
		t.Fatalf("SlotOf(summary) = %q, %t", slot, ok)
	}
	if slot, ok := blockkit.SlotOf(message, delivery.OriginalCallID); !ok || slot != slackapi.MarkdownType {
		t.Fatalf("SlotOf(call_id) = %q, %t", slot, ok)
	}
	if slot, ok := blockkit.SlotOf(message, "repo"); !ok || slot != slackapi.PlainTextType {
		t.Fatalf("SlotOf(project) = %q, %t", slot, ok)
	}
	encoded, err := json.Marshal(blocks)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	if strings.Count(text, `"value":"wrapper-abc"`) != 3 {
		t.Fatalf("confirmation action values = %s", text)
	}
	if strings.Contains(text, "Payload:") || strings.Contains(text, "workstream_id") {
		t.Fatalf("JSON payload was rendered as raw confirmation content: %s", text)
	}
	if fallback == "" || strings.ContainsAny(fallback, "*`") || strings.Contains(fallback, "<@U12345678>") {
		t.Fatalf("fallback = %q", fallback)
	}
}

func TestConfirmationPromptUsesDefaultsAndOptionalRegions(t *testing.T) {
	fallback, blocks, err := compileConfirmationMessageV2(mustConfirmationEngine(t), port.ConfirmationDelivery{
		WrapperCallID: "wrapper-1", OriginalCallID: "call-1", Summary: "Summary", Expiry: time.Date(2026, 7, 21, 15, 30, 0, 0, time.UTC),
	}, mustConfirmationLayoutSHA256(t))
	if err != nil {
		t.Fatal(err)
	}
	message := blockkit.Message{FallbackText: fallback, Blocks: blocks}
	for _, value := range []string{"not provided"} {
		if !blockkit.Reachable(message, value) {
			t.Fatalf("default value %q did not reach the rendered tree", value)
		}
	}
	encoded, err := json.Marshal(blocks)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "Workstream data:") || strings.Contains(string(encoded), "Payload:") {
		t.Fatalf("optional regions rendered without values: %s", encoded)
	}
}

func TestConfirmationPromptRendersLongUnparsedPayload(t *testing.T) {
	payload := strings.Repeat("abcdefghij_", 900)
	fallback, blocks, err := compileConfirmationMessageV2(mustConfirmationEngine(t), port.ConfirmationDelivery{
		WrapperCallID: "wrapper-1", OriginalCallID: "call-1", Summary: "Summary", Payload: payload,
		Expiry: time.Date(2026, 7, 21, 15, 30, 0, 0, time.UTC),
	}, mustConfirmationLayoutSHA256(t))
	if err != nil {
		t.Fatal(err)
	}
	message := blockkit.Message{FallbackText: fallback, Blocks: blocks}
	if !blockkit.Reachable(message, payload[:100]) || !blockkit.Reachable(message, payload[len(payload)-100:]) {
		t.Fatal("long unparsed payload did not reach the rendered tree")
	}
}

func TestConfirmationPromptChunksPayload(t *testing.T) {
	payload := strings.Repeat("x", 2801)
	_, blocks, err := compileConfirmationMessageV2(mustConfirmationEngine(t), port.ConfirmationDelivery{
		WrapperCallID: "wrapper-1", OriginalCallID: "call-1", Summary: "Summary", Payload: payload,
		Expiry: time.Date(2026, 7, 21, 15, 30, 0, 0, time.UTC),
	}, mustConfirmationLayoutSHA256(t))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(blocks)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(encoded), "Payload:"); got != 1 {
		t.Fatalf("payload header count = %d, want 1", got)
	}
	if got := strings.Count(string(encoded), `"type":"section"`); got < 4 {
		t.Fatalf("payload render did not include both chunks: %s", encoded)
	}
}

func TestConfirmationPromptTruncatesLongJSONTask(t *testing.T) {
	longTask := strings.Repeat("task_", 700)
	fallback, blocks, err := compileConfirmationMessageV2(mustConfirmationEngine(t), port.ConfirmationDelivery{
		WrapperCallID: "wrapper-1", OriginalCallID: "call-1", Summary: "Summary",
		Payload: `{"project":"repo","task":"` + longTask + `"}`,
		Expiry:  time.Date(2026, 7, 21, 15, 30, 0, 0, time.UTC),
	}, mustConfirmationLayoutSHA256(t))
	if err != nil {
		t.Fatal(err)
	}
	message := blockkit.Message{FallbackText: fallback, Blocks: blocks}
	data, err := json.Marshal(message.Blocks)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "...") || strings.Contains(string(data), longTask) {
		t.Fatalf("long JSON task was not bounded: %s", data)
	}
}

func TestConfirmationPromptExtractsStructTaskDescription(t *testing.T) {
	fallback, blocks, err := compileConfirmationMessageV2(mustConfirmationEngine(t), port.ConfirmationDelivery{
		WrapperCallID: "wrapper-1", Summary: "Approve task", OriginalCallID: "call-1",
		Payload: `{"project":"repo","task":{"ID":"task-1","Project":"repo","Description":"Inspect the repository"}}`,
		Expiry:  time.Date(2026, 7, 21, 15, 30, 0, 0, time.UTC),
	}, mustConfirmationLayoutSHA256(t))
	if err != nil {
		t.Fatal(err)
	}
	message := blockkit.Message{FallbackText: fallback, Blocks: blocks}
	if !blockkit.Reachable(message, "Inspect the repository") {
		t.Fatal("task description did not reach the rendered tree")
	}
}

func TestConfirmationResolvedRendersWithAndWithoutResult(t *testing.T) {
	statuses := []port.ConfirmationDeliveryStatus{
		port.ConfirmationApproved, port.ConfirmationConsumed, port.ConfirmationRejected,
		port.ConfirmationExpired, port.ConfirmationFailed,
	}
	for _, status := range statuses {
		for _, result := range []string{"", "done <@U12345678> & report"} {
			name := string(status)
			if result != "" {
				name += " with result"
			}
			t.Run(name, func(t *testing.T) {
				fallback, blocks, err := compileConfirmationResolvedMessage(mustConfirmationEngine(t), port.ConfirmationDelivery{
					Status: status, Summary: "Write <@U12345678> & report", OriginalCallID: "orig-abc",
				}, time.Date(2026, 7, 21, 15, 30, 0, 0, time.UTC), result)
				if err != nil {
					t.Fatal(err)
				}
				message := blockkit.Message{FallbackText: fallback, Blocks: blocks}
				wantStatus := string(status)
				if status == port.ConfirmationConsumed {
					wantStatus = string(port.ConfirmationApproved)
				}
				for _, value := range []string{wantStatus, "Write <@U12345678> & report", "orig-abc", "15:30"} {
					if !blockkit.Reachable(message, value) {
						t.Fatalf("value %q did not reach the resolved tree", value)
					}
				}
				encoded, err := json.Marshal(blocks)
				if err != nil {
					t.Fatal(err)
				}
				if result == "" && strings.Contains(string(encoded), "Confirmation result:") {
					t.Fatal("empty result rendered a result block")
				}
				if result != "" && !blockkit.Reachable(message, result) {
					t.Fatal("result did not reach the resolved tree")
				}
				if fallback == "" || strings.ContainsAny(fallback, "*`") || strings.Contains(fallback, "<@U12345678>") {
					t.Fatalf("fallback = %q", fallback)
				}
			})
		}
	}
}

func mustConfirmationEngine(t *testing.T) *blockkit.Engine {
	t.Helper()
	engine, err := newConfirmationViewEngine()
	if err != nil {
		t.Fatalf("new confirmation view engine: %v", err)
	}
	if err := engine.Register(confirmationPromptView{}, confirmationResolvedView{}); err != nil {
		t.Fatalf("register confirmation views: %v", err)
	}
	return engine
}

func mustConfirmationLayoutSHA256(t *testing.T) string {
	t.Helper()
	engine := mustConfirmationEngine(t)
	layoutSHA256, ok := engine.LayoutSHA256(confirmationPromptTemplateName)
	if !ok || layoutSHA256 == "" {
		t.Fatalf("confirmation prompt layout fingerprint = %q, %t", layoutSHA256, ok)
	}
	return layoutSHA256
}

func TestConfirmationMetadata(t *testing.T) {
	t.Parallel()
	delivery := port.ConfirmationDelivery{
		WrapperCallID: "wrapper-abc", OriginalCallID: "orig-abc",
		Summary: "Write file", CorrelationID: "confirmation:wrapper-abc",
		Expiry: time.Date(2026, 7, 21, 15, 30, 0, 0, time.UTC),
	}
	metadata := confirmationMetadata(delivery, mustConfirmationLayoutSHA256(t))
	if metadata.EventType != confirmationMetadataEventType {
		t.Errorf("metadata EventType = %q, want %q", metadata.EventType, confirmationMetadataEventType)
	}
}

func TestConfirmationContentDigestDeterministic(t *testing.T) {
	t.Parallel()
	delivery := port.ConfirmationDelivery{
		WrapperCallID: "wrapper-abc", OriginalCallID: "orig-abc",
		Summary: "Write file", Expiry: time.Date(2026, 7, 21, 15, 30, 0, 0, time.UTC),
	}
	layoutSHA256 := mustConfirmationLayoutSHA256(t)
	d1 := confirmationContentDigest(delivery, layoutSHA256)
	d2 := confirmationContentDigest(delivery, layoutSHA256)
	if d1 != d2 {
		t.Errorf("digest not deterministic: %q vs %q", d1, d2)
	}
	if d1 == "" {
		t.Error("digest is empty")
	}
}

func TestConfirmationPublisherResolvesPromptLayoutFingerprint(t *testing.T) {
	t.Parallel()
	publisher := newConfirmationPublisher(nil, "U99999999", 5*time.Second, nil)
	got, err := publisher.ConfirmationPromptLayoutSHA256()
	if err != nil {
		t.Fatalf("ConfirmationPromptLayoutSHA256() error = %v", err)
	}
	engine := mustConfirmationEngine(t)
	want, ok := engine.LayoutSHA256(confirmationPromptTemplateName)
	if !ok || got != want {
		t.Fatalf("prompt layout fingerprint = %q, want %q", got, want)
	}
}

func TestConfirmationPromptCompileRejectsFingerprintMismatch(t *testing.T) {
	t.Parallel()
	_, _, err := compileConfirmationMessageV2(
		mustConfirmationEngine(t),
		port.ConfirmationDelivery{WrapperCallID: "wrapper", OriginalCallID: "call", Summary: "summary", Expiry: time.Now().Add(time.Hour)},
		strings.Repeat("0", 64),
	)
	if err == nil {
		t.Fatal("compileConfirmationMessageV2 accepted a mismatched layout fingerprint")
	}
}

func TestConfirmationContentDigestChangesWithLayoutFingerprint(t *testing.T) {
	t.Parallel()
	delivery := port.ConfirmationDelivery{
		WrapperCallID: "wrapper-abc", OriginalCallID: "orig-abc", Summary: "Write file",
		Expiry: time.Date(2026, 7, 21, 15, 30, 0, 0, time.UTC),
	}
	left := port.ConfirmationContentDigest(delivery, strings.Repeat("a", 64))
	right := port.ConfirmationContentDigest(delivery, strings.Repeat("b", 64))
	if left == right {
		t.Fatal("confirmation digest ignored the layout fingerprint")
	}
}

func TestConfirmationContentDigestBindsPresentationContract(t *testing.T) {
	delivery := port.ConfirmationDelivery{
		WrapperCallID: "wrapper-abc", OriginalCallID: "orig-abc", Actor: "U12345678",
		TeamID: "T12345678", ChannelID: "D12345678", ThreadTS: "1710000000.000000",
		Summary: "Write file", ParameterHash: "abc123", RendererMode: confirmationRenderModeV2,
		Expiry: time.Date(2026, 7, 21, 15, 30, 0, 0, time.UTC),
	}
	layoutSHA256 := mustConfirmationLayoutSHA256(t)
	digest := port.ConfirmationContentDigest(delivery, layoutSHA256)
	withPayload := delivery
	withPayload.Payload = `{"project":"repo","task":"Inspect the repository"}`
	if digest == port.ConfirmationContentDigest(withPayload, layoutSHA256) {
		t.Fatal("digest omitted display payload data")
	}
}

func TestListenerIgnoresInteractiveWhenNoHandlerSet(t *testing.T) {
	t.Parallel()
	client := newFakeSocketClient()
	listener := newListener(client, NewRouter(testBot), nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- listener.Run(ctx, func(context.Context, domain.Invocation) {})
	}()

	callback := newTestCallback(approveActionID, "wrapper-abc", "T12345678", "U12345678", "C12345678", "1720000001.000001", "")
	client.events <- socketmode.Event{
		Type:    socketmode.EventTypeInteractive,
		Data:    callback,
		Request: &socketmode.Request{Type: "interactive", EnvelopeID: "interactive-1"},
	}

	deadline := time.After(time.Second)
	for !client.wasAcked("interactive-1") {
		select {
		case <-deadline:
			t.Fatal("interactive envelope was not acknowledged")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run() shutdown error = %v", err)
	}
}

func TestListenerDispatchesInteractiveEvents(t *testing.T) {
	t.Parallel()
	client := newFakeSocketClient()
	listener := newListener(client, NewRouter(testBot), nil)
	received := make(chan domain.ConfirmationInteractiveAction, 1)
	listener.SetInteractiveHandler(func(_ context.Context, action domain.ConfirmationInteractiveAction) error {
		received <- action
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- listener.Run(ctx, func(context.Context, domain.Invocation) {})
	}()

	callback := newTestCallback(approveActionID, "wrapper-test", "T12345678", "U12345678", "C12345678", "1720000001.000001", "1720000000.000000")
	client.events <- socketmode.Event{
		Type:    socketmode.EventTypeInteractive,
		Data:    callback,
		Request: &socketmode.Request{Type: "interactive", EnvelopeID: "interactive-dispatch"},
	}

	select {
	case action := <-received:
		if !action.Approved {
			t.Error("expected approved=true")
		}
		if action.WrapperCallID != "wrapper-test" {
			t.Errorf("WrapperCallID = %q", action.WrapperCallID)
		}
		if action.Actor != "U12345678" {
			t.Errorf("Actor = %q", action.Actor)
		}
	case <-time.After(time.Second):
		t.Fatal("interactive handler was not dispatched")
	}

	if !client.wasAcked("interactive-dispatch") {
		t.Fatal("interactive envelope was not acknowledged before dispatch")
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run() shutdown error = %v", err)
	}
}

func TestNonBlockActionInteractiveIgnored(t *testing.T) {
	t.Parallel()
	client := newFakeSocketClient()
	listener := newListener(client, NewRouter(testBot), nil)
	called := atomic.Bool{}
	listener.SetInteractiveHandler(func(_ context.Context, _ domain.ConfirmationInteractiveAction) error {
		called.Store(true)
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- listener.Run(ctx, func(context.Context, domain.Invocation) {})
	}()

	callback := newTestCallback("other.action", "val", "T12345678", "U12345678", "C12345678", "1720000001.000001", "")
	client.events <- socketmode.Event{
		Type:    socketmode.EventTypeInteractive,
		Data:    callback,
		Request: &socketmode.Request{Type: "interactive", EnvelopeID: "interactive-ignored"},
	}

	deadline := time.After(time.Second)
	for !client.wasAcked("interactive-ignored") {
		select {
		case <-deadline:
			t.Fatal("interactive envelope was not acknowledged")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	time.Sleep(20 * time.Millisecond)
	if called.Load() {
		t.Error("handler should not be called for unknown action ID")
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run() shutdown error = %v", err)
	}
}

type fakeConfirmationBlockClient struct {
	mu               sync.Mutex
	postedChans      []string
	postedBlocks     [][]slackapi.Block
	updatedChans     []string
	updatedTS        []string
	updatedBlocks    [][]slackapi.Block
	updatedFallbacks []string
	fallbackTexts    []string
	postedMetadata   []slackapi.SlackMetadata
	postedThreads    []string
	messages         []slackapi.Message
	hasMore          bool
	postErr          error
	updateErr        error
	historyErr       error
}

func (c *fakeConfirmationBlockClient) PostBlocks(_ context.Context, channelID, fallbackText string, blocks []slackapi.Block, metadata slackapi.SlackMetadata, threadTS string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.postErr != nil {
		return "", c.postErr
	}
	c.postedChans = append(c.postedChans, channelID)
	c.postedBlocks = append(c.postedBlocks, blocks)
	c.fallbackTexts = append(c.fallbackTexts, fallbackText)
	c.postedMetadata = append(c.postedMetadata, metadata)
	c.postedThreads = append(c.postedThreads, threadTS)
	return fmt.Sprintf("172000000%d.000001", len(c.postedChans)), nil
}

func (c *fakeConfirmationBlockClient) ConfirmationMessages(_ context.Context, _, _ string, _ int) ([]slackapi.Message, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]slackapi.Message(nil), c.messages...), c.hasMore, c.historyErr
}

func (c *fakeConfirmationBlockClient) UpdateBlocks(_ context.Context, channelID, messageTS string, blocks []slackapi.Block, fallback string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.updateErr != nil {
		return c.updateErr
	}
	c.updatedChans = append(c.updatedChans, channelID)
	c.updatedTS = append(c.updatedTS, messageTS)
	c.updatedBlocks = append(c.updatedBlocks, blocks)
	c.updatedFallbacks = append(c.updatedFallbacks, fallback)
	return nil
}

func TestConfirmationPublisherPublish(t *testing.T) {
	t.Parallel()
	client := &fakeConfirmationBlockClient{}
	pub := newConfirmationPublisher(client, "U99999999", 5*time.Second, nil)

	delivery := port.ConfirmationDelivery{
		WrapperCallID: "wrapper-abc", OriginalCallID: "orig-abc",
		ChannelID: "C12345678", Summary: "Write file",
		CorrelationID: "confirmation:wrapper-abc",
		Expiry:        time.Date(2026, 7, 21, 15, 30, 0, 0, time.UTC),
	}

	result, err := pub.PublishConfirmation(context.Background(), delivery)
	if err != nil {
		t.Fatalf("PublishConfirmation() error = %v", err)
	}
	if result.SlackMessageTS == "" {
		t.Error("SlackMessageTS is empty")
	}
	if len(client.postedChans) != 1 || client.postedChans[0] != "C12345678" {
		t.Errorf("posted channel = %v, want [C12345678]", client.postedChans)
	}
	if len(client.fallbackTexts) != 1 || client.fallbackTexts[0] == "" {
		t.Fatalf("accessible fallback = %q", client.fallbackTexts)
	}
	if client.postedThreads[0] != delivery.ThreadTS {
		t.Fatalf("posted thread = %q, want %q", client.postedThreads[0], delivery.ThreadTS)
	}
	if len(client.postedMetadata) != 1 || client.postedMetadata[0].EventType != confirmationMetadataEventType {
		t.Fatalf("posted metadata = %#v", client.postedMetadata)
	}
	if client.postedMetadata[0].EventPayload["correlation_id"] != delivery.CorrelationID ||
		client.postedMetadata[0].EventPayload["render_mode"] != confirmationRenderMode ||
		client.postedMetadata[0].EventPayload["content_sha256"] != confirmationContentDigest(delivery, pub.layoutSHA256) {
		t.Fatalf("posted metadata payload = %#v", client.postedMetadata[0].EventPayload)
	}
	message := blockkit.Message{FallbackText: client.fallbackTexts[0], Blocks: client.postedBlocks[0]}
	for _, value := range []string{"Write file", "orig-abc", "15:30", "not provided"} {
		if !blockkit.Reachable(message, value) {
			t.Fatalf("published value %q did not reach the rendered tree", value)
		}
	}
	encoded, err := json.Marshal(client.postedBlocks[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(encoded), `"value":"wrapper-abc"`) != 3 {
		t.Fatalf("published action values = %s", encoded)
	}
}

func TestConfirmationPublisherUpdate(t *testing.T) {
	t.Parallel()
	client := &fakeConfirmationBlockClient{}
	pub := newConfirmationPublisher(client, "U99999999", 5*time.Second, nil)

	delivery := port.ConfirmationDelivery{
		WrapperCallID: "wrapper-abc", OriginalCallID: "orig-abc",
		ChannelID: "C12345678", Summary: "Write file",
		SlackMessageTS: "1720000001.000001",
		Status:         port.ConfirmationConsumed,
	}

	err := pub.UpdateConfirmation(context.Background(), delivery, "done")
	if err != nil {
		t.Fatalf("UpdateConfirmation() error = %v", err)
	}
	if len(client.updatedChans) != 1 || client.updatedChans[0] != "C12345678" {
		t.Errorf("updated channel = %v", client.updatedChans)
	}
	if client.updatedTS[0] != "1720000001.000001" {
		t.Errorf("updated timestamp = %q", client.updatedTS[0])
	}
	message := blockkit.Message{FallbackText: client.updatedFallbacks[0], Blocks: client.updatedBlocks[0]}
	for _, value := range []string{"Write file", "orig-abc", "approved", "done"} {
		if !blockkit.Reachable(message, value) {
			t.Fatalf("updated value %q did not reach the rendered tree", value)
		}
	}
	if message.FallbackText == "" || strings.ContainsAny(message.FallbackText, "*`") {
		t.Fatalf("updated fallback = %q", message.FallbackText)
	}
}

func TestConfirmationPublisherUpdateNonTerminalStatus(t *testing.T) {
	t.Parallel()
	client := &fakeConfirmationBlockClient{}
	pub := newConfirmationPublisher(client, "U99999999", 5*time.Second, nil)

	delivery := port.ConfirmationDelivery{
		WrapperCallID:  "wrapper-abc",
		ChannelID:      "C12345678",
		SlackMessageTS: "1720000001.000001",
		Status:         port.ConfirmationPending,
	}

	err := pub.UpdateConfirmation(context.Background(), delivery, "text")
	if err == nil {
		t.Fatal("UpdateConfirmation should fail for non-terminal status")
	}
}

func TestConfirmationPublisherPublishError(t *testing.T) {
	t.Parallel()
	client := &fakeConfirmationBlockClient{postErr: errors.New("slack down")}
	pub := newConfirmationPublisher(client, "U99999999", 5*time.Second, nil)

	delivery := port.ConfirmationDelivery{
		WrapperCallID: "wrapper-abc", ChannelID: "C12345678",
		Expiry: time.Now().Add(15 * time.Minute),
	}
	_, err := pub.PublishConfirmation(context.Background(), delivery)
	if err == nil {
		t.Fatal("PublishConfirmation should return error")
	}
}

func TestConfirmationPublisherNilClient(t *testing.T) {
	t.Parallel()
	pub := newConfirmationPublisher(nil, "U99999999", 5*time.Second, nil)
	delivery := port.ConfirmationDelivery{ChannelID: "C12345678", Expiry: time.Now()}
	_, err := pub.PublishConfirmation(context.Background(), delivery)
	if err == nil {
		t.Fatal("PublishConfirmation with nil client should error")
	}
}

func TestConfirmationPublisherRecoversMatchingPrompt(t *testing.T) {
	t.Parallel()
	delivery := port.ConfirmationDelivery{
		WrapperCallID: "wrapper-abc", OriginalCallID: "orig-abc", Actor: "U12345678",
		TeamID: "T12345678", ChannelID: "C12345678", ThreadTS: "1720000000.000000",
		Summary: "Write file", ParameterHash: "abc123", CorrelationID: "confirmation:wrapper-abc",
		RendererMode: confirmationRenderMode, Expiry: time.Date(2026, 7, 21, 15, 30, 0, 0, time.UTC),
	}
	client := &fakeConfirmationBlockClient{messages: []slackapi.Message{{
		User: "U99999999", Timestamp: "1720000001.000001", Metadata: confirmationMetadata(delivery, mustConfirmationLayoutSHA256(t)),
	}}}
	pub := newConfirmationPublisher(client, "U99999999", 5*time.Second, nil)

	result, found, err := pub.RecoverConfirmation(t.Context(), delivery)
	if err != nil || !found || result.SlackMessageTS != "1720000001.000001" {
		t.Fatalf("RecoverConfirmation() = %#v, %t, %v", result, found, err)
	}
}

func TestConfirmationPublisherRecoveryFailsClosed(t *testing.T) {
	t.Parallel()
	delivery := port.ConfirmationDelivery{
		WrapperCallID: "wrapper-abc", OriginalCallID: "orig-abc", Actor: "U12345678",
		TeamID: "T12345678", ChannelID: "D12345678", Summary: "Write file",
		CorrelationID: "confirmation:wrapper-abc", RendererMode: confirmationRenderMode,
		Expiry: time.Date(2026, 7, 21, 15, 30, 0, 0, time.UTC),
	}
	metadata := confirmationMetadata(delivery, mustConfirmationLayoutSHA256(t))
	metadata.EventPayload["content_sha256"] = strings.Repeat("0", 64)
	client := &fakeConfirmationBlockClient{messages: []slackapi.Message{{
		User: "U99999999", Timestamp: "1720000001.000001", Metadata: metadata,
	}}}
	pub := newConfirmationPublisher(client, "U99999999", 5*time.Second, nil)

	if _, _, err := pub.RecoverConfirmation(t.Context(), delivery); err == nil {
		t.Fatal("RecoverConfirmation accepted mismatched digest")
	}

	client.messages = nil
	client.hasMore = true
	if _, _, err := pub.RecoverConfirmation(t.Context(), delivery); err == nil {
		t.Fatal("RecoverConfirmation accepted incomplete history")
	}

	client.messages = []slackapi.Message{{
		User: "U99999999", Timestamp: "1720000001.000001", Metadata: confirmationMetadata(delivery, mustConfirmationLayoutSHA256(t)),
	}}
	if _, _, err := pub.RecoverConfirmation(t.Context(), delivery); err == nil {
		t.Fatal("RecoverConfirmation accepted a match from incomplete history")
	}
}
