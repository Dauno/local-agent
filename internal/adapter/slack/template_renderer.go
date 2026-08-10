package slack

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Dauno/slack-local-agent/internal/agentdef"
	"github.com/Dauno/slack-local-agent/internal/domain"
	slackapi "github.com/slack-go/slack"
)

const (
	maxRendererBlocksPerModal         = 100
	maxRendererCompositionTextLength  = 3000
	maxRendererModalTitleLength       = 24
	maxRendererModalSubmitCloseLength = 24
	maxRendererModalLabelLength       = 200
	maxRendererModalHintLength        = 200
	maxRendererPlaceholderLength      = 150
	maxRendererButtonTextLength       = 75
	maxRendererSectionFields          = 10
	maxRendererActionElements         = 25
	maxRendererIDLength               = 255
	maxRendererPlainTextInputLength   = 3000
	maxRendererStaticSelectOptions    = 100
	maxRendererOptionTextLength       = 75
	maxRendererOptionValueLength      = 2000
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
	compiled, err := compileTemplateDocument(doc, context)
	if err != nil {
		return slackapi.ModalViewRequest{}, err
	}
	if compiled.Modal == nil {
		return slackapi.ModalViewRequest{}, errors.New("compiled modal is missing")
	}
	return *compiled.Modal, nil
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
	compiled, err := compileTemplateDocument(doc, context)
	if err != nil {
		return "", nil, err
	}
	return compiled.Fallback, compiled.Blocks, nil
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

type compiledTemplate struct {
	Modal    *slackapi.ModalViewRequest
	Fallback string
	Blocks   []slackapi.Block
}

type renderEnvironment struct {
	templateName     string
	kind             domain.AgentKind
	isACP            bool
	values           map[string]string
	profiles         []BuilderProviderProfile
	previewYAMLParts []string
	suggestedPrompts []string
}

func compileTemplateDocument(doc templateDocument, context TemplateContext) (compiledTemplate, error) {
	env, err := newRenderEnvironment(doc.Name, context)
	if err != nil {
		return compiledTemplate{}, err
	}
	if doc.Modal != nil {
		modal := doc.Modal
		blocks, err := compileBlocks(doc.Name, modal.Blocks, env)
		if err != nil {
			return compiledTemplate{}, err
		}
		if len(blocks) > maxRendererBlocksPerModal {
			return compiledTemplate{}, fmt.Errorf("modal exceeds %d block limit", maxRendererBlocksPerModal)
		}
		title, err := compileText(modal.Title, env, false, maxRendererModalTitleLength)
		if err != nil {
			return compiledTemplate{}, fmt.Errorf("compile modal title: %w", err)
		}
		view := slackapi.ModalViewRequest{
			Type:            slackapi.VTModal,
			Title:           title,
			CallbackID:      modal.CallbackID,
			PrivateMetadata: modal.PrivateMetadata,
			Blocks:          slackapi.Blocks{BlockSet: blocks},
		}
		if modal.Submit != nil {
			view.Submit, err = compileText(modal.Submit, env, false, maxRendererModalSubmitCloseLength)
			if err != nil {
				return compiledTemplate{}, fmt.Errorf("compile modal submit: %w", err)
			}
		}
		if modal.Close != nil {
			view.Close, err = compileText(modal.Close, env, false, maxRendererModalSubmitCloseLength)
			if err != nil {
				return compiledTemplate{}, fmt.Errorf("compile modal close: %w", err)
			}
		}
		if err := validateCompiledView(doc.Name, view); err != nil {
			return compiledTemplate{}, err
		}
		return compiledTemplate{Modal: &view}, nil
	}

	if doc.Message == nil {
		return compiledTemplate{}, errors.New("template payload is missing")
	}
	fallback, err := resolveString(doc.Message.FallbackText, env, false)
	if err != nil {
		return compiledTemplate{}, fmt.Errorf("compile fallback_text: %w", err)
	}
	if strings.TrimSpace(fallback) == "" {
		return compiledTemplate{}, errors.New("compiled fallback_text must not be empty")
	}
	if utf8.RuneCountInString(fallback) > maxFallbackText {
		return compiledTemplate{}, fmt.Errorf("fallback_text exceeds %d character limit", maxFallbackText)
	}
	blocks, err := compileBlocks(doc.Name, doc.Message.Blocks, env)
	if err != nil {
		return compiledTemplate{}, err
	}
	if len(blocks) > maxBlocksPerMessage {
		return compiledTemplate{}, fmt.Errorf("message exceeds %d block limit", maxBlocksPerMessage)
	}
	if err := validateCompiledBlocks(doc.Name, blocks, false); err != nil {
		return compiledTemplate{}, err
	}
	return compiledTemplate{Fallback: fallback, Blocks: blocks}, nil
}

func newRenderEnvironment(templateName string, context TemplateContext) (renderEnvironment, error) {
	kind := context.Kind
	if kind == "" {
		kind = domain.AgentKindLLM
	}
	if kind != domain.AgentKindLLM && kind != domain.AgentKindACP {
		return renderEnvironment{}, fmt.Errorf("unsupported template agent kind %q", kind)
	}
	return renderEnvironment{
		templateName:     templateName,
		kind:             kind,
		isACP:            kind == domain.AgentKindACP,
		values:           context.Values,
		profiles:         append([]BuilderProviderProfile(nil), context.Profiles...),
		previewYAMLParts: append([]string(nil), context.PreviewYAMLParts...),
		suggestedPrompts: append([]string(nil), context.SuggestedPrompts...),
	}, nil
}

func compileBlocks(templateName string, source []templateBlock, env renderEnvironment) ([]slackapi.Block, error) {
	blocks := make([]slackapi.Block, 0, len(source))
	for index, sourceBlock := range source {
		if sourceBlock.CollectionToken != "" {
			collection, err := compileCollectionBlocks(sourceBlock.CollectionToken, env, fmt.Sprintf("blocks[%d]", index))
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, collection...)
			continue
		}
		if sourceBlock.Condition == "is_acp" && !env.isACP {
			continue
		}
		block, err := compileBlock(sourceBlock, env, fmt.Sprintf("blocks[%d]", index))
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, block)
	}
	return blocks, nil
}

func compileCollectionBlocks(token string, env renderEnvironment, fieldPath string) ([]slackapi.Block, error) {
	parsed, isToken, err := parseTemplateString(token)
	if err != nil || !isToken || parsed.Kind != tokenOptions {
		return nil, fmt.Errorf("%s has an invalid collection token", fieldPath)
	}
	var values []string
	limit := maxRendererCompositionTextLength
	switch parsed.Key {
	case "preview_yaml_parts":
		if env.templateName != "agent_preview" {
			return nil, fmt.Errorf("%s uses preview_yaml_parts outside agent_preview", fieldPath)
		}
		values = env.previewYAMLParts
		limit = builderBlockTextLimit
	case "suggested_prompts":
		if env.templateName != "onboarding_message" {
			return nil, fmt.Errorf("%s uses suggested_prompts outside onboarding_message", fieldPath)
		}
		if len(env.suggestedPrompts) > 5 {
			return nil, fmt.Errorf("%s exceeds five suggested prompts", fieldPath)
		}
		values = env.suggestedPrompts
	default:
		return nil, fmt.Errorf("%s uses unknown collection placeholder %q", fieldPath, parsed.Key)
	}
	blocks := make([]slackapi.Block, 0, len(values))
	for index, value := range values {
		if strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("%s[%d] must not be empty", fieldPath, index)
		}
		if parsed.Key == "suggested_prompts" && (utf8.RuneCountInString(value) > 200 || strings.ContainsAny(value, "\r\n\x00")) {
			return nil, fmt.Errorf("%s[%d] is not a valid suggested prompt", fieldPath, index)
		}
		if utf8.RuneCountInString(value) > limit {
			return nil, fmt.Errorf("%s[%d] exceeds %d character limit", fieldPath, index, limit)
		}
		text := value
		if parsed.Key == "suggested_prompts" {
			text = "- " + value
			if utf8.RuneCountInString(text) > maxRendererCompositionTextLength {
				return nil, fmt.Errorf("%s[%d] exceeds %d character limit", fieldPath, index, maxRendererCompositionTextLength)
			}
		}
		blocks = append(blocks, slackapi.NewSectionBlock(slackapi.NewTextBlockObject("mrkdwn", text, false, false), nil, nil))
	}
	return blocks, nil
}

func compileBlock(source templateBlock, env renderEnvironment, fieldPath string) (slackapi.Block, error) {
	switch source.Type {
	case "section":
		var text *slackapi.TextBlockObject
		var err error
		if source.Text != nil {
			text, err = compileText(source.Text, env, false, maxRendererCompositionTextLength)
			if err != nil {
				return nil, fmt.Errorf("%s.text: %w", fieldPath, err)
			}
		}
		var fields []*slackapi.TextBlockObject
		if len(source.Fields) > 0 {
			fields = make([]*slackapi.TextBlockObject, 0, len(source.Fields))
			for index, field := range source.Fields {
				compiled, err := compileText(field, env, false, maxRendererCompositionTextLength)
				if err != nil {
					return nil, fmt.Errorf("%s.fields[%d]: %w", fieldPath, index, err)
				}
				fields = append(fields, compiled)
			}
		}
		var accessory *slackapi.Accessory
		if source.Accessory != nil {
			compiled, err := compileElement(source.Accessory, env, fieldPath+".accessory")
			if err != nil {
				return nil, err
			}
			accessory = slackapi.NewAccessory(compiled)
		}
		block := slackapi.NewSectionBlock(text, fields, accessory)
		block.BlockID = source.BlockID
		return block, nil
	case "input":
		label, err := compileText(source.Label, env, false, maxRendererModalLabelLength)
		if err != nil {
			return nil, fmt.Errorf("%s.label: %w", fieldPath, err)
		}
		var hint *slackapi.TextBlockObject
		if source.Hint != nil {
			hint, err = compileText(source.Hint, env, false, maxRendererModalHintLength)
			if err != nil {
				return nil, fmt.Errorf("%s.hint: %w", fieldPath, err)
			}
		}
		element, err := compileElement(source.Element, env, fieldPath+".element")
		if err != nil {
			return nil, err
		}
		block := slackapi.NewInputBlock(source.BlockID, label, hint, element)
		block.WithOptional(source.Optional).WithDispatchAction(source.DispatchAction)
		return block, nil
	case "actions":
		elements := make([]slackapi.BlockElement, 0, len(source.Elements))
		for index, element := range source.Elements {
			compiled, err := compileElement(element, env, fmt.Sprintf("%s.elements[%d]", fieldPath, index))
			if err != nil {
				return nil, err
			}
			elements = append(elements, compiled)
		}
		block := slackapi.NewActionBlock(source.BlockID, elements...)
		return block, nil
	default:
		return nil, fmt.Errorf("%s has unsupported block type %q", fieldPath, source.Type)
	}
}

func compileElement(source *templateElement, env renderEnvironment, fieldPath string) (slackapi.BlockElement, error) {
	if source == nil {
		return nil, fmt.Errorf("%s is required", fieldPath)
	}
	switch source.Type {
	case "plain_text_input":
		var placeholder *slackapi.TextBlockObject
		var err error
		if source.Placeholder != nil {
			placeholder, err = compileText(source.Placeholder, env, false, maxRendererPlaceholderLength)
			if err != nil {
				return nil, fmt.Errorf("%s.placeholder: %w", fieldPath, err)
			}
		}
		element := slackapi.NewPlainTextInputBlockElement(placeholder, source.ActionID)
		element.Multiline = source.Multiline
		element.MinLength = source.MinLength
		element.MaxLength = source.MaxLength
		element.FocusOnLoad = source.FocusOnLoad
		if source.InitialValuePresent {
			value, err := resolveString(source.InitialValue, env, true)
			if err != nil {
				return nil, fmt.Errorf("%s.initial_value: %w", fieldPath, err)
			}
			element.InitialValue = value
		}
		if source.DispatchActionConfig != nil {
			element.DispatchActionConfig = &slackapi.DispatchActionConfig{TriggerActionsOn: append([]string(nil), source.DispatchActionConfig.TriggerActionsOn...)}
		}
		return element, nil
	case "static_select":
		options, err := compileOptions(source, env, fieldPath)
		if err != nil {
			return nil, err
		}
		var placeholder *slackapi.TextBlockObject
		if source.Placeholder != nil {
			placeholder, err = compileText(source.Placeholder, env, false, maxRendererPlaceholderLength)
			if err != nil {
				return nil, fmt.Errorf("%s.placeholder: %w", fieldPath, err)
			}
		}
		element := slackapi.NewOptionsSelectBlockElement(slackapi.OptTypeStatic, placeholder, source.ActionID, options...)
		element.FocusOnLoad = source.FocusOnLoad
		if source.InitialOptionPresent {
			selected, err := compileInitialOption(source, options, env, fieldPath)
			if err != nil {
				return nil, err
			}
			element.InitialOption = selected
		}
		return element, nil
	case "button":
		text, err := compileText(source.Text, env, false, maxRendererButtonTextLength)
		if err != nil {
			return nil, fmt.Errorf("%s.text: %w", fieldPath, err)
		}
		value, err := resolveString(source.Value, env, false)
		if err != nil {
			return nil, fmt.Errorf("%s.value: %w", fieldPath, err)
		}
		element := slackapi.NewButtonBlockElement(source.ActionID, value, text)
		element.Style = slackapi.Style(source.Style)
		element.URL = source.URL
		return element, nil
	default:
		return nil, fmt.Errorf("%s has unsupported element type %q", fieldPath, source.Type)
	}
}

func compileOptions(source *templateElement, env renderEnvironment, fieldPath string) ([]*slackapi.OptionBlockObject, error) {
	var options []*slackapi.OptionBlockObject
	if source.OptionsToken != "" {
		if source.OptionsToken != "{{options.model}}" {
			return nil, fmt.Errorf("%s.options has unsupported collection token", fieldPath)
		}
		for _, profile := range builderProfilesForKind(env.kind, env.profiles) {
			if strings.TrimSpace(profile.Reference) == "" {
				return nil, errors.New("provider profile reference must not be empty")
			}
			options = append(options, slackapi.NewOptionBlockObject(
				profile.Reference,
				slackapi.NewTextBlockObject("plain_text", profile.Reference, false, false),
				nil,
			))
		}
		if len(options) == 0 {
			return nil, fmt.Errorf("%s.options has no compatible provider profiles", fieldPath)
		}
		return options, nil
	}
	for index, option := range source.Options {
		compiled, err := compileOption(option, env, fmt.Sprintf("%s.options[%d]", fieldPath, index))
		if err != nil {
			return nil, err
		}
		options = append(options, compiled)
	}
	return options, nil
}

func compileInitialOption(source *templateElement, options []*slackapi.OptionBlockObject, env renderEnvironment, fieldPath string) (*slackapi.OptionBlockObject, error) {
	if source.InitialOptionToken != "" {
		value, err := resolveString(source.InitialOptionToken, env, true)
		if err != nil {
			return nil, fmt.Errorf("%s.initial_option: %w", fieldPath, err)
		}
		if value == "" {
			return firstOption(options), nil
		}
		for _, option := range options {
			if option.Value == value {
				return option, nil
			}
		}
		// The imperative builder keeps its first option when a preserved
		// selection is no longer present in the trusted provider catalog.
		return firstOption(options), nil
	}
	if source.InitialOption != nil {
		return compileOption(*source.InitialOption, env, fieldPath+".initial_option")
	}
	return nil, nil
}

func firstOption(options []*slackapi.OptionBlockObject) *slackapi.OptionBlockObject {
	if len(options) == 0 {
		return nil
	}
	return options[0]
}

func compileOption(source templateOption, env renderEnvironment, fieldPath string) (*slackapi.OptionBlockObject, error) {
	text, err := compileText(source.Text, env, false, maxRendererOptionTextLength)
	if err != nil {
		return nil, fmt.Errorf("%s.text: %w", fieldPath, err)
	}
	value, err := resolveString(source.Value, env, false)
	if err != nil {
		return nil, fmt.Errorf("%s.value: %w", fieldPath, err)
	}
	option := slackapi.NewOptionBlockObject(value, text, nil)
	if source.Description != nil {
		description, err := compileText(source.Description, env, false, maxRendererOptionTextLength)
		if err != nil {
			return nil, fmt.Errorf("%s.description: %w", fieldPath, err)
		}
		option.Description = description
	}
	option.URL = source.URL
	return option, nil
}

func compileText(source *templateText, env renderEnvironment, optional bool, limit int) (*slackapi.TextBlockObject, error) {
	if source == nil {
		return nil, errors.New("text object is required")
	}
	text, err := resolveString(source.Text, env, optional)
	if err != nil {
		return nil, err
	}
	if text == "" && optional {
		return nil, nil
	}
	if text == "" {
		return nil, errors.New("text must not be empty")
	}
	if utf8.RuneCountInString(text) > limit {
		return nil, fmt.Errorf("text exceeds %d character limit", limit)
	}
	if source.Type == "mrkdwn" {
		return &slackapi.TextBlockObject{Type: source.Type, Text: text, Verbatim: source.Verbatim}, nil
	}
	return &slackapi.TextBlockObject{Type: source.Type, Text: text, Emoji: source.Emoji, Verbatim: source.Verbatim}, nil
}

func resolveString(source string, env renderEnvironment, optional bool) (string, error) {
	token, isToken, err := parseTemplateString(source)
	if err != nil {
		return "", err
	}
	if !isToken {
		return source, nil
	}
	if token.Kind != tokenValue {
		return "", errors.New("collection token is only valid at a registered collection position")
	}
	if _, ok := scalarKeysByTemplate[env.templateName][token.Key]; !ok {
		return "", fmt.Errorf("unknown scalar placeholder %q", token.Key)
	}
	value := env.values[token.Key]
	if env.templateName == "builder_modal" {
		switch token.Key {
		case "agent_type":
			if value == "" {
				value = string(env.kind)
			}
		case "execution_mode":
			if value == "" && env.isACP {
				value = domain.ExecutionModeForeground
			}
		case "timeout_seconds":
			if value == "" && env.isACP {
				value = strconv.Itoa(domain.DefaultACPTimeoutSeconds)
			}
		}
	}
	if value == "" && !optional {
		return "", fmt.Errorf("scalar placeholder %q resolved to an empty value", token.Key)
	}
	return value, nil
}

func builderProfilesForKind(kind domain.AgentKind, profiles []BuilderProviderProfile) []BuilderProviderProfile {
	filtered := make([]BuilderProviderProfile, 0, len(profiles))
	for _, profile := range profiles {
		if kind == domain.AgentKindLLM && profile.ProviderType == agentdef.ProviderTypeOpenAICompatible {
			filtered = append(filtered, profile)
		}
		if kind == domain.AgentKindACP && profile.ProviderType == agentdef.ProviderTypeACP && strings.HasPrefix(profile.Reference, "opencode/") && len(profile.Reference) > len("opencode/") {
			filtered = append(filtered, profile)
		}
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].Reference < filtered[j].Reference })
	return filtered
}

func validateCompiledView(templateName string, view slackapi.ModalViewRequest) error {
	if view.Type != slackapi.VTModal || view.Title == nil {
		return errors.New("compiled modal root is invalid")
	}
	if err := validateCompiledText(view.Title, maxRendererModalTitleLength, "title"); err != nil {
		return err
	}
	if view.Submit != nil {
		if err := validateCompiledText(view.Submit, maxRendererModalSubmitCloseLength, "submit"); err != nil {
			return err
		}
	}
	if view.Close != nil {
		if err := validateCompiledText(view.Close, maxRendererModalSubmitCloseLength, "close"); err != nil {
			return err
		}
	}
	return validateCompiledBlocks(templateName, view.Blocks.BlockSet, true)
}

func validateCompiledBlocks(templateName string, blocks []slackapi.Block, modal bool) error {
	blockIDs := make(map[string]struct{}, len(blocks))
	actionIDs := make(map[string]struct{})
	for index, block := range blocks {
		if block == nil {
			return fmt.Errorf("compiled block %d is nil", index)
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
		}
		if modal {
			if blockID != "" && !allowedBuilderBlockIDs[blockID] {
				return fmt.Errorf("unregistered compiled modal block ID %q", blockID)
			}
		} else if blockID != "" && !allowedMessageBlockIDs[blockID] {
			return fmt.Errorf("unregistered compiled message block ID %q", blockID)
		}

		switch typed := block.(type) {
		case *slackapi.SectionBlock:
			if len(typed.Fields) > maxRendererSectionFields {
				return fmt.Errorf("section exceeds %d fields", maxRendererSectionFields)
			}
			if typed.Text != nil {
				limit := maxRendererCompositionTextLength
				if templateName == "agent_preview" && typed.Text.Type == "mrkdwn" {
					limit = builderBlockTextLimit
				}
				if err := validateCompiledText(typed.Text, limit, "section.text"); err != nil {
					return err
				}
			}
			for _, field := range typed.Fields {
				if err := validateCompiledText(field, maxRendererCompositionTextLength, "section.fields"); err != nil {
					return err
				}
			}
			if typed.Accessory != nil {
				if err := validateCompiledAccessory(templateName, typed.Accessory, modal, &actionIDs); err != nil {
					return err
				}
			}
		case *slackapi.InputBlock:
			if !modal {
				return errors.New("input blocks are not valid on message surface")
			}
			if err := validateCompiledText(typed.Label, maxRendererCompositionTextLength, "input.label"); err != nil {
				return err
			}
			if typed.Hint != nil {
				if err := validateCompiledText(typed.Hint, maxRendererCompositionTextLength, "input.hint"); err != nil {
					return err
				}
			}
			switch typed.Element.(type) {
			case *slackapi.PlainTextInputBlockElement, *slackapi.SelectBlockElement:
			default:
				return errors.New("input block element type is not supported by its parent")
			}
			if err := validateCompiledElement(typed.Element, modal, &actionIDs); err != nil {
				return err
			}
		case *slackapi.ActionBlock:
			if typed.Elements == nil || len(typed.Elements.ElementSet) == 0 {
				return errors.New("compiled action block has no elements")
			}
			if len(typed.Elements.ElementSet) > maxRendererActionElements {
				return fmt.Errorf("action block exceeds %d elements", maxRendererActionElements)
			}
			for _, element := range typed.Elements.ElementSet {
				switch element.(type) {
				case *slackapi.ButtonBlockElement, *slackapi.SelectBlockElement:
				default:
					return errors.New("action block element type is not supported by its parent")
				}
				if err := validateCompiledElement(element, modal, &actionIDs); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("compiled block type %T is not allowed", block)
		}
	}
	return nil
}

func validateCompiledAccessory(templateName string, accessory *slackapi.Accessory, modal bool, actionIDs *map[string]struct{}) error {
	if accessory == nil {
		return nil
	}
	if accessory.ButtonElement != nil {
		return validateCompiledElement(accessory.ButtonElement, modal, actionIDs)
	}
	if accessory.SelectElement != nil {
		return validateCompiledElement(accessory.SelectElement, modal, actionIDs)
	}
	if accessory.PlainTextInputElement != nil {
		return errors.New("plain text input is not valid as a section accessory")
	}
	return fmt.Errorf("compiled accessory for %s is not allowed", templateName)
}

func validateCompiledElement(element slackapi.BlockElement, modal bool, actionIDs *map[string]struct{}) error {
	if element == nil {
		return errors.New("compiled element is nil")
	}
	var actionID string
	switch typed := element.(type) {
	case *slackapi.PlainTextInputBlockElement:
		if !modal {
			return errors.New("input elements are not valid on message surface")
		}
		actionID = typed.ActionID
		if typed.MaxLength > maxRendererPlainTextInputLength {
			return fmt.Errorf("input max_length exceeds %d", maxRendererPlainTextInputLength)
		}
		if typed.MaxLength > 0 && utf8.RuneCountInString(typed.InitialValue) > typed.MaxLength {
			return errors.New("input initial value exceeds max_length")
		}
		if typed.Placeholder != nil {
			if err := validateCompiledText(typed.Placeholder, maxRendererCompositionTextLength, "input.placeholder"); err != nil {
				return err
			}
		}
	case *slackapi.SelectBlockElement:
		actionID = typed.ActionID
		if typed.Type != slackapi.OptTypeStatic {
			return fmt.Errorf("select element type %q is not allowed", typed.Type)
		}
		if len(typed.Options) > maxRendererStaticSelectOptions {
			return fmt.Errorf("select exceeds %d options", maxRendererStaticSelectOptions)
		}
		for _, option := range typed.Options {
			if err := validateCompiledOption(option); err != nil {
				return err
			}
		}
		if typed.InitialOption != nil {
			if err := validateCompiledOption(typed.InitialOption); err != nil {
				return err
			}
		}
		if typed.Placeholder != nil {
			if err := validateCompiledText(typed.Placeholder, maxRendererCompositionTextLength, "select.placeholder"); err != nil {
				return err
			}
		}
	case *slackapi.ButtonBlockElement:
		actionID = typed.ActionID
		if typed.Text == nil {
			return errors.New("button text is required")
		}
		if err := validateCompiledText(typed.Text, maxRendererCompositionTextLength, "button.text"); err != nil {
			return err
		}
	default:
		return fmt.Errorf("compiled element type %T is not allowed", element)
	}
	if err := validateLiteralID(actionID, "action_id"); err != nil {
		return err
	}
	if _, exists := (*actionIDs)[actionID]; exists {
		return fmt.Errorf("duplicate compiled action ID %q", actionID)
	}
	(*actionIDs)[actionID] = struct{}{}
	if modal {
		if !allowedModalActionID(actionID) {
			return fmt.Errorf("unregistered compiled modal action ID %q", actionID)
		}
	} else if !allowedMessageActionIDs[actionID] {
		return fmt.Errorf("unregistered compiled message action ID %q", actionID)
	}
	return nil
}

func validateCompiledOption(option *slackapi.OptionBlockObject) error {
	if option == nil || option.Text == nil {
		return errors.New("compiled option text is required")
	}
	if err := validateCompiledText(option.Text, maxRendererOptionTextLength, "option.text"); err != nil {
		return err
	}
	if utf8.RuneCountInString(option.Value) > maxRendererOptionValueLength {
		return fmt.Errorf("option value exceeds %d character limit", maxRendererOptionValueLength)
	}
	if option.Description != nil {
		if err := validateCompiledText(option.Description, maxRendererOptionTextLength, "option.description"); err != nil {
			return err
		}
	}
	return nil
}

func validateCompiledText(text *slackapi.TextBlockObject, limit int, fieldPath string) error {
	if text == nil {
		return fmt.Errorf("%s is required", fieldPath)
	}
	if text.Type != "plain_text" && text.Type != "mrkdwn" {
		return fmt.Errorf("%s has invalid type %q", fieldPath, text.Type)
	}
	if text.Text == "" {
		return fmt.Errorf("%s must not be empty", fieldPath)
	}
	if utf8.RuneCountInString(text.Text) > limit {
		return fmt.Errorf("%s exceeds %d character limit", fieldPath, limit)
	}
	return nil
}
