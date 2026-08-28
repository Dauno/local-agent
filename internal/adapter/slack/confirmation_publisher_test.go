package slack

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	slackapi "github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"

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

func TestNormalizeInteractiveActionAcceptsPendingV1Metadata(t *testing.T) {
	t.Parallel()
	callback := newTestCallback(approveActionID, "wrapper-v1", "T12345678", "U12345678", "C12345678", "1720000001.000001", "")
	callback.Message.Metadata.EventPayload["render_mode"] = confirmationRenderModeV1
	if _, ok := normalizeInteractiveAction(&callback); !ok {
		t.Fatal("normalizeInteractiveAction rejected a pending v1 confirmation")
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

func TestRenderConfirmationBlocks(t *testing.T) {
	t.Parallel()
	delivery := port.ConfirmationDelivery{
		WrapperCallID: "wrapper-abc", OriginalCallID: "orig-abc",
		Summary: "Write file", CorrelationID: "confirmation:wrapper-abc",
		Expiry: time.Date(2026, 7, 21, 15, 30, 0, 0, time.UTC),
	}
	blocks := renderConfirmationBlocks(delivery)
	if len(blocks) != 3 {
		t.Fatalf("renderConfirmationBlocks returned %d blocks, want 3", len(blocks))
	}
}

func TestConfirmationV2RendersOrderedSafeBlocks(t *testing.T) {
	delivery := port.ConfirmationDelivery{
		WrapperCallID: "wrapper-abc", OriginalCallID: "orig-abc", Summary: "Write <@U12345678> & report",
		Payload: `{"project":"repo","task":"Inspect <@U12345678> and report","workstream_id":"ws-1","expected_revision":4,"action":"propose_task","task_id":"task-1","current_phase":"plan"}`,
		Expiry:  time.Date(2026, 7, 21, 15, 30, 0, 0, time.UTC),
	}
	fallback, blocks, err := compileConfirmationMessage(mustEmbeddedRenderer(t), delivery)
	if err != nil {
		t.Fatal(err)
	}
	if utf8.RuneCountInString(fallback) > maxFallbackText || strings.Contains(fallback, "<@U12345678>") {
		t.Fatalf("fallback = %q", fallback)
	}
	if len(blocks) != 4 {
		t.Fatalf("blocks = %d, want card, task, workstream, actions", len(blocks))
	}
	card := blocks[0].(*slackapi.CardBlock)
	if card.Title == nil || card.Title.Type != slackapi.PlainTextType || card.Title.Text != "Confirmation required" {
		t.Fatalf("card title = %#v", card.Title)
	}
	if card.Subtitle == nil || card.Subtitle.Type != slackapi.MarkdownType {
		t.Fatalf("card subtitle = %#v", card.Subtitle)
	}
	if card.Body == nil || card.Body.Type != slackapi.PlainTextType || card.Body.Text != confirmationCardSummary(delivery.Summary) {
		t.Fatalf("card body = %#v", card.Body)
	}
	task := blocks[1].(*slackapi.SectionBlock)
	if task.Text.Type != slackapi.PlainTextType || !strings.Contains(task.Text.Text, "Inspect &lt;@U12345678>") || len(task.Fields) != 1 || task.Fields[0].Type != slackapi.PlainTextType ||
		task.Fields[0].Text != "Project: repo" {
		t.Fatalf("task block = %#v", task)
	}
	workstream := blocks[2].(*slackapi.SectionBlock)
	if workstream.Text.Type != slackapi.PlainTextType || !strings.Contains(workstream.Text.Text, "Workstream data:") || !strings.Contains(workstream.Text.Text, "Workstream ID: ws-1") {
		t.Fatalf("workstream block = %#v", workstream)
	}
	actions := blocks[3].(*slackapi.ActionBlock)
	if len(actions.Elements.ElementSet) != 3 {
		t.Fatalf("confirmation buttons = %d, want 3", len(actions.Elements.ElementSet))
	}
	approve := actions.Elements.ElementSet[0].(*slackapi.ButtonBlockElement)
	reject := actions.Elements.ElementSet[1].(*slackapi.ButtonBlockElement)
	status := actions.Elements.ElementSet[2].(*slackapi.ButtonBlockElement)
	if approve.ActionID != approveActionID || reject.ActionID != rejectActionID || status.ActionID != statusActionID ||
		approve.Value != delivery.WrapperCallID || reject.Value != delivery.WrapperCallID || status.Value != delivery.WrapperCallID {
		t.Fatalf("confirmation buttons = %#v", actions.Elements.ElementSet)
	}
	if approve.Text.Type != slackapi.PlainTextType || reject.Text.Type != slackapi.PlainTextType || status.Text.Type != slackapi.PlainTextType || status.Text.Text != "Ver estado" {
		t.Fatalf("button text types = %q, %q, %q", approve.Text.Type, reject.Text.Type, status.Text.Type)
	}
}

func TestConfirmationV2BoundsLongSummaryInsideCardBody(t *testing.T) {
	delivery := port.ConfirmationDelivery{
		WrapperCallID:  "wrapper-long-summary",
		OriginalCallID: "call-long-summary",
		Summary:        strings.Repeat("summary ", 80),
		Payload:        `{"project":"local-agent","task":"Review the repository README and report all findings."}`,
		Expiry:         time.Date(2026, 8, 28, 15, 30, 0, 0, time.UTC),
	}
	_, blocks, err := compileConfirmationMessage(mustEmbeddedRenderer(t), delivery)
	if err != nil {
		t.Fatal(err)
	}
	card := blocks[0].(*slackapi.CardBlock)
	if card.Title == nil || utf8.RuneCountInString(card.Title.Text) > maxRendererCardTitleLength {
		t.Fatalf("card title = %#v", card.Title)
	}
	if card.Body == nil || utf8.RuneCountInString(card.Body.Text) != maxRendererCardBodyLength || !strings.HasSuffix(card.Body.Text, "...") {
		t.Fatalf("card body = %#v", card.Body)
	}
	if task := blocks[1].(*slackapi.SectionBlock).Text.Text; !strings.Contains(task, "Review the repository README") {
		t.Fatalf("proposed task = %q", task)
	}
}

func TestConfirmationV2RendersStructStyleTaskDescription(t *testing.T) {
	delivery := port.ConfirmationDelivery{
		WrapperCallID: "wrapper-task",
		Summary:       "Approve task",
		Payload:       `{"project":"repo","task":{"ID":"task-1","Project":"repo","Description":"Inspect the repository"}}`,
		Expiry:        time.Date(2026, 7, 21, 15, 30, 0, 0, time.UTC),
	}
	_, blocks, err := compileConfirmationMessage(mustEmbeddedRenderer(t), delivery)
	if err != nil {
		t.Fatal(err)
	}
	task := blocks[1].(*slackapi.SectionBlock)
	if task.Text.Text != "Proposed task:\nInspect the repository" {
		t.Fatalf("task text = %q", task.Text.Text)
	}
}

func TestConfirmationV2UsesUnicodeAndExactSlackLimits(t *testing.T) {
	renderer := mustEmbeddedRenderer(t)
	tests := []struct {
		name        string
		valueKey    string
		limit       int
		wantAtLimit bool
	}{
		{name: "fallback", valueKey: "fallback_text", limit: maxFallbackText, wantAtLimit: true},
		{name: "card body", valueKey: "card_summary", limit: maxRendererCardBodyLength, wantAtLimit: true},
		{name: "card subtitle", valueKey: "subtitle", limit: maxRendererCardSubtitleLength, wantAtLimit: true},
		{name: "section field", valueKey: "project", limit: maxRendererSectionFieldLength, wantAtLimit: true},
		{name: "section text", valueKey: "proposed_task", limit: maxRendererCompositionTextLength, wantAtLimit: true},
		{name: "button value", valueKey: "wrapper_call_id", limit: maxRendererOptionValueLength, wantAtLimit: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			context := confirmationV2TemplateContext()
			context.Values[test.valueKey] = strings.Repeat("界", test.limit)
			if _, _, err := renderer.CompileMessageWithFallback(confirmationTemplateV2, context); (err == nil) != test.wantAtLimit {
				t.Fatalf("at-limit compile error = %v", err)
			}
			context.Values[test.valueKey] += "界"
			if _, _, err := renderer.CompileMessageWithFallback(confirmationTemplateV2, context); err == nil {
				t.Fatal("limit+1 value was accepted")
			}
		})
	}
}

func TestConfirmationTemplateValidatesButtonTextAndURL(t *testing.T) {
	files := embeddedTemplateFiles(t)
	replaceMessage(
		files,
		confirmationTemplateV2,
		`"text": {"type": "plain_text", "text": "Approve", "emoji": false}`,
		`"text": {"type": "plain_text", "text": "`+strings.Repeat("x", maxRendererButtonTextLength)+`", "emoji": false}`,
	)
	if _, err := LoadTemplateCatalogFromFS(templateMapFS(files)); err != nil {
		t.Fatalf("button text at limit rejected: %v", err)
	}

	files = embeddedTemplateFiles(t)
	replaceMessage(files, confirmationTemplateV2, `"style": "primary"`, `"url": "javascript:alert(1)", "style": "primary"`)
	if _, err := LoadTemplateCatalogFromFS(templateMapFS(files)); err == nil {
		t.Fatal("javascript button URL was accepted")
	}
}

func confirmationV2TemplateContext() TemplateContext {
	return TemplateContext{Values: map[string]string{
		"card_summary":    "Summary",
		"subtitle":        "*Call ID:*\n`call-1` · *Expires:* 15:04 UTC",
		"project":         "Project: repo",
		"proposed_task":   "Proposed task:\nInspect the repository",
		"wrapper_call_id": "wrapper-1",
		"fallback_text":   "Confirmation required: Summary",
	}}
}

func TestConfirmationMetadata(t *testing.T) {
	t.Parallel()
	delivery := port.ConfirmationDelivery{
		WrapperCallID: "wrapper-abc", OriginalCallID: "orig-abc",
		Summary: "Write file", CorrelationID: "confirmation:wrapper-abc",
		Expiry: time.Date(2026, 7, 21, 15, 30, 0, 0, time.UTC),
	}
	metadata := confirmationMetadata(delivery)
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
	d1 := confirmationContentDigest(delivery)
	d2 := confirmationContentDigest(delivery)
	if d1 != d2 {
		t.Errorf("digest not deterministic: %q vs %q", d1, d2)
	}
	if d1 == "" {
		t.Error("digest is empty")
	}
}

func TestConfirmationContentDigestPreservesLegacyV1(t *testing.T) {
	delivery := port.ConfirmationDelivery{
		WrapperCallID: "wrapper-abc", OriginalCallID: "orig-abc", Actor: "U12345678",
		TeamID: "T12345678", ChannelID: "D12345678", ThreadTS: "1710000000.000000",
		Summary: "Write file", ParameterHash: "abc123",
		RendererMode: confirmationRenderModeV1,
		Expiry:       time.Date(2026, 7, 21, 15, 30, 0, 0, time.UTC),
	}
	const legacyV1 = "2f36fd991b846946e691510ec93c1356e769b5088f7f97731c25d5102d39c2b7"
	if got := confirmationContentDigest(delivery); got != legacyV1 {
		t.Fatalf("legacy digest = %q, want %q", got, legacyV1)
	}
	delivery.Payload = `{"action":"cancel_workstream"}`
	if got := confirmationContentDigest(delivery); got == legacyV1 {
		t.Fatal("payload-bearing confirmation reused legacy digest")
	}
}

func TestConfirmationContentDigestV2BindsPresentationContract(t *testing.T) {
	delivery := port.ConfirmationDelivery{
		WrapperCallID: "wrapper-abc", OriginalCallID: "orig-abc", Actor: "U12345678",
		TeamID: "T12345678", ChannelID: "D12345678", ThreadTS: "1710000000.000000",
		Summary: "Write file", ParameterHash: "abc123", RendererMode: confirmationRenderModeV2,
		Expiry: time.Date(2026, 7, 21, 15, 30, 0, 0, time.UTC),
	}
	v2Digest := port.ConfirmationContentDigestV2(delivery)
	if v2Digest != port.ConfirmationContentDigest(delivery) {
		t.Fatalf("v2 digest dispatch = %q, want %q", port.ConfirmationContentDigest(delivery), v2Digest)
	}
	v1 := delivery
	v1.RendererMode = confirmationRenderModeV1
	if v2Digest == port.ConfirmationContentDigest(v1) {
		t.Fatal("v2 digest reused the v1 digest")
	}
	withPayload := delivery
	withPayload.Payload = `{"project":"repo","task":"Inspect the repository"}`
	if v2Digest == port.ConfirmationContentDigestV2(withPayload) {
		t.Fatal("v2 digest omitted display payload data")
	}
}

func TestConfirmationPayloadPrecedesActionsAndAppearsInFallback(t *testing.T) {
	delivery := port.ConfirmationDelivery{
		WrapperCallID: "wrapper-abc", OriginalCallID: "orig-abc", Summary: "Cancel workstream",
		Payload: strings.Repeat("x", 3000), Expiry: time.Date(2026, 7, 21, 15, 30, 0, 0, time.UTC),
	}
	renderer, err := NewEmbeddedTemplateRenderer()
	if err != nil {
		t.Fatal(err)
	}
	fallback, blocks, err := compileConfirmationMessageV1(renderer, delivery)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fallback, "complete proposed payload is shown") {
		t.Fatalf("oversized fallback did not identify complete payload blocks: %q", fallback)
	}
	if len(blocks) != 4 {
		t.Fatalf("blocks = %d, want two payload blocks plus confirmation blocks", len(blocks))
	}
	for index := range 2 {
		section, ok := blocks[index].(*slackapi.SectionBlock)
		if !ok || section.Text == nil || section.Text.Type != slackapi.PlainTextType || !strings.HasPrefix(section.Text.Text, "Proposed payload:\n") {
			t.Fatalf("payload block %d = %#v", index, blocks[index])
		}
	}
	if _, ok := blocks[len(blocks)-1].(*slackapi.ActionBlock); !ok {
		t.Fatalf("last block = %#v, want actions after payload", blocks[len(blocks)-1])
	}
	delivery.Payload = `{"action":"cancel_workstream"}`
	if fallback := confirmationFallbackTextV1(delivery); !strings.Contains(fallback, delivery.Payload) {
		t.Fatal("bounded accessible fallback omitted confirmation payload")
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
	mu             sync.Mutex
	postedChans    []string
	postedBlocks   [][]slackapi.Block
	updatedChans   []string
	updatedTS      []string
	updatedBlocks  [][]slackapi.Block
	fallbackTexts  []string
	postedMetadata []slackapi.SlackMetadata
	postedThreads  []string
	messages       []slackapi.Message
	hasMore        bool
	postErr        error
	updateErr      error
	historyErr     error
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

func (c *fakeConfirmationBlockClient) UpdateBlocks(_ context.Context, channelID, messageTS string, blocks []slackapi.Block, _ string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.updateErr != nil {
		return c.updateErr
	}
	c.updatedChans = append(c.updatedChans, channelID)
	c.updatedTS = append(c.updatedTS, messageTS)
	c.updatedBlocks = append(c.updatedBlocks, blocks)
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
	if len(client.fallbackTexts) != 1 || client.fallbackTexts[0] != confirmationFallbackText(delivery) {
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
		client.postedMetadata[0].EventPayload["content_sha256"] != confirmationContentDigest(delivery) {
		t.Fatalf("posted metadata payload = %#v", client.postedMetadata[0].EventPayload)
	}
	blocks := client.postedBlocks[0]
	if len(blocks) != 3 {
		t.Fatalf("confirmation blocks = %d, want 3", len(blocks))
	}
	card := blocks[0].(*slackapi.CardBlock)
	if card.Title == nil || card.Title.Type != slackapi.PlainTextType || card.Title.Text != "Confirmation required" {
		t.Fatalf("confirmation card title = %#v", card.Title)
	}
	if card.Subtitle == nil || card.Subtitle.Text != "*Call ID:*\n`orig-abc` · *Expires:* 15:30 UTC" {
		t.Fatalf("confirmation card subtitle = %#v", card.Subtitle)
	}
	if card.Body == nil || card.Body.Text != "Write file" {
		t.Fatalf("confirmation card body = %#v", card.Body)
	}
	projectTask := blocks[1].(*slackapi.SectionBlock)
	if projectTask.Text == nil || projectTask.Text.Type != slackapi.PlainTextType || projectTask.Text.Text != "Proposed task: not provided" || len(projectTask.Fields) != 1 ||
		projectTask.Fields[0].Type != slackapi.PlainTextType ||
		projectTask.Fields[0].Text != "Project: not provided" {
		t.Fatalf("confirmation project and task = %#v", projectTask)
	}
	actions := blocks[2].(*slackapi.ActionBlock)
	if actions.BlockID != "confirmation_buttons" || len(actions.Elements.ElementSet) != 3 {
		t.Fatalf("confirmation actions = %#v", actions)
	}
	approve := actions.Elements.ElementSet[0].(*slackapi.ButtonBlockElement)
	reject := actions.Elements.ElementSet[1].(*slackapi.ButtonBlockElement)
	status := actions.Elements.ElementSet[2].(*slackapi.ButtonBlockElement)
	if approve.ActionID != approveActionID || approve.Value != delivery.WrapperCallID || approve.Style != slackapi.StylePrimary || approve.Text.Text != "Approve" {
		t.Fatalf("approve button = %#v", approve)
	}
	if reject.ActionID != rejectActionID || reject.Value != delivery.WrapperCallID || reject.Style != slackapi.StyleDanger || reject.Text.Text != "Reject" {
		t.Fatalf("reject button = %#v", reject)
	}
	if status.ActionID != statusActionID || status.Value != delivery.WrapperCallID || status.Text.Text != "Ver estado" {
		t.Fatalf("status button = %#v", status)
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
	if got := client.updatedBlocks[0][0].(*slackapi.SectionBlock).Text.Text; strings.Contains(got, ":white_check_mark:") || !strings.HasPrefix(got, "*Confirmation approved*") {
		t.Fatalf("updated title = %q", got)
	}
	if got := client.updatedBlocks[0][2].(*slackapi.SectionBlock).Text; got.Type != slackapi.PlainTextType || got.Text != "Confirmation result:\ndone" {
		t.Fatalf("updated result = %#v", got)
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
		User: "U99999999", Timestamp: "1720000001.000001", Metadata: confirmationMetadata(delivery),
	}}}
	pub := newConfirmationPublisher(client, "U99999999", 5*time.Second, nil)

	result, found, err := pub.RecoverConfirmation(t.Context(), delivery)
	if err != nil || !found || result.SlackMessageTS != "1720000001.000001" {
		t.Fatalf("RecoverConfirmation() = %#v, %t, %v", result, found, err)
	}
}

func TestConfirmationPublisherRecoversPendingV1Prompt(t *testing.T) {
	t.Parallel()
	delivery := port.ConfirmationDelivery{
		WrapperCallID: "wrapper-v1", OriginalCallID: "orig-v1", Actor: "U12345678",
		TeamID: "T12345678", ChannelID: "C12345678", ThreadTS: "1720000000.000000",
		Summary: "Write file", ParameterHash: "abc123", CorrelationID: "confirmation:wrapper-v1",
		RendererMode: confirmationRenderModeV1, Expiry: time.Date(2026, 7, 21, 15, 30, 0, 0, time.UTC),
	}
	client := &fakeConfirmationBlockClient{messages: []slackapi.Message{{
		User: "U99999999", Timestamp: "1720000001.000001", Metadata: confirmationMetadata(delivery),
	}}}
	pub := newConfirmationPublisher(client, "U99999999", 5*time.Second, nil)
	result, found, err := pub.RecoverConfirmation(t.Context(), delivery)
	if err != nil || !found || result.SlackMessageTS != "1720000001.000001" {
		t.Fatalf("RecoverConfirmation(v1) = %#v, %t, %v", result, found, err)
	}
	_, blocks, err := compileConfirmationMessageForMode(mustEmbeddedRenderer(t), delivery, confirmationRenderModeV1)
	if err != nil || len(blocks) != 2 {
		t.Fatalf("v1 renderer compatibility = blocks:%d err:%v", len(blocks), err)
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
	metadata := confirmationMetadata(delivery)
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
		User: "U99999999", Timestamp: "1720000001.000001", Metadata: confirmationMetadata(delivery),
	}}
	if _, _, err := pub.RecoverConfirmation(t.Context(), delivery); err == nil {
		t.Fatal("RecoverConfirmation accepted a match from incomplete history")
	}
}
