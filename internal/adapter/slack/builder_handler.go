package slack

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Dauno/slack-local-agent/internal/agentdef"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	slackapi "github.com/slack-go/slack"
)

const (
	builderSubmitCallbackID = "local_agent.builder.submit"
	builderInstallActionID  = "local_agent.builder.request_install"
	builderDraftTTL         = 24 * time.Hour
	builderBlockTextLimit   = 2900
)

// BuilderSubmissionHandler processes view_submission from the agent builder modal.
type BuilderSubmissionHandler struct {
	draftStore      port.AgentDraftStore
	agentBuilder    port.AgentBuilderService
	currentDefs     *agentdef.Definitions
	conversationKey string
	publisher       port.ResponsePublisher
	now             func() time.Time
}

func NewBuilderSubmissionHandler(
	draftStore port.AgentDraftStore,
	agentBuilder port.AgentBuilderService,
	currentDefs *agentdef.Definitions,
	publisher port.ResponsePublisher,
) *BuilderSubmissionHandler {
	return &BuilderSubmissionHandler{
		draftStore: draftStore, agentBuilder: agentBuilder,
		currentDefs: currentDefs, publisher: publisher, now: time.Now,
	}
}

// WithConversationKey supplies a fallback conversation for Slack payloads that
// omit channel information from a view_submission callback.
func (h *BuilderSubmissionHandler) WithConversationKey(key string) *BuilderSubmissionHandler {
	if h != nil {
		h.conversationKey = key
	}
	return h
}

// HandleSubmission validates only the fields needed for the synchronous ACK.
// Previewing and publishing are deliberately performed by PreviewAndPublish.
func (h *BuilderSubmissionHandler) HandleSubmission(_ context.Context, callback slackapi.InteractionCallback) *slackapi.ViewSubmissionResponse {
	draft, missing := builderDraftFromCallback(callback)
	if missing != "" {
		return slackapi.NewErrorsViewSubmissionResponse(map[string]string{missing: "El campo es obligatorio"})
	}

	if err := agentdef.ValidateAgentName(draft.Name); err != nil {
		return slackapi.NewErrorsViewSubmissionResponse(map[string]string{"name": err.Error()})
	}
	if utf8.RuneCountInString(draft.Description) > agentdef.MaxDescriptionLength {
		return slackapi.NewErrorsViewSubmissionResponse(map[string]string{
			"description": fmt.Sprintf("Maximo %d caracteres", agentdef.MaxDescriptionLength),
		})
	}
	if utf8.RuneCountInString(draft.Instruction) > agentdef.MaxInstructionLength {
		return slackapi.NewErrorsViewSubmissionResponse(map[string]string{
			"instruction": fmt.Sprintf("Maximo %d caracteres", agentdef.MaxInstructionLength),
		})
	}
	return nil
}

// PreviewAndPublish performs the work that must happen after Slack has ACKed
// the submission: compile, persist the immutable preview, and publish it.
func (h *BuilderSubmissionHandler) PreviewAndPublish(ctx context.Context, callback slackapi.InteractionCallback) error {
	if h == nil || h.draftStore == nil || h.agentBuilder == nil || h.publisher == nil {
		return errors.New("builder submission handler is not configured")
	}
	if !domain.PlausibleTeamID(callback.Team.ID) || !domain.PlausibleUserID(callback.User.ID) {
		return errors.New("builder actor or team is invalid")
	}
	draftInput, missing := builderDraftFromCallback(callback)
	if missing != "" {
		return fmt.Errorf("builder field %q is missing", missing)
	}
	preview, err := h.agentBuilder.Preview(draftInput, h.currentDefs)
	if err != nil {
		return fmt.Errorf("preview agent definition: %w", err)
	}
	if preview == nil {
		return errors.New("preview agent definition returned no result")
	}

	conversationKey, target, err := builderConversation(callback, h.conversationKey)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if h.now != nil {
		now = h.now().UTC()
	}
	draftID, err := newBuilderDraftID()
	if err != nil {
		return err
	}
	draft := &port.AgentDraft{
		DraftID:         draftID,
		TeamID:          callback.Team.ID,
		ActorID:         callback.User.ID,
		ConversationKey: conversationKey,
		Name:            draftInput.Name,
		Description:     draftInput.Description,
		Instruction:     draftInput.Instruction,
		Model:           draftInput.Model,
		DefinitionHash:  preview.SHA256,
		Status:          port.DraftStatusPreviewed,
		CreatedAt:       now,
		ExpiresAt:       now.Add(builderDraftTTL),
	}
	if err := h.draftStore.Create(ctx, draft); err != nil {
		return fmt.Errorf("persist agent draft: %w", err)
	}

	if publisher, ok := h.publisher.(*Publisher); ok {
		if err := publisher.publishBuilderPreview(ctx, target, draftInput.Name, preview.YAML, preview.SHA256, draftID); err != nil {
			return fmt.Errorf("publish agent preview: %w", err)
		}
		return nil
	}
	text := builderPreviewMarkdown(draftInput.Name, preview.YAML, preview.SHA256)
	if _, err := h.publisher.Publish(ctx, target, text); err != nil {
		return fmt.Errorf("publish agent preview: %w", err)
	}
	return nil
}

// HandleInstallRequest records the button request and tells the user to invoke
// the ADK install tool. Slack actions cannot invoke that tool directly.
func (h *BuilderSubmissionHandler) HandleInstallRequest(ctx context.Context, callback slackapi.InteractionCallback, draftID string) error {
	if h == nil || h.draftStore == nil || h.publisher == nil {
		return errors.New("builder install handler is not configured")
	}
	if !domain.PlausibleTeamID(callback.Team.ID) || !domain.PlausibleUserID(callback.User.ID) {
		return errors.New("builder actor or team is invalid")
	}
	draftID = strings.TrimSpace(draftID)
	if draftID == "" {
		return errors.New("agent draft ID is required")
	}

	draft, err := h.draftStore.Get(ctx, draftID)
	if err != nil {
		return fmt.Errorf("load agent draft: %w", err)
	}
	if draft == nil {
		return fmt.Errorf("agent draft %q was not found", draftID)
	}
	conversationKey, target, err := builderConversation(callback, h.conversationKey)
	if err != nil {
		return err
	}
	if draft.TeamID != callback.Team.ID || draft.ActorID != callback.User.ID || draft.ConversationKey != conversationKey {
		return errors.New("agent draft does not belong to the current actor and conversation")
	}
	now := time.Now().UTC()
	if h.now != nil {
		now = h.now().UTC()
	}
	if !draft.ExpiresAt.After(now) {
		return fmt.Errorf("agent draft %q has expired", draftID)
	}
	if draft.Status != port.DraftStatusPreviewed && draft.Status != port.DraftStatusInstallRequested {
		return fmt.Errorf("agent draft %q is not installable from status %q", draftID, draft.Status)
	}
	if draft.Status == port.DraftStatusPreviewed {
		if err := h.draftStore.UpdateStatus(ctx, draftID, port.DraftStatusPreviewed, port.DraftStatusInstallRequested); err != nil {
			return fmt.Errorf("request agent draft installation: %w", err)
		}
	}

	message := fmt.Sprintf(
		"Se ha solicitado la instalacion del agente `%s` (draft `%s`). Para completar la instalacion, escribe: usa `install_agent_def` con `draft_id` `%s`, `name` `%s` y `definition_hash` `%s`.",
		neutralizeUnsafeControls(draft.Name), neutralizeUnsafeControls(draftID), neutralizeUnsafeControls(draftID), neutralizeUnsafeControls(draft.Name), neutralizeUnsafeControls(draft.DefinitionHash),
	)
	if _, err := h.publisher.Publish(ctx, target, message); err != nil {
		return fmt.Errorf("publish agent install request: %w", err)
	}
	return nil
}

func (h *BuilderSubmissionHandler) publishModalFallback(ctx context.Context, callback slackapi.InteractionCallback) error {
	if h == nil || h.publisher == nil {
		return errors.New("builder publisher is not configured")
	}
	_, target, err := builderConversation(callback, h.conversationKey)
	if err != nil {
		return err
	}
	if _, err := h.publisher.Publish(ctx, target, "No se pudo abrir el formulario para crear un agente. Intenta de nuevo."); err != nil {
		return fmt.Errorf("publish builder modal fallback: %w", err)
	}
	return nil
}

func builderDraftFromCallback(callback slackapi.InteractionCallback) (domain.AgentDraft, string) {
	if callback.View.State == nil {
		return domain.AgentDraft{}, "name"
	}
	values := callback.View.State.Values
	value := func(blockID, actionID string) (string, bool) {
		block, ok := values[blockID]
		if !ok {
			return "", false
		}
		action, ok := block[actionID]
		if !ok {
			return "", false
		}
		return action.Value, true
	}

	name, ok := value("name", "name")
	if !ok || name == "" {
		return domain.AgentDraft{}, "name"
	}
	description, ok := value("description", "description")
	if !ok {
		return domain.AgentDraft{}, "description"
	}
	instruction, ok := value("instruction", "instruction")
	if !ok {
		return domain.AgentDraft{}, "instruction"
	}
	model, _ := value("model", "model")
	if model == "" {
		if block, exists := values["model"]; exists {
			if action, exists := block["model"]; exists {
				model = action.SelectedOption.Value
			}
		}
	}
	return domain.AgentDraft{Name: name, Description: description, Instruction: instruction, Model: model}, ""
}

func builderConversation(callback slackapi.InteractionCallback, fallback string) (string, domain.ReplyTarget, error) {
	teamID := callback.Team.ID
	channelID := callback.Channel.ID
	if channelID == "" {
		channelID = callback.Container.ChannelID
	}
	threadTS := callback.Container.ThreadTs
	if threadTS == "" {
		threadTS = callback.Message.ThreadTimestamp
	}

	if teamID != "" && channelID != "" && domain.PlausibleTeamID(teamID) && domain.PlausibleChannelID(channelID) {
		if channelID[0] == 'D' {
			key := domain.ConversationKey(fmt.Sprintf("slack:%s:dm:%s", teamID, channelID))
			if threadTS != "" {
				key = domain.ConversationKey(fmt.Sprintf("slack:%s:dm:%s:thread:%s", teamID, channelID, threadTS))
			}
			return string(key), domain.ReplyTarget{ChannelID: channelID, ThreadTS: threadTS}, nil
		}
		if threadTS == "" {
			threadTS = callback.Message.Timestamp
		}
		if threadTS == "" {
			threadTS = callback.Container.MessageTs
		}
		if threadTS == "" {
			return "", domain.ReplyTarget{}, errors.New("builder conversation thread is required")
		}
		key := domain.ConversationKey(fmt.Sprintf("slack:%s:channel:%s:thread:%s", teamID, channelID, threadTS))
		return string(key), domain.ReplyTarget{ChannelID: channelID, ThreadTS: threadTS}, nil
	}

	if strings.TrimSpace(fallback) == "" {
		return "", domain.ReplyTarget{}, errors.New("builder conversation is required")
	}
	target, err := domain.ConversationReplyTarget(domain.ConversationKey(fallback))
	if err != nil {
		return "", domain.ReplyTarget{}, fmt.Errorf("resolve builder conversation: %w", err)
	}
	if !domain.PlausibleChannelID(target.ChannelID) {
		return "", domain.ReplyTarget{}, errors.New("builder conversation channel is invalid")
	}
	return fallback, target, nil
}

func newBuilderDraftID() (string, error) {
	data := make([]byte, 12)
	if _, err := rand.Read(data); err != nil {
		return "", fmt.Errorf("generate agent draft ID: %w", err)
	}
	return "draft_" + hex.EncodeToString(data), nil
}

func builderPreviewMarkdown(name, yaml, sha256 string) string {
	return fmt.Sprintf("*Previsualizacion del agente `%s`*\n\n```yaml\n%s\n```\n\n*SHA-256:* `%s`\n\nSolicitar instalación con el botón del preview.",
		neutralizeUnsafeControls(name), neutralizeUnsafeControls(yaml), sha256)
}

type builderBlockPoster interface {
	PostBlocks(context.Context, string, string, []slackapi.Block, slackapi.SlackMetadata, string) (string, error)
}

func (c sdkPostClient) PostBlocks(ctx context.Context, channelID, fallbackText string, blocks []slackapi.Block, metadata slackapi.SlackMetadata, threadTS string) (string, error) {
	options := []slackapi.MsgOption{
		slackapi.MsgOptionText(fallbackText, false),
		slackapi.MsgOptionBlocks(blocks...),
		slackapi.MsgOptionDisableLinkUnfurl(),
		slackapi.MsgOptionDisableMediaUnfurl(),
	}
	if threadTS != "" {
		options = append(options, slackapi.MsgOptionTS(threadTS))
	}
	if metadata.EventType != "" {
		options = append(options, slackapi.MsgOptionMetadata(metadata))
	}
	_, timestamp, err := c.client.PostMessageContext(ctx, channelID, options...)
	return timestamp, err
}

func (p *Publisher) publishBuilderPreview(ctx context.Context, target domain.ReplyTarget, name, yaml, sha256, draftID string) error {
	if p == nil || p.client == nil {
		return errors.New("Slack posting client is required")
	}
	poster, ok := p.client.(builderBlockPoster)
	if !ok {
		_, err := p.Publish(ctx, target, builderPreviewMarkdown(name, yaml, sha256))
		return err
	}
	blocks := renderBuilderPreviewBlocks(name, yaml, sha256, draftID)
	callCtx := ctx
	cancel := func() {}
	if p.timeout > 0 {
		callCtx, cancel = context.WithTimeout(ctx, p.timeout)
	}
	defer cancel()
	timestamp, err := poster.PostBlocks(callCtx, target.ChannelID, builderPreviewMarkdown(name, yaml, sha256), blocks, slackapi.SlackMetadata{}, target.ThreadTS)
	if err != nil {
		return err
	}
	if timestamp == "" {
		return errors.New("Slack published agent preview without a message timestamp")
	}
	return nil
}

func renderBuilderPreviewBlocks(name, yaml, sha256, draftID string) []slackapi.Block {
	blocks := []slackapi.Block{
		slackapi.NewSectionBlock(
			slackapi.NewTextBlockObject("mrkdwn", fmt.Sprintf("*Previsualizacion del agente `%s`*", neutralizeUnsafeControls(name)), false, false),
			nil,
			nil,
		),
	}
	code := "```yaml\n" + neutralizeUnsafeControls(yaml) + "\n```"
	for _, part := range splitBuilderBlockText(code, builderBlockTextLimit) {
		blocks = append(blocks, slackapi.NewSectionBlock(slackapi.NewTextBlockObject("mrkdwn", part, false, false), nil, nil))
	}
	blocks = append(blocks,
		slackapi.NewSectionBlock(slackapi.NewTextBlockObject("mrkdwn", fmt.Sprintf("*SHA-256:* `%s`", sha256), false, false), nil, nil),
		slackapi.NewActionBlock("builder_preview_actions",
			slackapi.NewButtonBlockElement(builderInstallActionID, draftID, slackapi.NewTextBlockObject("plain_text", "Solicitar instalación", false, false)),
		),
	)
	return blocks
}

func splitBuilderBlockText(text string, maxRunes int) []string {
	if maxRunes <= 0 || utf8.RuneCountInString(text) <= maxRunes {
		return []string{text}
	}
	runes := []rune(text)
	parts := make([]string, 0, (len(runes)+maxRunes-1)/maxRunes)
	for len(runes) > 0 {
		n := min(len(runes), maxRunes)
		parts = append(parts, string(runes[:n]))
		runes = runes[n:]
	}
	return parts
}
