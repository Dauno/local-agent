package slack

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	slackapi "github.com/slack-go/slack"

	"github.com/Dauno/slack-local-agent/internal/agentdef"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

const (
	builderSubmitCallbackID = "local_agent.builder.submit"
	builderInstallActionID  = "local_agent.builder.request_install"
	builderDraftTTL         = 24 * time.Hour
	builderBlockTextLimit   = 2900
)

// BuilderSubmissionHandler processes view_submission from the agent builder modal.
type BuilderSubmissionHandler struct {
	draftStore   port.AgentDraftStore
	agentBuilder port.AgentBuilderService
	currentDefs  *agentdef.Definitions
	publisher    port.ResponsePublisher
	now          func() time.Time
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
	if err := validateBuilderDraft(callback, draft); err != nil {
		return slackapi.NewErrorsViewSubmissionResponse(map[string]string{builderValidationField(err): err.Error()})
	}
	if h == nil || h.agentBuilder == nil {
		return slackapi.NewErrorsViewSubmissionResponse(map[string]string{"model": "El catalogo de proveedores no esta disponible"})
	}
	if _, err := h.agentBuilder.Preview(draft, h.currentDefs); err != nil {
		return slackapi.NewErrorsViewSubmissionResponse(map[string]string{builderValidationField(err): err.Error()})
	}
	if _, _, err := builderContextForSubmission(callback); err != nil {
		return slackapi.NewErrorsViewSubmissionResponse(map[string]string{"name": err.Error()})
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
	conversationKey, target, err := builderContextForSubmission(callback)
	if err != nil {
		return err
	}
	preview, err := h.agentBuilder.Preview(draftInput, h.currentDefs)
	if err != nil {
		return fmt.Errorf("preview agent definition: %w", err)
	}
	if preview == nil {
		return errors.New("preview agent definition returned no result")
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
		Kind:            string(draftInput.Kind),
		ExecutionMode:   preview.AgentDef.ExecutionMode,
		TimeoutSeconds:  preview.AgentDef.TimeoutSec,
		CanonicalYAML:   preview.YAML,
		DefinitionHash:  preview.SHA256,
		Status:          port.DraftStatusPreviewed,
		CreatedAt:       now,
		ExpiresAt:       now.Add(builderDraftTTL),
	}
	if err := h.draftStore.Create(ctx, draft); err != nil {
		return fmt.Errorf("persist agent draft: %w", err)
	}

	if publisher, ok := h.publisher.(*Publisher); ok {
		if err := publisher.publishBuilderPreview(ctx, target, draftInput, preview.AgentDef, preview.YAML, preview.SHA256, draftID); err != nil {
			return fmt.Errorf("publish agent preview: %w", err)
		}
		return nil
	}
	renderer, err := NewEmbeddedTemplateRenderer()
	if err != nil {
		return fmt.Errorf("initialize agent preview template renderer: %w", err)
	}
	text, _, err := compileBuilderPreviewMessage(renderer, draftInput, preview.AgentDef, preview.YAML, preview.SHA256, draftID)
	if err != nil {
		return fmt.Errorf("render agent preview template: %w", err)
	}
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
	if draft.TeamID != callback.Team.ID || draft.ActorID != callback.User.ID {
		return errors.New("agent draft does not belong to the current actor and conversation")
	}
	parts := strings.Split(draft.ConversationKey, ":")
	if len(parts) < 4 || parts[1] != callback.Team.ID {
		return errors.New("agent draft does not belong to the current actor and conversation")
	}
	target, err := domain.ConversationReplyTarget(domain.ConversationKey(draft.ConversationKey))
	if err != nil {
		return err
	}
	if _, err := encodeBuilderInteractionContext(draft.ActorID, domain.ConversationKey(draft.ConversationKey)); err != nil {
		return errors.New("agent draft conversation is invalid")
	}
	if !domain.PlausibleChannelID(target.ChannelID) {
		return errors.New("agent draft does not belong to the current actor and conversation")
	}
	if err := callbackTargetMatches(callback, target); err != nil {
		return err
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
	_, target, err := builderConversation(callback, "")
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
	kind, _ := value("agent_type", "agent_type")
	if kind == "" {
		if block, exists := values["agent_type"]; exists {
			if action, exists := block["agent_type"]; exists {
				kind = action.SelectedOption.Value
			}
		}
	}
	if kind == "" {
		kind = string(domain.AgentKindLLM)
	}
	providerProfile := selectedValue(values, "provider_profile", "provider_profile")
	if providerProfile == "" {
		providerProfile = selectedValue(values, "model", "model")
	}
	model := ""
	if kind == string(domain.AgentKindLLM) {
		model = selectedValue(values, "model", "model")
	}
	executionMode := selectedValue(values, "execution_mode", "execution_mode")
	timeoutSeconds := 0
	if timeoutText := selectedValue(values, "timeout_seconds", "timeout_seconds"); timeoutText != "" {
		timeoutSeconds, _ = strconv.Atoi(timeoutText)
	}
	return domain.AgentDraft{
		Name:            name,
		Description:     description,
		Instruction:     instruction,
		Model:           model,
		Kind:            domain.AgentKind(kind),
		ProviderProfile: providerProfile,
		ExecutionMode:   executionMode,
		TimeoutSeconds:  timeoutSeconds,
	}, ""
}

func selectedValue(values map[string]map[string]slackapi.BlockAction, blockID, actionID string) string {
	if block, ok := values[blockID]; ok {
		if action, ok := block[actionID]; ok {
			if action.Value != "" {
				return action.Value
			}
			return action.SelectedOption.Value
		}
	}
	return ""
}

func validateBuilderDraft(callback slackapi.InteractionCallback, draft domain.AgentDraft) error {
	if err := domain.ValidateAgentKind(draft.Kind); err != nil {
		return err
	}
	if draft.ProviderProfile == "" {
		return errors.New("selecciona un proveedor/perfil")
	}
	if draft.Kind == domain.AgentKindLLM {
		if draft.ExecutionMode != "" && draft.ExecutionMode != domain.ExecutionModeForeground {
			return errors.New("execution_mode solo admite foreground para LLM")
		}
		if draft.TimeoutSeconds != 0 {
			return errors.New("timeout_seconds solo es valido para ACP")
		}
		return nil
	}
	mode := draft.ExecutionMode
	if mode == "" {
		mode = domain.ExecutionModeForeground
	}
	if err := domain.ValidateExecutionMode(draft.Kind, mode); err != nil {
		return err
	}
	if err := domain.ValidateExternalAgentTimeout(draft.TimeoutSeconds); err != nil {
		return err
	}
	// Parse the raw value so malformed numeric input is not silently treated as zero.
	if callback.View.State != nil {
		if raw := selectedValue(callback.View.State.Values, "timeout_seconds", "timeout_seconds"); raw != "" {
			if _, err := strconv.Atoi(raw); err != nil {
				return errors.New("timeout_seconds debe ser un numero entero")
			}
		}
	}
	return nil
}

func builderValidationField(err error) string {
	message := err.Error()
	if strings.Contains(message, "timeout") {
		return "timeout_seconds"
	}
	if strings.Contains(message, "execution") {
		return "execution_mode"
	}
	if strings.Contains(message, "proveedor") || strings.Contains(message, "provider") || strings.Contains(message, "perfil") {
		return "model"
	}
	if strings.Contains(message, "kind") || strings.Contains(message, "tipo") {
		return "agent_type"
	}
	return "model"
}

func builderConversation(callback slackapi.InteractionCallback, fallback string) (string, domain.ReplyTarget, error) {
	teamID := callback.Team.ID
	channelID := callback.Channel.ID
	if channelID == "" {
		channelID = callback.Container.ChannelID
	}
	threadTS := callback.Container.ThreadTs

	if teamID != "" && channelID != "" && domain.PlausibleTeamID(teamID) && domain.PlausibleChannelID(channelID) {
		if channelID[0] == 'D' {
			key := domain.ConversationKey(fmt.Sprintf("slack:%s:dm:%s", teamID, channelID))
			if threadTS != "" {
				key = domain.ConversationKey(fmt.Sprintf("slack:%s:dm:%s:thread:%s", teamID, channelID, threadTS))
			}
			return string(key), domain.ReplyTarget{ChannelID: channelID, ThreadTS: threadTS}, nil
		}
		if threadTS == "" {
			return "", domain.ReplyTarget{}, errors.New("builder conversation thread is required")
		}
		kind := "channel"
		if channelID[0] == 'G' {
			kind = "group"
		}
		key := domain.ConversationKey(fmt.Sprintf("slack:%s:%s:%s:thread:%s", teamID, kind, channelID, threadTS))
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

func builderPreviewMarkdown(draft domain.AgentDraft, definition port.AgentDefPreview, yaml, sha256 string) string {
	profile := draft.ProviderProfile
	if profile == "" {
		profile = draft.Model
	}
	timeout := "no aplica"
	if definition.TimeoutSec > 0 {
		timeout = strconv.Itoa(definition.TimeoutSec) + " segundos"
	}
	return fmt.Sprintf("*Previsualizacion del agente `%s`*\n\n*Clase:* `%s`\n*Runtime/perfil:* `%s`\n*Ejecucion:* `%s`\n*Timeout:* `%s`\n\n```yaml\n%s\n```\n\n*SHA-256:* `%s`\n\nSolicitar instalación con el botón del preview.",
		neutralizeUnsafeControls(draft.Name), definition.AgentClass, neutralizeUnsafeControls(profile), definition.ExecutionMode, timeout, neutralizeUnsafeControls(yaml), sha256)
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

func (p *Publisher) publishBuilderPreview(ctx context.Context, target domain.ReplyTarget, draft domain.AgentDraft, definition port.AgentDefPreview, yaml, sha256, draftID string) error {
	if p == nil || p.client == nil {
		return errors.New("slack posting client is required")
	}
	renderer, err := NewEmbeddedTemplateRenderer()
	if err != nil {
		return fmt.Errorf("initialize agent preview template renderer: %w", err)
	}
	fallbackText, blocks, err := compileBuilderPreviewMessage(renderer, draft, definition, yaml, sha256, draftID)
	if err != nil {
		return fmt.Errorf("render agent preview template: %w", err)
	}
	poster, ok := p.client.(blockPostClient)
	if !ok {
		_, err := p.Publish(ctx, target, fallbackText)
		return err
	}
	callCtx, cancel := slackTimeout(ctx, p.timeout)
	defer cancel()
	timestamp, err := poster.PostBlocks(callCtx, target.ChannelID, fallbackText, blocks, slackapi.SlackMetadata{}, target.ThreadTS)
	if err != nil {
		return err
	}
	if timestamp == "" {
		return errors.New("slack published agent preview without a message timestamp")
	}
	return nil
}

func compileBuilderPreviewMessage(renderer *TemplateRenderer, draft domain.AgentDraft, definition port.AgentDefPreview, yaml, sha256, draftID string) (string, []slackapi.Block, error) {
	code := "```yaml\n" + neutralizeUnsafeControls(yaml) + "\n```"
	values := builderPreviewTemplateValues(draft, definition, yaml, sha256, draftID)
	parts := splitBuilderBlockText(code, builderBlockTextLimit)
	return compileMessageWithParts(renderer, "agent_preview", values, parts)
}

func compileMessageWithParts(renderer *TemplateRenderer, templateName string, values map[string]string, parts []string) (string, []slackapi.Block, error) {
	return renderer.CompileMessageWithFallback(templateName, TemplateContext{Values: values, PreviewYAMLParts: parts})
}

func builderPreviewTemplateValues(draft domain.AgentDraft, definition port.AgentDefPreview, yaml, sha256, draftID string) map[string]string {
	metadata := fmt.Sprintf("*Clase:* `%s`\n*Runtime/perfil:* `%s`\n*Ejecucion:* `%s`\n*Timeout:* `%s`", definition.AgentClass, neutralizeUnsafeControls(draft.ProviderProfile), definition.ExecutionMode, previewTimeout(definition.TimeoutSec))
	return map[string]string{
		"name":             fmt.Sprintf("*Previsualizacion del agente `%s`*", neutralizeUnsafeControls(draft.Name)),
		"agent_class":      metadata,
		"provider_profile": neutralizeUnsafeControls(draft.ProviderProfile),
		"execution_mode":   definition.ExecutionMode,
		"timeout":          previewTimeout(definition.TimeoutSec),
		"sha256":           fmt.Sprintf("*SHA-256:* `%s`", sha256),
		"draft_id":         draftID,
		"fallback_text":    builderPreviewFallbackText(draft, definition, yaml, sha256),
	}
}

func builderPreviewFallbackText(draft domain.AgentDraft, definition port.AgentDefPreview, yaml, sha256 string) string {
	full := builderPreviewMarkdown(draft, definition, yaml, sha256)
	if utf8.RuneCountInString(full) <= maxFallbackText {
		return full
	}
	profile := draft.ProviderProfile
	if profile == "" {
		profile = draft.Model
	}
	return fmt.Sprintf("*Previsualizacion del agente `%s`*\n\n*Clase:* `%s`\n*Runtime/perfil:* `%s`\n*Ejecucion:* `%s`\n*Timeout:* `%s`\n\nEl YAML completo se muestra en los bloques del mensaje.\n\n*SHA-256:* `%s`\n\nSolicitar instalación con el botón del preview.",
		neutralizeUnsafeControls(draft.Name), definition.AgentClass, neutralizeUnsafeControls(profile), definition.ExecutionMode, previewTimeout(definition.TimeoutSec), sha256)
}

func previewTimeout(seconds int) string {
	if seconds <= 0 {
		return "no aplica"
	}
	return strconv.Itoa(seconds) + " segundos"
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
