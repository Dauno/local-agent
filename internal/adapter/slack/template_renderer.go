package slack

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	slackapi "github.com/slack-go/slack"

	"github.com/Dauno/slack-local-agent/internal/agentdef"
	"github.com/Dauno/slack-local-agent/internal/domain"
)

const (
	maxRendererBlocksPerModal         = 100
	maxRendererCardTitleLength        = 150
	maxRendererCardSubtitleLength     = 150
	maxRendererCardBodyLength         = 200
	maxRendererCardSubtextLength      = 200
	maxRendererCardActions            = 3
	maxRendererCompositionTextLength  = 3000
	maxRendererModalTitleLength       = 24
	maxRendererModalSubmitCloseLength = 24
	maxRendererPlainTextInputLength   = 3000
	maxRendererSectionFields          = 10
	maxRendererSectionFieldLength     = 2000
	maxRendererButtonTextLength       = 75
	maxRendererOptionTextLength       = 75
	maxRendererOptionValueLength      = 2000
	maxRendererIDLength               = 255
)

// TemplateContext contains trusted values prepared by the owning Slack
// handler. Values are scalars only; provider profiles are converted to options
// by the renderer after applying the builder's existing kind policy.
type TemplateContext struct {
	Values           map[string]string
	Profiles         []BuilderProviderProfile
	PreviewYAMLParts []string
	SuggestedPrompts []string
	Kind             domain.AgentKind
}

// TemplateRenderer compiles validated catalog entries into slack-go values.
// It has no Slack client and cannot publish or mutate application state.
type TemplateRenderer struct {
	catalog *TemplateCatalog
}

// NewTemplateRenderer constructs a renderer over a validated catalog.
func NewTemplateRenderer(catalog *TemplateCatalog) (*TemplateRenderer, error) {
	if catalog == nil {
		return nil, errors.New("template catalog is required")
	}
	if len(catalog.templates) != len(requiredTemplateNames) {
		return nil, errors.New("template catalog is incomplete")
	}
	for _, name := range requiredTemplateNames {
		if _, ok := catalog.templates[name]; !ok {
			return nil, fmt.Errorf("template catalog is missing %q", name)
		}
	}
	return &TemplateRenderer{catalog: catalog}, nil
}

// NewEmbeddedTemplateRenderer constructs a renderer over the embedded catalog.
func NewEmbeddedTemplateRenderer() (*TemplateRenderer, error) {
	catalog, err := EmbeddedTemplateCatalog()
	if err != nil {
		return nil, err
	}
	return NewTemplateRenderer(catalog)
}

// CompileModal compiles a modal template into a Slack modal view request.
func (r *TemplateRenderer) CompileModal(templateName string, context TemplateContext) (slackapi.ModalViewRequest, error) {
	doc, err := r.document(templateName)
	if err != nil {
		return slackapi.ModalViewRequest{}, err
	}
	if doc.Surface != "modal" {
		return slackapi.ModalViewRequest{}, fmt.Errorf("template %q is not a modal", templateName)
	}
	return compileModalTemplate(doc, context)
}

// CompileMessage compiles a message template into bounded Slack blocks. The
// accessible fallback can be obtained with CompileMessageWithFallback.
func (r *TemplateRenderer) CompileMessage(templateName string, context TemplateContext) ([]slackapi.Block, error) {
	_, blocks, err := r.CompileMessageWithFallback(templateName, context)
	return blocks, err
}

// CompileMessageWithFallback returns both message components needed by a
// Block Kit publisher.
func (r *TemplateRenderer) CompileMessageWithFallback(templateName string, context TemplateContext) (string, []slackapi.Block, error) {
	doc, err := r.document(templateName)
	if err != nil {
		return "", nil, err
	}
	if doc.Surface != "message" {
		return "", nil, fmt.Errorf("template %q is not a message", templateName)
	}
	return compileMessageTemplate(doc, context)
}

func (r *TemplateRenderer) document(name string) (templateDocument, error) {
	if r == nil || r.catalog == nil {
		return templateDocument{}, errors.New("template renderer is not configured")
	}
	doc, ok := r.catalog.templates[name]
	if !ok {
		return templateDocument{}, fmt.Errorf("template %q is not registered", name)
	}
	return doc, nil
}

type renderEnvironment struct {
	templateName     string
	kind             domain.AgentKind
	isExternalAgent  bool
	values           map[string]string
	profiles         []BuilderProviderProfile
	previewYAMLParts []string
	suggestedPrompts []string
}

func newRenderEnvironment(templateName string, context TemplateContext) (renderEnvironment, error) {
	kind := context.Kind
	if kind == "" {
		kind = domain.AgentKindLLM
	}
	if kind != domain.AgentKindLLM && kind != domain.AgentKindAgentCLI {
		return renderEnvironment{}, fmt.Errorf("unsupported template agent kind %q", kind)
	}
	return renderEnvironment{
		templateName:     templateName,
		kind:             kind,
		isExternalAgent:  kind == domain.AgentKindAgentCLI,
		values:           context.Values,
		profiles:         append([]BuilderProviderProfile(nil), context.Profiles...),
		previewYAMLParts: append([]string(nil), context.PreviewYAMLParts...),
		suggestedPrompts: append([]string(nil), context.SuggestedPrompts...),
	}, nil
}

func compileModalTemplate(doc templateDocument, context TemplateContext) (slackapi.ModalViewRequest, error) {
	env, err := newRenderEnvironment(doc.Name, context)
	if err != nil {
		return slackapi.ModalViewRequest{}, err
	}
	if doc.Modal == nil {
		return slackapi.ModalViewRequest{}, errors.New("template payload is missing")
	}
	modal := doc.Modal

	title, err := compileRawText(modal.Title, env, maxRendererModalTitleLength)
	if err != nil {
		return slackapi.ModalViewRequest{}, fmt.Errorf("compile modal title: %w", err)
	}

	blocks, err := substituteAndDecodeBlocks(modal.Blocks, env)
	if err != nil {
		return slackapi.ModalViewRequest{}, fmt.Errorf("compile modal blocks: %w", err)
	}

	view := slackapi.ModalViewRequest{
		Type:            slackapi.VTModal,
		Title:           title,
		CallbackID:      modal.CallbackID,
		PrivateMetadata: modal.PrivateMetadata,
		Blocks:          slackapi.Blocks{BlockSet: blocks},
	}
	if len(modal.Submit) > 0 {
		view.Submit, err = compileRawText(modal.Submit, env, maxRendererModalSubmitCloseLength)
		if err != nil {
			return slackapi.ModalViewRequest{}, fmt.Errorf("compile modal submit: %w", err)
		}
	}
	if len(modal.Close) > 0 {
		view.Close, err = compileRawText(modal.Close, env, maxRendererModalSubmitCloseLength)
		if err != nil {
			return slackapi.ModalViewRequest{}, fmt.Errorf("compile modal close: %w", err)
		}
	}
	if err := validateCompiledBlocks(doc.Name, view.Blocks.BlockSet, true); err != nil {
		return slackapi.ModalViewRequest{}, err
	}
	return view, nil
}

func compileMessageTemplate(doc templateDocument, context TemplateContext) (string, []slackapi.Block, error) {
	env, err := newRenderEnvironment(doc.Name, context)
	if err != nil {
		return "", nil, err
	}
	if doc.Message == nil {
		return "", nil, errors.New("template payload is missing")
	}
	fallback, err := substituteTopLevelString(doc.Message.FallbackText, env)
	if err != nil {
		return "", nil, fmt.Errorf("compile fallback_text: %w", err)
	}
	if strings.TrimSpace(fallback) == "" {
		return "", nil, errors.New("compiled fallback_text must not be empty")
	}
	if utf8.RuneCountInString(fallback) > maxFallbackText {
		return "", nil, fmt.Errorf("fallback_text exceeds %d character limit", maxFallbackText)
	}
	blocks, err := substituteAndDecodeBlocks(doc.Message.Blocks, env)
	if err != nil {
		return "", nil, err
	}
	if err := validateCompiledBlocks(doc.Name, blocks, false); err != nil {
		return "", nil, err
	}
	return fallback, blocks, nil
}

func substituteAndDecodeBlocks(raw json.RawMessage, env renderEnvironment) ([]slackapi.Block, error) {
	substituted, err := substituteTemplateTokens(raw, env)
	if err != nil {
		return nil, err
	}
	var blocks slackapi.Blocks
	if err := json.Unmarshal(substituted, &blocks); err != nil {
		return nil, fmt.Errorf("decode blocks: %w", err)
	}
	return blocks.BlockSet, nil
}

func compileRawText(raw json.RawMessage, env renderEnvironment, limit int) (*slackapi.TextBlockObject, error) {
	substituted, err := substituteTemplateTokens(raw, env)
	if err != nil {
		return nil, err
	}
	var text slackapi.TextBlockObject
	if err := json.Unmarshal(substituted, &text); err != nil {
		return nil, fmt.Errorf("decode text: %w", err)
	}
	if err := validateGenericText(&text, limit); err != nil {
		return nil, err
	}
	return &text, nil
}

// substituteTemplateTokens walks the template's JSON as a generic tree and
// resolves every {{value.x}}/{{options.x}} placeholder it finds, then
// re-marshals the tree. Because a replacement value is inserted into the
// parsed tree and re-serialized by encoding/json (never spliced into raw
// text), a value containing '"', '{', or '}' can never break out of its
// JSON string context and alter block structure.
func substituteTemplateTokens(raw json.RawMessage, env renderEnvironment) (json.RawMessage, error) {
	var tree any
	if err := json.Unmarshal(raw, &tree); err != nil {
		return nil, err
	}
	substituted, err := substituteNode(tree, env)
	if err != nil {
		return nil, err
	}
	return json.Marshal(substituted)
}

// literalOnlyKeys names the JSON object keys that must always be a fixed,
// developer-chosen literal and must never be substituted from a placeholder:
// they either drive slack-go's own type dispatch ("type") or this project's
// action-routing allowlist ("action_id", "block_id", "callback_id").
var literalOnlyKeys = map[string]bool{
	"type":        true,
	"action_id":   true,
	"block_id":    true,
	"callback_id": true,
}

func substituteNode(node any, env renderEnvironment) (any, error) {
	switch v := node.(type) {
	case string:
		return substituteStringLeaf(v, env)
	case map[string]any:
		out := make(map[string]any, len(v))
		var initialOptionRaw any
		hasInitialOption := false
		for key, val := range v {
			if key == "$if" {
				continue
			}
			if key == "initial_option" {
				initialOptionRaw = val
				hasInitialOption = true
				continue
			}
			if literalOnlyKeys[key] {
				literal, ok := val.(string)
				if !ok {
					return nil, fmt.Errorf("%q must be a string", key)
				}
				if err := rejectPlaceholder(literal, key); err != nil {
					return nil, err
				}
				out[key] = literal
				continue
			}
			resolved, err := substituteNode(val, env)
			if err != nil {
				return nil, err
			}
			out[key] = resolved
		}
		if hasInitialOption {
			resolved, err := resolveInitialOption(initialOptionRaw, out["options"], env)
			if err != nil {
				return nil, err
			}
			if resolved != nil {
				out["initial_option"] = resolved
			}
		}
		return out, nil
	case []any:
		out := make([]any, 0, len(v))
		for _, item := range v {
			if obj, ok := item.(map[string]any); ok {
				if condRaw, present := obj["$if"]; present {
					cond, ok := condRaw.(string)
					if !ok || cond != "is_external_agent" {
						return nil, fmt.Errorf("block has unknown condition %v", condRaw)
					}
					if !env.isExternalAgent {
						continue
					}
				}
			}
			if s, ok := item.(string); ok && isCollectionToken(s) {
				value, flatten, err := resolveCollectionToken(s, env)
				if err != nil {
					return nil, err
				}
				if flatten {
					out = append(out, value.([]any)...)
					continue
				}
				out = append(out, value)
				continue
			}
			resolved, err := substituteNode(item, env)
			if err != nil {
				return nil, err
			}
			out = append(out, resolved)
		}
		return out, nil
	default:
		return v, nil
	}
}

func substituteStringLeaf(s string, env renderEnvironment) (any, error) {
	token, isToken, err := parseTemplateString(s)
	if err != nil {
		return nil, err
	}
	if !isToken {
		return s, nil
	}
	switch token.Kind {
	case tokenValue:
		return resolveScalar(token.Key, env)
	case tokenOptions:
		value, flatten, err := resolveCollectionToken(s, env)
		if err != nil {
			return nil, err
		}
		if flatten {
			return nil, fmt.Errorf("collection %q must be a direct blocks array element", token.Key)
		}
		return value, nil
	default:
		return nil, fmt.Errorf("unknown placeholder kind for %q", s)
	}
}

func isCollectionToken(s string) bool {
	token, isToken, err := parseTemplateString(s)
	return err == nil && isToken && token.Kind == tokenOptions
}

// resolveScalar resolves a {{value.KEY}} placeholder. builder_modal's
// scalars are the imperative-builder's preserved-draft values and may
// legitimately be empty (first-open, no prior draft); every other
// template's scalars back displayed text or interactive payloads and must
// resolve to a non-empty value.
func resolveScalar(key string, env renderEnvironment) (string, error) {
	if _, ok := scalarKeysByTemplate[env.templateName][key]; !ok {
		return "", fmt.Errorf("unknown scalar placeholder %q", key)
	}
	value, supplied := env.values[key]
	if !supplied {
		return "", fmt.Errorf("scalar placeholder %q has no supplied value", key)
	}
	if env.templateName == "builder_modal" {
		switch key {
		case "agent_type":
			if value == "" {
				value = string(env.kind)
			}
		case "execution_mode":
			if value == "" && env.isExternalAgent {
				value = domain.ExecutionModeForeground
			}
		case "timeout_seconds":
			if value == "" && env.isExternalAgent {
				value = strconv.Itoa(domain.DefaultExternalAgentTimeoutSeconds)
			}
		}
		return value, nil
	}
	if value == "" {
		return "", fmt.Errorf("scalar placeholder %q resolved to an empty value", key)
	}
	return value, nil
}

func substituteTopLevelString(s string, env renderEnvironment) (string, error) {
	token, isToken, err := parseTemplateString(s)
	if err != nil {
		return "", err
	}
	if !isToken {
		return s, nil
	}
	if token.Kind != tokenValue {
		return "", fmt.Errorf("placeholder %q is not valid here", s)
	}
	return resolveScalar(token.Key, env)
}

// resolveInitialOption resolves a select-like element's "initial_option"
// field. It is a {{value.KEY}} placeholder naming a scalar; the resolved
// value is looked up by "value" among the element's own (already resolved)
// "options" array, falling back to the first option when absent, matching
// the imperative builder's preserved-draft behavior.
func resolveInitialOption(raw any, options any, env renderEnvironment) (any, error) {
	s, isString := raw.(string)
	if !isString {
		return substituteNode(raw, env)
	}
	token, isToken, err := parseTemplateString(s)
	if err != nil {
		return nil, err
	}
	if !isToken {
		return s, nil
	}
	if token.Kind != tokenValue {
		return nil, fmt.Errorf("initial_option must use a value placeholder, got %q", s)
	}
	value, err := resolveScalar(token.Key, env)
	if err != nil {
		return nil, err
	}
	if value == "" {
		return nil, nil
	}
	optionList, _ := options.([]any)
	for _, item := range optionList {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if obj["value"] == value {
			return obj, nil
		}
	}
	if len(optionList) > 0 {
		return optionList[0], nil
	}
	return nil, nil
}

// resolveCollectionToken resolves a {{options.KEY}} placeholder. This
// remains a small, explicit, named set: each key is a call-site-specific
// data shape (provider model choices, a dynamic run of YAML preview
// sections, a dynamic run of suggested prompts), not a generic mechanism.
// flatten reports whether value is a []any of blocks meant to replace a
// single blocks-array slot with multiple blocks.
func resolveCollectionToken(s string, env renderEnvironment) (value any, flatten bool, err error) {
	token, isToken, err := parseTemplateString(s)
	if err != nil {
		return nil, false, err
	}
	if !isToken || token.Kind != tokenOptions {
		return nil, false, fmt.Errorf("%q is not a collection token", s)
	}
	switch token.Key {
	case "model":
		if env.templateName != "builder_modal" {
			return nil, false, fmt.Errorf("collection %q is not valid outside builder_modal", token.Key)
		}
		options, err := buildModelOptions(env)
		if err != nil {
			return nil, false, err
		}
		return options, false, nil
	case "preview_yaml_parts":
		if env.templateName != "agent_preview" {
			return nil, false, fmt.Errorf("collection %q is not valid outside agent_preview", token.Key)
		}
		blocks, err := buildYAMLPreviewBlocks(env)
		if err != nil {
			return nil, false, err
		}
		return blocks, true, nil
	case "suggested_prompts":
		if env.templateName != "onboarding_message" {
			return nil, false, fmt.Errorf("collection %q is not valid outside onboarding_message", token.Key)
		}
		blocks, err := buildSuggestedPromptBlocks(env)
		if err != nil {
			return nil, false, err
		}
		return blocks, true, nil
	default:
		return nil, false, fmt.Errorf("unknown collection placeholder %q", token.Key)
	}
}

func buildModelOptions(env renderEnvironment) ([]any, error) {
	profiles := builderProfilesForKind(env.kind, env.profiles)
	options := make([]any, 0, len(profiles))
	for _, profile := range profiles {
		if strings.TrimSpace(profile.Reference) == "" {
			return nil, errors.New("provider profile reference must not be empty")
		}
		options = append(options, map[string]any{
			"text":  map[string]any{"type": "plain_text", "text": profile.Reference, "emoji": false},
			"value": profile.Reference,
		})
	}
	if len(options) == 0 {
		return nil, errors.New("options.model has no compatible provider profiles")
	}
	return options, nil
}

func buildYAMLPreviewBlocks(env renderEnvironment) ([]any, error) {
	blocks := make([]any, 0, len(env.previewYAMLParts))
	for index, value := range env.previewYAMLParts {
		if strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("preview_yaml_parts[%d] must not be empty", index)
		}
		if utf8.RuneCountInString(value) > builderBlockTextLimit {
			return nil, fmt.Errorf("preview_yaml_parts[%d] exceeds %d character limit", index, builderBlockTextLimit)
		}
		blocks = append(blocks, map[string]any{
			"type": "section",
			"text": map[string]any{"type": "mrkdwn", "text": value},
		})
	}
	return blocks, nil
}

func buildSuggestedPromptBlocks(env renderEnvironment) ([]any, error) {
	if len(env.suggestedPrompts) > 5 {
		return nil, errors.New("suggested_prompts exceeds five items")
	}
	blocks := make([]any, 0, len(env.suggestedPrompts))
	for index, value := range env.suggestedPrompts {
		if strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("suggested_prompts[%d] must not be empty", index)
		}
		if utf8.RuneCountInString(value) > 200 || strings.ContainsAny(value, "\r\n\x00") {
			return nil, fmt.Errorf("suggested_prompts[%d] is not a valid suggested prompt", index)
		}
		text := "- " + value
		if utf8.RuneCountInString(text) > maxRendererCompositionTextLength {
			return nil, fmt.Errorf("suggested_prompts[%d] exceeds %d character limit", index, maxRendererCompositionTextLength)
		}
		blocks = append(blocks, map[string]any{
			"type": "section",
			"text": map[string]any{"type": "mrkdwn", "text": text},
		})
	}
	return blocks, nil
}

func builderProfilesForKind(kind domain.AgentKind, profiles []BuilderProviderProfile) []BuilderProviderProfile {
	filtered := make([]BuilderProviderProfile, 0, len(profiles))
	for _, profile := range profiles {
		if kind == domain.AgentKindLLM && profile.ProviderType == agentdef.ProviderTypeOpenAICompatible {
			filtered = append(filtered, profile)
		}
		if kind == domain.AgentKindAgentCLI && profile.ProviderType == agentdef.ProviderTypeAgentCLI {
			filtered = append(filtered, profile)
		}
	}
	slices.SortFunc(filtered, func(a, b BuilderProviderProfile) int { return cmp.Compare(a.Reference, b.Reference) })
	return filtered
}

// validateGenericText applies this project's default text-length ceiling. It
// is not an attempt to re-implement Slack's exact per-field limits (Slack's
// API is authoritative there); it is a cheap defense-in-depth cap shared by
// every text-bearing Block Kit type, known and future.
func validateGenericText(text *slackapi.TextBlockObject, limit int) error {
	if text == nil {
		return errors.New("text object is required")
	}
	if text.Type != "plain_text" && text.Type != "mrkdwn" {
		return fmt.Errorf("text has invalid type %q", text.Type)
	}
	if text.Text == "" {
		return errors.New("text must not be empty")
	}
	if utf8.RuneCountInString(text.Text) > limit {
		return fmt.Errorf("text exceeds %d character limit", limit)
	}
	return nil
}

// validateCompiledBlocks is the small, mostly type-agnostic validation pass
// that runs after slack-go has decoded a template's substituted JSON. It
// enforces what is specific to this project's security posture (duplicate
// or unregistered block/action IDs, the message-surface input restriction,
// block-count limits) via a generic reflection walk (validateCompiledNode)
// that works for any block or element slack-go knows how to decode -
// including ones this file never names. A few historically load-bearing,
// already-tested exact limits (section text/fields, button text/value,
// option text/value) are kept as explicit cases so existing behavior does
// not regress; everything else relies on slack-go's decode plus Slack's own
// API to catch a malformed field.
func validateCompiledBlocks(templateName string, blocks []slackapi.Block, modal bool) error {
	limit := maxBlocksPerMessage
	if modal {
		limit = maxRendererBlocksPerModal
	}
	if len(blocks) > limit {
		return fmt.Errorf("blocks exceed %d limit", limit)
	}
	blockIDs := make(map[string]struct{}, len(blocks))
	actionIDs := make(map[string]struct{})
	for index, block := range blocks {
		if block == nil {
			return fmt.Errorf("compiled block %d is nil", index)
		}
		if !modal && block.BlockType() == slackapi.MBTInput {
			return errors.New("input blocks are not valid on message surface")
		}
		blockID := block.ID()
		if blockID != "" {
			if err := validateLiteralID(blockID, fmt.Sprintf("blocks[%d].block_id", index)); err != nil {
				return err
			}
			if _, exists := blockIDs[blockID]; exists {
				return fmt.Errorf("duplicate compiled block ID %q", blockID)
			}
			blockIDs[blockID] = struct{}{}
			allowed := allowedMessageBlockIDs
			if modal {
				allowed = allowedBuilderBlockIDs
			}
			if !allowed[blockID] {
				return fmt.Errorf("unregistered compiled block ID %q", blockID)
			}
		}
		if err := validateCompiledNode(reflect.ValueOf(block), templateName, modal, false, &actionIDs); err != nil {
			return fmt.Errorf("blocks[%d]: %w", index, err)
		}
	}
	return nil
}

func validateCompiledNode(
	v reflect.Value,
	templateName string,
	modal bool,
	allowBuilderStateActionID bool,
	actionIDs *map[string]struct{},
) error {
	if !v.IsValid() {
		return nil
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return nil
		}
		return validateCompiledNode(v.Elem(), templateName, modal, allowBuilderStateActionID, actionIDs)
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			if err := validateCompiledNode(v.Index(i), templateName, modal, false, actionIDs); err != nil {
				return err
			}
		}
		return nil
	case reflect.Map:
		for _, key := range v.MapKeys() {
			if err := validateCompiledNode(v.MapIndex(key), templateName, modal, false, actionIDs); err != nil {
				return err
			}
		}
		return nil
	case reflect.Struct:
		if err := validateCompiledStruct(v, templateName); err != nil {
			return err
		}
		if _, ok := v.Interface().(slackapi.InputBlock); ok && !modal {
			return errors.New("input blocks are not valid on message surface")
		}
		if field := v.FieldByName("ActionID"); field.IsValid() && field.Kind() == reflect.String {
			if err := validateCompiledActionID(field.String(), modal, allowBuilderStateActionID, actionIDs); err != nil {
				return err
			}
		}
		input, isInput := v.Interface().(slackapi.InputBlock)
		for i := 0; i < v.NumField(); i++ {
			fieldType := v.Type().Field(i)
			if !fieldType.IsExported() {
				continue
			}
			allowStateActionID := isInput && fieldType.Name == "Element" && !inputElementDispatches(input)
			if err := validateCompiledNode(v.Field(i), templateName, modal, allowStateActionID, actionIDs); err != nil {
				return err
			}
		}
		return nil
	default:
		return nil
	}
}

func validateCompiledStruct(v reflect.Value, templateName string) error {
	switch typed := v.Interface().(type) {
	case slackapi.UnknownBlock:
		return fmt.Errorf("unsupported block type %q", typed.Type)
	case slackapi.UnknownBlockElement:
		return fmt.Errorf("unsupported block element type %q", typed.Type)
	case slackapi.TextBlockObject:
		limit := maxRendererCompositionTextLength
		if templateName == "agent_preview" && typed.Type == "mrkdwn" {
			limit = builderBlockTextLimit
		}
		return validateGenericText(&typed, limit)
	case slackapi.OptionBlockObject:
		return validateCompiledOption(&typed)
	case slackapi.CardBlock:
		return validateCompiledCard(&typed)
	case slackapi.ButtonBlockElement:
		return validateCompiledButton(&typed)
	case slackapi.SectionBlock:
		return validateCompiledSection(&typed, templateName)
	case slackapi.PlainTextInputBlockElement:
		return validateCompiledPlainTextInput(&typed)
	}
	return nil
}

func validateCompiledCard(card *slackapi.CardBlock) error {
	if card.Icon != nil && card.SlackIcon != nil {
		return errors.New("card icon and slack_icon are mutually exclusive")
	}
	if card.HeroImage == nil && card.Title == nil && card.Body == nil &&
		(card.Actions == nil || len(card.Actions.ElementSet) == 0) {
		return errors.New("card requires a hero image, title, body, or actions")
	}
	fields := []struct {
		name  string
		text  *slackapi.TextBlockObject
		limit int
	}{
		{name: "title", text: card.Title, limit: maxRendererCardTitleLength},
		{name: "subtitle", text: card.Subtitle, limit: maxRendererCardSubtitleLength},
		{name: "body", text: card.Body, limit: maxRendererCardBodyLength},
		{name: "subtext", text: card.Subtext, limit: maxRendererCardSubtextLength},
	}
	for _, field := range fields {
		if field.text == nil {
			continue
		}
		if err := validateGenericText(field.text, field.limit); err != nil {
			return fmt.Errorf("card %s: %w", field.name, err)
		}
	}
	if card.Actions != nil && len(card.Actions.ElementSet) > maxRendererCardActions {
		return fmt.Errorf("card exceeds %d actions", maxRendererCardActions)
	}
	return nil
}

func validateCompiledSection(section *slackapi.SectionBlock, templateName string) error {
	if len(section.Fields) > maxRendererSectionFields {
		return fmt.Errorf("section exceeds %d fields", maxRendererSectionFields)
	}
	if section.Text != nil {
		limit := maxRendererCompositionTextLength
		if templateName == "agent_preview" && section.Text.Type == "mrkdwn" {
			limit = builderBlockTextLimit
		}
		if err := validateGenericText(section.Text, limit); err != nil {
			return err
		}
	}
	for _, field := range section.Fields {
		if err := validateGenericText(field, maxRendererSectionFieldLength); err != nil {
			return err
		}
	}
	return nil
}

func validateCompiledButton(button *slackapi.ButtonBlockElement) error {
	if err := validateGenericText(button.Text, maxRendererButtonTextLength); err != nil {
		return err
	}
	if utf8.RuneCountInString(button.Value) > maxRendererOptionValueLength {
		return fmt.Errorf("button value exceeds %d character limit", maxRendererOptionValueLength)
	}
	return validateSlackButtonURL(button.URL, "button.url")
}

func validateCompiledOption(option *slackapi.OptionBlockObject) error {
	if err := validateGenericText(option.Text, maxRendererOptionTextLength); err != nil {
		return err
	}
	if utf8.RuneCountInString(option.Value) > maxRendererOptionValueLength {
		return fmt.Errorf("option value exceeds %d character limit", maxRendererOptionValueLength)
	}
	if option.Description != nil {
		if err := validateGenericText(option.Description, maxRendererOptionTextLength); err != nil {
			return err
		}
	}
	return validateSlackButtonURL(option.URL, "option.url")
}

func validateCompiledPlainTextInput(element *slackapi.PlainTextInputBlockElement) error {
	if element.MaxLength > maxRendererPlainTextInputLength {
		return fmt.Errorf("input max_length exceeds %d", maxRendererPlainTextInputLength)
	}
	if element.MaxLength > 0 && utf8.RuneCountInString(element.InitialValue) > element.MaxLength {
		return errors.New("input initial value exceeds max_length")
	}
	return nil
}

func inputElementDispatches(input slackapi.InputBlock) bool {
	if input.DispatchAction || input.Element == nil {
		return input.DispatchAction
	}
	element := reflect.ValueOf(input.Element)
	if element.Kind() == reflect.Pointer {
		if element.IsNil() {
			return false
		}
		element = element.Elem()
	}
	if element.Kind() != reflect.Struct {
		return false
	}
	config := element.FieldByName("DispatchActionConfig")
	return config.IsValid() && config.Kind() == reflect.Pointer && !config.IsNil()
}

func validateCompiledActionID(actionID string, modal, allowBuilderStateActionID bool, actionIDs *map[string]struct{}) error {
	if err := validateLiteralID(actionID, "action_id"); err != nil {
		return err
	}
	if _, exists := (*actionIDs)[actionID]; exists {
		return fmt.Errorf("duplicate compiled action ID %q", actionID)
	}
	(*actionIDs)[actionID] = struct{}{}
	if modal && allowBuilderStateActionID {
		if !allowedBuilderBlockIDs[actionID] {
			return fmt.Errorf("unregistered compiled modal state action ID %q", actionID)
		}
	} else if modal {
		if !allowedInteractiveActionIDs[actionID] {
			return fmt.Errorf("unregistered compiled modal action ID %q", actionID)
		}
	} else if !allowedMessageActionIDs[actionID] {
		return fmt.Errorf("unregistered compiled message action ID %q", actionID)
	}
	return nil
}

// collectInteractiveIDs extracts the callback ID, block IDs, and action IDs
// from an already-compiled, already-validated block set via the same
// generic reflection walk used by validateCompiledNode, so the catalog's ID
// inventory (used by ValidateDispatcher) automatically covers any block or
// element type without a dedicated collector per type.
func collectInteractiveIDs(templateName, callbackID string, blocks []slackapi.Block, modal bool) (TemplateInteractiveIDs, error) {
	ids := TemplateInteractiveIDs{}
	if callbackID != "" {
		appendTemplateID(&ids.ModalCallbacks, callbackID)
	}
	actionIDs := map[string]struct{}{}
	for _, block := range blocks {
		if block == nil {
			continue
		}
		if blockID := block.ID(); blockID != "" {
			if modal {
				appendTemplateID(&ids.BuilderBlocks, blockID)
			} else {
				appendTemplateID(&ids.MessageBlocks, blockID)
			}
		}
		if err := collectActionIDs(reflect.ValueOf(block), modal, false, &actionIDs); err != nil {
			return TemplateInteractiveIDs{}, err
		}
	}
	for actionID := range actionIDs {
		appendTemplateID(&ids.Actions, actionID)
	}
	return ids, nil
}

func collectActionIDs(v reflect.Value, modal, allowBuilderStateActionID bool, actionIDs *map[string]struct{}) error {
	if !v.IsValid() {
		return nil
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return nil
		}
		return collectActionIDs(v.Elem(), modal, allowBuilderStateActionID, actionIDs)
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			if err := collectActionIDs(v.Index(i), modal, false, actionIDs); err != nil {
				return err
			}
		}
		return nil
	case reflect.Map:
		for _, key := range v.MapKeys() {
			if err := collectActionIDs(v.MapIndex(key), modal, false, actionIDs); err != nil {
				return err
			}
		}
		return nil
	case reflect.Struct:
		if field := v.FieldByName("ActionID"); field.IsValid() && field.Kind() == reflect.String && field.String() != "" && !allowBuilderStateActionID {
			(*actionIDs)[field.String()] = struct{}{}
		}
		input, isInput := v.Interface().(slackapi.InputBlock)
		for i := 0; i < v.NumField(); i++ {
			fieldType := v.Type().Field(i)
			if !fieldType.IsExported() {
				continue
			}
			allowStateActionID := isInput && fieldType.Name == "Element" && !inputElementDispatches(input)
			if err := collectActionIDs(v.Field(i), modal, allowStateActionID, actionIDs); err != nil {
				return err
			}
		}
		return nil
	default:
		return nil
	}
}
