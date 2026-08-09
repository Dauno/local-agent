package slack

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Dauno/slack-local-agent/internal/agentdef"
	"github.com/Dauno/slack-local-agent/internal/domain"
)

// The embedded catalog is immutable. A process must not be able to replace a
// layout with data from configuration or an external request.
//
//go:embed templates/*.json
var embeddedTemplates embed.FS

const templateSchemaVersion = 1

var requiredTemplateNames = []string{
	"agent_preview",
	"builder_modal",
	"confirmation_message",
	"onboarding_message",
}

var requiredTemplateSet = map[string]struct{}{
	"agent_preview":        {},
	"builder_modal":        {},
	"confirmation_message": {},
	"onboarding_message":   {},
}

// TemplateCatalog is the validated, immutable set of declarative Slack
// templates. Its contents are intentionally not exposed as mutable JSON.
type TemplateCatalog struct {
	templates map[string]templateDocument
}

// TemplateInfo describes one catalog entry without exposing its layout AST.
type TemplateInfo struct {
	Name          string
	SchemaVersion int
	Surface       string
}

// TemplateInteractiveIDs is the complete interactive-ID inventory extracted
// from the validated catalog. Builder and message block IDs are kept separate
// from action IDs because text inputs have action IDs but do not dispatch
// listener actions.
type TemplateInteractiveIDs struct {
	ModalCallbacks []string
	Actions        []string
	BuilderBlocks  []string
	MessageBlocks  []string
}

// EmbeddedTemplateCatalog returns the package's startup-validated catalog.
func EmbeddedTemplateCatalog() (*TemplateCatalog, error) {
	return embeddedTemplateCatalog, embeddedTemplateCatalogErr
}

// LoadTemplateCatalog loads and validates the embedded catalog. Loading is
// repeatable and has no external side effects, which keeps renderer tests
// hermetic while retaining the same startup validation path.
func LoadTemplateCatalog() (*TemplateCatalog, error) {
	return loadTemplateCatalog(embeddedTemplates)
}

// LoadTemplateCatalogFromFS is intended for hermetic validation tests. Runtime
// code should use LoadTemplateCatalog so templates remain embedded.
func LoadTemplateCatalogFromFS(fsys fs.FS) (*TemplateCatalog, error) {
	return loadTemplateCatalog(fsys)
}

// Names returns the fixed catalog names in deterministic order.
func (c *TemplateCatalog) Names() []string {
	if c == nil {
		return nil
	}
	names := make([]string, 0, len(c.templates))
	for name := range c.templates {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Has reports whether name is in the validated catalog.
func (c *TemplateCatalog) Has(name string) bool {
	if c == nil {
		return false
	}
	_, ok := c.templates[name]
	return ok
}

// Info returns metadata for a validated catalog entry.
func (c *TemplateCatalog) Info(name string) (TemplateInfo, bool) {
	if c == nil {
		return TemplateInfo{}, false
	}
	doc, ok := c.templates[name]
	if !ok {
		return TemplateInfo{}, false
	}
	return TemplateInfo{Name: doc.Name, SchemaVersion: doc.SchemaVersion, Surface: doc.Surface}, true
}

// InteractiveIDs extracts the literal IDs from every validated template in a
// deterministic order. The returned slices are owned by the caller.
func (c *TemplateCatalog) InteractiveIDs() TemplateInteractiveIDs {
	if c == nil {
		return TemplateInteractiveIDs{}
	}
	ids := TemplateInteractiveIDs{}
	for _, name := range c.Names() {
		doc := c.templates[name]
		if doc.Modal != nil {
			appendTemplateID(&ids.ModalCallbacks, doc.Modal.CallbackID)
			collectTemplateBlockIDs(&ids, doc.Modal.Blocks, true)
			continue
		}
		if doc.Message != nil {
			collectTemplateBlockIDs(&ids, doc.Message.Blocks, false)
		}
	}
	sort.Strings(ids.ModalCallbacks)
	sort.Strings(ids.Actions)
	sort.Strings(ids.BuilderBlocks)
	sort.Strings(ids.MessageBlocks)
	return ids
}

// ValidateDispatcher verifies catalog coverage and rejects registered IDs
// outside the allowlist before Socket Mode starts.
func (c *TemplateCatalog) ValidateDispatcher(dispatcher *InteractiveDispatcher) error {
	if c == nil {
		return errors.New("template catalog is required for dispatcher validation")
	}
	if dispatcher == nil {
		return errors.New("interactive dispatcher is required for template validation")
	}
	ids := c.InteractiveIDs()
	for _, callbackID := range ids.ModalCallbacks {
		if !allowedModalCallbackIDs[callbackID] {
			return fmt.Errorf("template callback ID %q is not in the allowlist", callbackID)
		}
		if !dispatcher.HasView(callbackID) {
			return fmt.Errorf("template callback ID %q has no registered view handler", callbackID)
		}
	}
	for _, actionID := range ids.Actions {
		if !allowedInteractiveActionIDs[actionID] && !allowedMessageActionIDs[actionID] {
			return fmt.Errorf("template action ID %q is not in the allowlist", actionID)
		}
		if !dispatcher.HasAction(actionID) {
			return fmt.Errorf("template action ID %q has no registered block-action handler", actionID)
		}
	}
	for _, blockID := range ids.BuilderBlocks {
		if !allowedBuilderBlockIDs[blockID] {
			return fmt.Errorf("template builder block ID %q is not in the allowlist", blockID)
		}
	}
	for _, blockID := range ids.MessageBlocks {
		if !allowedMessageBlockIDs[blockID] {
			return fmt.Errorf("template message block ID %q is not in the allowlist", blockID)
		}
	}
	for _, callbackID := range dispatcher.RegisteredViewIDs() {
		if !allowedModalCallbackIDs[callbackID] {
			return fmt.Errorf("registered callback ID %q is not in the allowlist", callbackID)
		}
	}
	for _, actionID := range dispatcher.RegisteredActionIDs() {
		if !allowedInteractiveActionIDs[actionID] {
			return fmt.Errorf("registered action ID %q is not in the allowlist", actionID)
		}
	}
	return nil
}

func collectTemplateBlockIDs(ids *TemplateInteractiveIDs, blocks []templateBlock, modal bool) {
	for _, block := range blocks {
		if block.BlockID != "" {
			if modal {
				appendTemplateID(&ids.BuilderBlocks, block.BlockID)
			} else {
				appendTemplateID(&ids.MessageBlocks, block.BlockID)
			}
		}
		collectTemplateElementID(ids, block.Accessory, modal)
		collectTemplateElementID(ids, block.Element, modal)
		for _, element := range block.Elements {
			collectTemplateElementID(ids, element, modal)
		}
	}
}

func collectTemplateElementID(ids *TemplateInteractiveIDs, element *templateElement, modal bool) {
	if element == nil {
		return
	}
	if (modal && allowedInteractiveActionIDs[element.ActionID]) || (!modal && allowedMessageActionIDs[element.ActionID]) {
		appendTemplateID(&ids.Actions, element.ActionID)
	}
}

func appendTemplateID(ids *[]string, value string) {
	if value == "" {
		return
	}
	for _, existing := range *ids {
		if existing == value {
			return
		}
	}
	*ids = append(*ids, value)
}

var embeddedTemplateCatalog, embeddedTemplateCatalogErr = loadTemplateCatalog(embeddedTemplates)

type templateDocument struct {
	Name          string
	SchemaVersion int
	Surface       string
	Modal         *templateModalPayload
	Message       *templateMessagePayload
}

type templateModalPayload struct {
	Type            string
	Title           *templateText
	Submit          *templateText
	Close           *templateText
	CallbackID      string
	PrivateMetadata string
	Blocks          []templateBlock
}

type templateMessagePayload struct {
	FallbackText string
	Blocks       []templateBlock
}

type templateText struct {
	Type     string
	Text     string
	Emoji    *bool
	Verbatim bool
}

type templateOption struct {
	Text        *templateText
	Value       string
	Description *templateText
	URL         string
}

type templateDispatchActionConfig struct {
	TriggerActionsOn []string
}

type templateElement struct {
	Type                 string
	ActionID             string
	Text                 *templateText
	Value                string
	URL                  string
	Style                string
	Placeholder          *templateText
	InitialValue         string
	InitialValuePresent  bool
	Multiline            bool
	MinLength            int
	MaxLength            int
	DispatchActionConfig *templateDispatchActionConfig
	FocusOnLoad          bool
	Options              []templateOption
	OptionsToken         string
	OptionsPresent       bool
	InitialOption        *templateOption
	InitialOptionToken   string
	InitialOptionPresent bool
}

type templateBlock struct {
	Condition       string
	CollectionToken string
	Type            string
	BlockID         string
	Text            *templateText
	Fields          []*templateText
	Accessory       *templateElement
	Label           *templateText
	Hint            *templateText
	Element         *templateElement
	Optional        bool
	DispatchAction  bool
	Elements        []*templateElement
}

type rawTemplateDocument struct {
	Name          string          `json:"name"`
	SchemaVersion int             `json:"schema_version"`
	Surface       string          `json:"surface"`
	Payload       json.RawMessage `json:"payload"`
}

type rawModalPayload struct {
	Type            string            `json:"type"`
	Title           json.RawMessage   `json:"title"`
	Submit          json.RawMessage   `json:"submit"`
	Close           json.RawMessage   `json:"close"`
	CallbackID      string            `json:"callback_id"`
	PrivateMetadata string            `json:"private_metadata"`
	Blocks          []json.RawMessage `json:"blocks"`
}

type rawMessagePayload struct {
	FallbackText string            `json:"fallback_text"`
	Blocks       []json.RawMessage `json:"blocks"`
}

type rawText struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Emoji    *bool  `json:"emoji"`
	Verbatim bool   `json:"verbatim"`
}

type rawOption struct {
	Text        json.RawMessage `json:"text"`
	Value       string          `json:"value"`
	Description json.RawMessage `json:"description"`
	URL         string          `json:"url"`
}

type rawDispatchActionConfig struct {
	TriggerActionsOn []string `json:"trigger_actions_on"`
}

type rawElement struct {
	Type                 string          `json:"type"`
	ActionID             string          `json:"action_id"`
	Text                 json.RawMessage `json:"text"`
	Value                string          `json:"value"`
	URL                  string          `json:"url"`
	Style                string          `json:"style"`
	Placeholder          json.RawMessage `json:"placeholder"`
	InitialValue         json.RawMessage `json:"initial_value"`
	Multiline            bool            `json:"multiline"`
	MinLength            int             `json:"min_length"`
	MaxLength            int             `json:"max_length"`
	DispatchActionConfig json.RawMessage `json:"dispatch_action_config"`
	FocusOnLoad          bool            `json:"focus_on_load"`
	Options              json.RawMessage `json:"options"`
	InitialOption        json.RawMessage `json:"initial_option"`
}

type rawBlock struct {
	Condition      string            `json:"$if"`
	Type           string            `json:"type"`
	BlockID        string            `json:"block_id"`
	Text           json.RawMessage   `json:"text"`
	Fields         []json.RawMessage `json:"fields"`
	Accessory      json.RawMessage   `json:"accessory"`
	Label          json.RawMessage   `json:"label"`
	Hint           json.RawMessage   `json:"hint"`
	Element        json.RawMessage   `json:"element"`
	Optional       bool              `json:"optional"`
	DispatchAction bool              `json:"dispatch_action"`
	Elements       []json.RawMessage `json:"elements"`
}

type templateTokenKind uint8

const (
	tokenValue templateTokenKind = iota + 1
	tokenOptions
)

type templateToken struct {
	Kind templateTokenKind
	Key  string
}

var scalarKeysByTemplate = map[string]map[string]struct{}{
	"builder_modal": {
		"name": {}, "description": {}, "instruction": {}, "agent_type": {},
		"model": {}, "execution_mode": {}, "timeout_seconds": {},
	},
	"confirmation_message": {
		"summary": {}, "original_call_id": {}, "expires_at": {}, "wrapper_call_id": {}, "fallback_text": {},
	},
	"agent_preview": {
		"name": {}, "agent_class": {}, "provider_profile": {}, "execution_mode": {},
		"timeout": {}, "yaml": {}, "sha256": {}, "draft_id": {}, "fallback_text": {},
	},
	"onboarding_message": {
		"builder_context": {}, "intro": {}, "describe_prompt": {},
	},
}

func loadTemplateCatalog(fsys fs.FS) (*TemplateCatalog, error) {
	if fsys == nil {
		return nil, errors.New("template filesystem is required")
	}
	files, err := fs.Glob(fsys, "templates/*.json")
	if err != nil {
		return nil, fmt.Errorf("glob embedded templates: %w", err)
	}
	sort.Strings(files)
	if len(files) != len(requiredTemplateNames) {
		return nil, fmt.Errorf("template catalog must contain exactly %d JSON files, found %d", len(requiredTemplateNames), len(files))
	}

	catalog := &TemplateCatalog{templates: make(map[string]templateDocument, len(files))}
	for _, filename := range files {
		base := path.Base(filename)
		fileName := strings.TrimSuffix(base, ".json")
		if _, ok := requiredTemplateSet[fileName]; !ok {
			return nil, fmt.Errorf("unknown template filename %q", base)
		}
		data, readErr := fs.ReadFile(fsys, filename)
		if readErr != nil {
			return nil, fmt.Errorf("read template %q: %w", base, readErr)
		}
		doc, parseErr := parseTemplateDocument(data)
		if parseErr != nil {
			return nil, fmt.Errorf("parse template %q: %w", base, parseErr)
		}
		if doc.Name != fileName {
			return nil, fmt.Errorf("template filename %q does not match document name %q", base, doc.Name)
		}
		if _, exists := catalog.templates[doc.Name]; exists {
			return nil, fmt.Errorf("duplicate template name %q", doc.Name)
		}
		catalog.templates[doc.Name] = doc
	}

	for _, name := range requiredTemplateNames {
		if _, ok := catalog.templates[name]; !ok {
			return nil, fmt.Errorf("required template %q is missing", name)
		}
	}
	for _, name := range requiredTemplateNames {
		if err := validateTemplateDocument(catalog.templates[name]); err != nil {
			return nil, fmt.Errorf("validate template %q: %w", name, err)
		}
	}
	for _, name := range requiredTemplateNames {
		if err := validateTemplateRepresentatives(catalog.templates[name]); err != nil {
			return nil, fmt.Errorf("compile template %q representative: %w", name, err)
		}
	}
	return catalog, nil
}

func parseTemplateDocument(data []byte) (templateDocument, error) {
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return templateDocument{}, err
	}
	var raw rawTemplateDocument
	if err := decodeStrictJSON(data, &raw); err != nil {
		return templateDocument{}, err
	}
	if raw.Name == "" {
		return templateDocument{}, errors.New("template name is required")
	}
	if raw.SchemaVersion != templateSchemaVersion {
		return templateDocument{}, fmt.Errorf("unsupported schema_version %d", raw.SchemaVersion)
	}
	if raw.Surface != "modal" && raw.Surface != "message" {
		return templateDocument{}, fmt.Errorf("unsupported surface %q", raw.Surface)
	}
	if len(raw.Payload) == 0 || string(raw.Payload) == "null" {
		return templateDocument{}, errors.New("payload is required")
	}

	doc := templateDocument{Name: raw.Name, SchemaVersion: raw.SchemaVersion, Surface: raw.Surface}
	switch raw.Surface {
	case "modal":
		payload, err := parseModalPayload(raw.Payload, raw.Name)
		if err != nil {
			return templateDocument{}, err
		}
		doc.Modal = &payload
	case "message":
		payload, err := parseMessagePayload(raw.Payload, raw.Name)
		if err != nil {
			return templateDocument{}, err
		}
		doc.Message = &payload
	}
	return doc, nil
}

func parseModalPayload(data []byte, templateName string) (templateModalPayload, error) {
	var raw rawModalPayload
	if err := decodeStrictJSON(data, &raw); err != nil {
		return templateModalPayload{}, fmt.Errorf("modal payload: %w", err)
	}
	if raw.Type != "modal" {
		return templateModalPayload{}, fmt.Errorf("modal payload type must be %q", "modal")
	}
	if len(raw.Title) == 0 {
		return templateModalPayload{}, errors.New("modal title is required")
	}
	title, err := parseText(raw.Title, templateName, "payload.title")
	if err != nil {
		return templateModalPayload{}, err
	}
	payload := templateModalPayload{
		Type:            raw.Type,
		Title:           title,
		CallbackID:      raw.CallbackID,
		PrivateMetadata: raw.PrivateMetadata,
	}
	if err := validateTemplateString(templateName, "payload.private_metadata", raw.PrivateMetadata, false, false); err != nil {
		return templateModalPayload{}, err
	}
	if len(raw.Submit) > 0 {
		payload.Submit, err = parseText(raw.Submit, templateName, "payload.submit")
		if err != nil {
			return templateModalPayload{}, err
		}
	}
	if len(raw.Close) > 0 {
		payload.Close, err = parseText(raw.Close, templateName, "payload.close")
		if err != nil {
			return templateModalPayload{}, err
		}
	}
	if raw.Blocks == nil {
		return templateModalPayload{}, errors.New("modal blocks are required")
	}
	payload.Blocks, err = parseBlocks(raw.Blocks, templateName)
	if err != nil {
		return templateModalPayload{}, err
	}
	return payload, nil
}

func parseMessagePayload(data []byte, templateName string) (templateMessagePayload, error) {
	var raw rawMessagePayload
	if err := decodeStrictJSON(data, &raw); err != nil {
		return templateMessagePayload{}, fmt.Errorf("message payload: %w", err)
	}
	if raw.Blocks == nil {
		return templateMessagePayload{}, errors.New("message blocks are required")
	}
	if strings.TrimSpace(raw.FallbackText) == "" {
		return templateMessagePayload{}, errors.New("message fallback_text is required")
	}
	if err := validateTemplateString(templateName, "payload.fallback_text", raw.FallbackText, true, false); err != nil {
		return templateMessagePayload{}, err
	}
	blocks, err := parseBlocks(raw.Blocks, templateName)
	if err != nil {
		return templateMessagePayload{}, err
	}
	return templateMessagePayload{FallbackText: raw.FallbackText, Blocks: blocks}, nil
}

func parseBlocks(rawBlocks []json.RawMessage, templateName string) ([]templateBlock, error) {
	blocks := make([]templateBlock, 0, len(rawBlocks))
	for index, rawBlock := range rawBlocks {
		var collectionToken string
		if err := decodeStrictJSON(rawBlock, &collectionToken); err == nil {
			token, isToken, tokenErr := parseTemplateString(collectionToken)
			if tokenErr != nil {
				return nil, fmt.Errorf("payload.blocks[%d]: %w", index, tokenErr)
			}
			if !isToken || token.Kind != tokenOptions {
				return nil, fmt.Errorf("payload.blocks[%d] must be a registered collection token", index)
			}
			if !allowedCollectionBlockToken(templateName, token.Key) {
				return nil, fmt.Errorf("payload.blocks[%d] uses unknown collection placeholder %q", index, token.Key)
			}
			blocks = append(blocks, templateBlock{CollectionToken: collectionToken})
			continue
		}
		block, err := parseBlock(rawBlock, templateName, fmt.Sprintf("payload.blocks[%d]", index))
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, block)
	}
	return blocks, nil
}

func parseBlock(data []byte, templateName, fieldPath string) (templateBlock, error) {
	keys, err := objectKeys(data)
	if err != nil {
		return templateBlock{}, fmt.Errorf("%s: %w", fieldPath, err)
	}
	var raw rawBlock
	if err := decodeStrictJSON(data, &raw); err != nil {
		return templateBlock{}, fmt.Errorf("%s: %w", fieldPath, err)
	}
	if raw.Type == "" {
		return templateBlock{}, fmt.Errorf("%s.type is required", fieldPath)
	}
	allowed := map[string]struct{}{"$if": {}, "type": {}, "block_id": {}}
	switch raw.Type {
	case "section":
		allowed["text"] = struct{}{}
		allowed["fields"] = struct{}{}
		allowed["accessory"] = struct{}{}
	case "input":
		allowed["label"] = struct{}{}
		allowed["hint"] = struct{}{}
		allowed["element"] = struct{}{}
		allowed["optional"] = struct{}{}
		allowed["dispatch_action"] = struct{}{}
	case "actions":
		allowed["elements"] = struct{}{}
	default:
		return templateBlock{}, fmt.Errorf("%s has unsupported block type %q", fieldPath, raw.Type)
	}
	if err := rejectUnknownObjectKeys(keys, allowed); err != nil {
		return templateBlock{}, fmt.Errorf("%s: %w", fieldPath, err)
	}
	if raw.Condition != "" && raw.Condition != "is_acp" {
		return templateBlock{}, fmt.Errorf("%s.$if has unknown condition %q", fieldPath, raw.Condition)
	}
	if err := validateLiteralString(raw.Type, templateName, fieldPath+".type"); err != nil {
		return templateBlock{}, err
	}
	if raw.BlockID != "" {
		if err := validateLiteralID(raw.BlockID, fieldPath+".block_id"); err != nil {
			return templateBlock{}, err
		}
	}

	block := templateBlock{Condition: raw.Condition, Type: raw.Type, BlockID: raw.BlockID, Optional: raw.Optional, DispatchAction: raw.DispatchAction}
	switch raw.Type {
	case "section":
		if len(raw.Text) == 0 && len(raw.Fields) == 0 {
			return templateBlock{}, fmt.Errorf("%s requires text or fields", fieldPath)
		}
		if len(raw.Fields) > maxRendererSectionFields {
			return templateBlock{}, fmt.Errorf("%s.fields exceeds %d items", fieldPath, maxRendererSectionFields)
		}
		if len(raw.Text) > 0 {
			block.Text, err = parseText(raw.Text, templateName, fieldPath+".text")
			if err != nil {
				return templateBlock{}, err
			}
		}
		if raw.Fields != nil {
			block.Fields = make([]*templateText, 0, len(raw.Fields))
			for index, field := range raw.Fields {
				text, parseErr := parseText(field, templateName, fmt.Sprintf("%s.fields[%d]", fieldPath, index))
				if parseErr != nil {
					return templateBlock{}, parseErr
				}
				block.Fields = append(block.Fields, text)
			}
		}
		if len(raw.Accessory) > 0 {
			block.Accessory, err = parseElement(raw.Accessory, templateName, fieldPath+".accessory")
			if err != nil {
				return templateBlock{}, err
			}
		}
	case "input":
		if raw.BlockID == "" {
			return templateBlock{}, fmt.Errorf("%s.block_id is required for input blocks", fieldPath)
		}
		if len(raw.Label) == 0 || len(raw.Element) == 0 {
			return templateBlock{}, fmt.Errorf("%s requires label and element", fieldPath)
		}
		block.Label, err = parseText(raw.Label, templateName, fieldPath+".label")
		if err != nil {
			return templateBlock{}, err
		}
		if len(raw.Hint) > 0 {
			block.Hint, err = parseText(raw.Hint, templateName, fieldPath+".hint")
			if err != nil {
				return templateBlock{}, err
			}
		}
		block.Element, err = parseElement(raw.Element, templateName, fieldPath+".element")
		if err != nil {
			return templateBlock{}, err
		}
	case "actions":
		if raw.Elements == nil || len(raw.Elements) == 0 {
			return templateBlock{}, fmt.Errorf("%s.elements must not be empty", fieldPath)
		}
		if len(raw.Elements) > maxRendererActionElements {
			return templateBlock{}, fmt.Errorf("%s.elements exceeds %d items", fieldPath, maxRendererActionElements)
		}
		block.Elements = make([]*templateElement, 0, len(raw.Elements))
		for index, element := range raw.Elements {
			parsed, parseErr := parseElement(element, templateName, fmt.Sprintf("%s.elements[%d]", fieldPath, index))
			if parseErr != nil {
				return templateBlock{}, parseErr
			}
			block.Elements = append(block.Elements, parsed)
		}
	}
	return block, nil
}

func parseText(data []byte, templateName, fieldPath string) (*templateText, error) {
	var raw rawText
	if err := decodeStrictJSON(data, &raw); err != nil {
		return nil, fmt.Errorf("%s: %w", fieldPath, err)
	}
	if raw.Type != "plain_text" && raw.Type != "mrkdwn" {
		return nil, fmt.Errorf("%s.type must be plain_text or mrkdwn", fieldPath)
	}
	if raw.Type == "mrkdwn" && raw.Emoji != nil {
		return nil, fmt.Errorf("%s.emoji is not valid for mrkdwn", fieldPath)
	}
	if raw.Text == "" {
		return nil, fmt.Errorf("%s.text must not be empty", fieldPath)
	}
	if err := validateTemplateString(templateName, fieldPath+".text", raw.Text, true, false); err != nil {
		return nil, err
	}
	return &templateText{Type: raw.Type, Text: raw.Text, Emoji: raw.Emoji, Verbatim: raw.Verbatim}, nil
}

func parseOption(data []byte, templateName, fieldPath string) (templateOption, error) {
	keys, err := objectKeys(data)
	if err != nil {
		return templateOption{}, fmt.Errorf("%s: %w", fieldPath, err)
	}
	var raw rawOption
	if err := decodeStrictJSON(data, &raw); err != nil {
		return templateOption{}, fmt.Errorf("%s: %w", fieldPath, err)
	}
	if err := rejectUnknownObjectKeys(keys, map[string]struct{}{"text": {}, "value": {}, "description": {}, "url": {}}); err != nil {
		return templateOption{}, fmt.Errorf("%s: %w", fieldPath, err)
	}
	if len(raw.Text) == 0 || !containsObjectKey(keys, "text") {
		return templateOption{}, fmt.Errorf("%s.text is required", fieldPath)
	}
	if !containsObjectKey(keys, "value") {
		return templateOption{}, fmt.Errorf("%s.value is required", fieldPath)
	}
	if raw.Value == "" {
		if _, isToken, tokenErr := parseTemplateString(raw.Value); tokenErr != nil || !isToken {
			return templateOption{}, fmt.Errorf("%s.value must not be empty", fieldPath)
		}
	}
	text, err := parseText(raw.Text, templateName, fieldPath+".text")
	if err != nil {
		return templateOption{}, err
	}
	if err := validateTemplateString(templateName, fieldPath+".value", raw.Value, true, false); err != nil {
		return templateOption{}, err
	}
	option := templateOption{Text: text, Value: raw.Value, URL: raw.URL}
	if len(raw.Description) > 0 {
		option.Description, err = parseText(raw.Description, templateName, fieldPath+".description")
		if err != nil {
			return templateOption{}, err
		}
	}
	if err := validateTemplateString(templateName, fieldPath+".url", raw.URL, false, false); err != nil {
		return templateOption{}, err
	}
	return option, nil
}

func parseElement(data []byte, templateName, fieldPath string) (*templateElement, error) {
	keys, err := objectKeys(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", fieldPath, err)
	}
	var raw rawElement
	if err := decodeStrictJSON(data, &raw); err != nil {
		return nil, fmt.Errorf("%s: %w", fieldPath, err)
	}
	if raw.Type == "" {
		return nil, fmt.Errorf("%s.type is required", fieldPath)
	}
	allowed := map[string]struct{}{"type": {}, "action_id": {}}
	switch raw.Type {
	case "plain_text_input":
		for _, key := range []string{"placeholder", "initial_value", "multiline", "min_length", "max_length", "dispatch_action_config", "focus_on_load"} {
			allowed[key] = struct{}{}
		}
	case "static_select":
		for _, key := range []string{"placeholder", "options", "initial_option", "focus_on_load"} {
			allowed[key] = struct{}{}
		}
	case "button":
		for _, key := range []string{"text", "value", "url", "style"} {
			allowed[key] = struct{}{}
		}
	default:
		return nil, fmt.Errorf("%s has unsupported element type %q", fieldPath, raw.Type)
	}
	if err := rejectUnknownObjectKeys(keys, allowed); err != nil {
		return nil, fmt.Errorf("%s: %w", fieldPath, err)
	}
	if err := validateLiteralString(raw.Type, templateName, fieldPath+".type"); err != nil {
		return nil, err
	}
	element := &templateElement{Type: raw.Type, ActionID: raw.ActionID, Value: raw.Value, URL: raw.URL, Style: raw.Style, Multiline: raw.Multiline, MinLength: raw.MinLength, MaxLength: raw.MaxLength, FocusOnLoad: raw.FocusOnLoad}
	if raw.ActionID != "" {
		if err := validateLiteralID(raw.ActionID, fieldPath+".action_id"); err != nil {
			return nil, err
		}
	}

	switch raw.Type {
	case "plain_text_input":
		if raw.ActionID == "" {
			return nil, fmt.Errorf("%s.action_id is required", fieldPath)
		}
		if len(raw.Placeholder) > 0 {
			element.Placeholder, err = parseText(raw.Placeholder, templateName, fieldPath+".placeholder")
			if err != nil {
				return nil, err
			}
		}
		if len(raw.InitialValue) > 0 {
			if err := decodeStrictJSON(raw.InitialValue, &element.InitialValue); err != nil {
				return nil, fmt.Errorf("%s.initial_value: %w", fieldPath, err)
			}
			element.InitialValuePresent = true
			if err := validateTemplateString(templateName, fieldPath+".initial_value", element.InitialValue, true, false); err != nil {
				return nil, err
			}
		}
		if element.MinLength < 0 || element.MaxLength < 0 {
			return nil, fmt.Errorf("%s input lengths must not be negative", fieldPath)
		}
		if element.MaxLength > maxRendererPlainTextInputLength {
			return nil, fmt.Errorf("%s.max_length exceeds %d", fieldPath, maxRendererPlainTextInputLength)
		}
		if element.MinLength > 0 && element.MaxLength > 0 && element.MinLength > element.MaxLength {
			return nil, fmt.Errorf("%s.min_length exceeds max_length", fieldPath)
		}
		if len(raw.DispatchActionConfig) > 0 {
			var config rawDispatchActionConfig
			if err := decodeStrictJSON(raw.DispatchActionConfig, &config); err != nil {
				return nil, fmt.Errorf("%s.dispatch_action_config: %w", fieldPath, err)
			}
			element.DispatchActionConfig = &templateDispatchActionConfig{TriggerActionsOn: append([]string(nil), config.TriggerActionsOn...)}
		}
	case "static_select":
		if raw.ActionID == "" {
			return nil, fmt.Errorf("%s.action_id is required", fieldPath)
		}
		if len(raw.Placeholder) > 0 {
			element.Placeholder, err = parseText(raw.Placeholder, templateName, fieldPath+".placeholder")
			if err != nil {
				return nil, err
			}
		}
		if len(raw.Options) == 0 {
			return nil, fmt.Errorf("%s.options is required", fieldPath)
		}
		element.OptionsPresent = true
		var optionsToken string
		if err := decodeStrictJSON(raw.Options, &optionsToken); err == nil {
			token, ok, tokenErr := parseTemplateString(optionsToken)
			if tokenErr != nil {
				return nil, fmt.Errorf("%s.options: %w", fieldPath, tokenErr)
			}
			if !ok || token.Kind != tokenOptions || token.Key != "model" || templateName != "builder_modal" {
				return nil, fmt.Errorf("%s.options must be the exact registered token {{options.model}}", fieldPath)
			}
			element.OptionsToken = optionsToken
		} else {
			var rawOptions []json.RawMessage
			if err := decodeStrictJSON(raw.Options, &rawOptions); err != nil {
				return nil, fmt.Errorf("%s.options must be an array or {{options.model}}: %w", fieldPath, err)
			}
			element.Options = make([]templateOption, 0, len(rawOptions))
			for index, rawOptionData := range rawOptions {
				option, parseErr := parseOption(rawOptionData, templateName, fmt.Sprintf("%s.options[%d]", fieldPath, index))
				if parseErr != nil {
					return nil, parseErr
				}
				element.Options = append(element.Options, option)
			}
		}
		if len(element.Options) > maxRendererStaticSelectOptions {
			return nil, fmt.Errorf("%s.options exceeds %d items", fieldPath, maxRendererStaticSelectOptions)
		}
		if len(raw.InitialOption) > 0 {
			element.InitialOptionPresent = true
			var tokenString string
			if err := decodeStrictJSON(raw.InitialOption, &tokenString); err == nil {
				token, ok, tokenErr := parseTemplateString(tokenString)
				if tokenErr != nil {
					return nil, fmt.Errorf("%s.initial_option: %w", fieldPath, tokenErr)
				}
				if !ok || token.Kind != tokenValue {
					return nil, fmt.Errorf("%s.initial_option must be an exact scalar token or option object", fieldPath)
				}
				if _, allowed := scalarKeysByTemplate[templateName][token.Key]; !allowed {
					return nil, fmt.Errorf("%s.initial_option uses unknown scalar token %q", fieldPath, token.Key)
				}
				element.InitialOptionToken = tokenString
			} else {
				option, parseErr := parseOption(raw.InitialOption, templateName, fieldPath+".initial_option")
				if parseErr != nil {
					return nil, parseErr
				}
				element.InitialOption = &option
			}
		}
	case "button":
		if raw.ActionID == "" {
			return nil, fmt.Errorf("%s.action_id is required", fieldPath)
		}
		if len(raw.Text) == 0 {
			return nil, fmt.Errorf("%s.text is required", fieldPath)
		}
		element.Text, err = parseText(raw.Text, templateName, fieldPath+".text")
		if err != nil {
			return nil, err
		}
		if err := validateTemplateString(templateName, fieldPath+".value", raw.Value, true, false); err != nil {
			return nil, err
		}
		if err := validateTemplateString(templateName, fieldPath+".url", raw.URL, false, false); err != nil {
			return nil, err
		}
		if raw.Style != "" && raw.Style != "primary" && raw.Style != "danger" {
			return nil, fmt.Errorf("%s.style must be primary or danger", fieldPath)
		}
	}
	return element, nil
}

func validateTemplateDocument(doc templateDocument) error {
	if _, ok := requiredTemplateSet[doc.Name]; !ok {
		return fmt.Errorf("unknown template name %q", doc.Name)
	}
	if err := validateScalarPlacements(doc); err != nil {
		return err
	}
	wantSurface := "message"
	if doc.Name == "builder_modal" {
		wantSurface = "modal"
	}
	if doc.Surface != wantSurface {
		return fmt.Errorf("template %q must use %s surface", doc.Name, wantSurface)
	}

	if doc.Modal != nil {
		modal := doc.Modal
		if err := validateModalText(modal.Title, maxRendererModalTitleLength, "title"); err != nil {
			return err
		}
		if modal.Submit != nil {
			if err := validateModalText(modal.Submit, maxRendererModalSubmitCloseLength, "submit"); err != nil {
				return err
			}
		}
		if modal.Close != nil {
			if err := validateModalText(modal.Close, maxRendererModalSubmitCloseLength, "close"); err != nil {
				return err
			}
		}
		if err := validateLiteralID(modal.CallbackID, "callback_id"); err != nil {
			return err
		}
		if !allowedModalCallbackIDs[modal.CallbackID] {
			return fmt.Errorf("unregistered modal callback ID %q", modal.CallbackID)
		}
		if len(modal.Blocks) > maxRendererBlocksPerModal {
			return fmt.Errorf("modal exceeds %d block limit", maxRendererBlocksPerModal)
		}
		return validateBlocks(doc, modal.Blocks, true)
	}

	if doc.Message == nil {
		return errors.New("template payload is missing")
	}
	if utf8.RuneCountInString(doc.Message.FallbackText) > maxFallbackText {
		return fmt.Errorf("fallback_text exceeds %d character limit", maxFallbackText)
	}
	if len(doc.Message.Blocks) > maxBlocksPerMessage {
		return fmt.Errorf("message exceeds %d block limit", maxBlocksPerMessage)
	}
	return validateBlocks(doc, doc.Message.Blocks, false)
}

type scalarPlacement string

const (
	placementFallback      scalarPlacement = "fallback_text"
	placementSectionText   scalarPlacement = "section.text"
	placementSectionField  scalarPlacement = "section.fields"
	placementElementText   scalarPlacement = "element.text"
	placementElementValue  scalarPlacement = "element.value"
	placementInitialValue  scalarPlacement = "element.initial_value"
	placementInitialOption scalarPlacement = "element.initial_option"
	placementOther         scalarPlacement = "other"
)

func validateScalarPlacements(doc templateDocument) error {
	counts := make(map[string]int)
	validate := func(value string, placement scalarPlacement, actionID string) error {
		token, isToken, err := parseTemplateString(value)
		if err != nil || !isToken || token.Kind != tokenValue {
			return err
		}
		if !allowedScalarPlacement(doc.Name, token.Key, placement, actionID) {
			return fmt.Errorf("scalar placeholder %q is not allowed at %s", token.Key, placement)
		}
		counts[token.Key]++
		return nil
	}
	validateText := func(text *templateText, placement scalarPlacement) error {
		if text == nil {
			return nil
		}
		return validate(text.Text, placement, "")
	}
	var validateElement func(*templateElement) error
	validateElement = func(element *templateElement) error {
		if element == nil {
			return nil
		}
		if element.Text != nil {
			if err := validate(element.Text.Text, placementElementText, element.ActionID); err != nil {
				return err
			}
		}
		for _, text := range []*templateText{element.Placeholder} {
			if err := validateText(text, placementOther); err != nil {
				return err
			}
		}
		if err := validate(element.Value, placementElementValue, element.ActionID); err != nil {
			return err
		}
		if element.InitialValuePresent {
			if err := validate(element.InitialValue, placementInitialValue, element.ActionID); err != nil {
				return err
			}
		}
		if element.InitialOptionToken != "" {
			if err := validate(element.InitialOptionToken, placementInitialOption, element.ActionID); err != nil {
				return err
			}
		}
		if element.InitialOption != nil {
			if err := validateText(element.InitialOption.Text, placementOther); err != nil {
				return err
			}
			if err := validate(element.InitialOption.Value, placementOther, element.ActionID); err != nil {
				return err
			}
			if err := validateText(element.InitialOption.Description, placementOther); err != nil {
				return err
			}
		}
		for _, option := range element.Options {
			if err := validateText(option.Text, placementOther); err != nil {
				return err
			}
			if err := validate(option.Value, placementOther, element.ActionID); err != nil {
				return err
			}
			if err := validateText(option.Description, placementOther); err != nil {
				return err
			}
		}
		return nil
	}
	validateBlocks := func(blocks []templateBlock) error {
		for _, block := range blocks {
			if err := validateText(block.Text, placementSectionText); err != nil {
				return err
			}
			for _, field := range block.Fields {
				if err := validateText(field, placementSectionField); err != nil {
					return err
				}
			}
			for _, text := range []*templateText{block.Label, block.Hint} {
				if err := validateText(text, placementOther); err != nil {
					return err
				}
			}
			if err := validateElement(block.Accessory); err != nil {
				return err
			}
			if err := validateElement(block.Element); err != nil {
				return err
			}
			for _, element := range block.Elements {
				if err := validateElement(element); err != nil {
					return err
				}
			}
		}
		return nil
	}

	if doc.Modal != nil {
		for _, text := range []*templateText{doc.Modal.Title, doc.Modal.Submit, doc.Modal.Close} {
			if err := validateText(text, placementOther); err != nil {
				return err
			}
		}
		if err := validateBlocks(doc.Modal.Blocks); err != nil {
			return err
		}
	} else if doc.Message != nil {
		if err := validate(doc.Message.FallbackText, placementFallback, ""); err != nil {
			return err
		}
		if err := validateBlocks(doc.Message.Blocks); err != nil {
			return err
		}
	}
	for key, want := range requiredScalarOccurrences[doc.Name] {
		if counts[key] != want {
			return fmt.Errorf("scalar placeholder %q must occur %d times, found %d", key, want, counts[key])
		}
	}
	return nil
}

func allowedScalarPlacement(templateName, key string, placement scalarPlacement, actionID string) bool {
	switch templateName {
	case "builder_modal":
		if key == actionID {
			return placement == placementInitialValue || placement == placementInitialOption
		}
	case "confirmation_message":
		switch key {
		case "fallback_text":
			return placement == placementFallback
		case "summary":
			return placement == placementSectionText
		case "original_call_id", "expires_at":
			return placement == placementSectionField
		case "wrapper_call_id":
			return placement == placementElementValue && (actionID == approveActionID || actionID == rejectActionID)
		}
	case "agent_preview":
		switch key {
		case "fallback_text":
			return placement == placementFallback
		case "name", "agent_class", "provider_profile", "execution_mode", "timeout", "sha256":
			return placement == placementSectionText
		case "draft_id":
			return placement == placementElementValue && actionID == builderInstallActionID
		}
	case "onboarding_message":
		switch key {
		case "intro":
			return placement == placementSectionText
		case "describe_prompt":
			return placement == placementElementText && actionID == "local_agent.onboarding.describe"
		case "builder_context":
			return placement == placementElementValue && (actionID == "local_agent.builder.open" || actionID == "local_agent.onboarding.describe")
		}
	}
	return false
}

var requiredScalarOccurrences = map[string]map[string]int{
	"builder_modal": {
		"name": 1, "description": 1, "instruction": 1, "agent_type": 1,
		"model": 1, "execution_mode": 1, "timeout_seconds": 1,
	},
	"confirmation_message": {
		"summary": 1, "original_call_id": 1, "expires_at": 1, "wrapper_call_id": 2, "fallback_text": 1,
	},
	"agent_preview": {
		"name": 1, "agent_class": 1, "sha256": 1, "draft_id": 1, "fallback_text": 1,
	},
	"onboarding_message": {
		"builder_context": 2, "intro": 1, "describe_prompt": 1,
	},
}

func validateModalText(text *templateText, limit int, field string) error {
	if text == nil {
		return fmt.Errorf("modal %s is required", field)
	}
	if text.Type != "plain_text" {
		return fmt.Errorf("modal %s must be plain_text", field)
	}
	if literal, ok := literalTemplateString(text.Text); ok && utf8.RuneCountInString(literal) > limit {
		return fmt.Errorf("modal %s exceeds %d character limit", field, limit)
	}
	return nil
}

func validateBlocks(doc templateDocument, blocks []templateBlock, modal bool) error {
	blockIDs := make(map[string]struct{}, len(blocks))
	actionIDs := make(map[string]struct{})
	collectionTokens := make(map[string]struct{})
	for index, block := range blocks {
		fieldPath := fmt.Sprintf("blocks[%d]", index)
		if block.CollectionToken != "" {
			if modal {
				return fmt.Errorf("%s collection injection is not valid on modal surface", fieldPath)
			}
			token, isToken, err := parseTemplateString(block.CollectionToken)
			if err != nil || !isToken || token.Kind != tokenOptions || !allowedCollectionBlockToken(doc.Name, token.Key) {
				return fmt.Errorf("%s uses an invalid collection injection", fieldPath)
			}
			if _, exists := collectionTokens[token.Key]; exists {
				return fmt.Errorf("duplicate collection injection %q", token.Key)
			}
			collectionTokens[token.Key] = struct{}{}
			continue
		}
		if block.BlockID != "" {
			if _, exists := blockIDs[block.BlockID]; exists {
				return fmt.Errorf("duplicate block ID %q", block.BlockID)
			}
			blockIDs[block.BlockID] = struct{}{}
			if modal {
				if !allowedBuilderBlockIDs[block.BlockID] {
					return fmt.Errorf("unregistered builder block ID %q", block.BlockID)
				}
			} else if !allowedMessageBlockIDs[block.BlockID] {
				return fmt.Errorf("unregistered message block ID %q", block.BlockID)
			}
		}
		if block.Condition != "" && block.Condition != "is_acp" {
			return fmt.Errorf("%s has unknown condition %q", fieldPath, block.Condition)
		}
		switch block.Type {
		case "section":
			if len(block.Fields) > maxRendererSectionFields {
				return fmt.Errorf("%s.fields exceeds %d items", fieldPath, maxRendererSectionFields)
			}
			if block.Text != nil {
				if err := validateBlockText(doc.Name, block.Text, fieldPath+".text"); err != nil {
					return err
				}
			}
			for fieldIndex, field := range block.Fields {
				if err := validateBlockText(doc.Name, field, fmt.Sprintf("%s.fields[%d]", fieldPath, fieldIndex)); err != nil {
					return err
				}
			}
			if block.Accessory != nil {
				if modal && block.Accessory.Type == "plain_text_input" {
					return fmt.Errorf("%s.accessory cannot be a modal input", fieldPath)
				}
				if err := validateElement(doc, block.Accessory, modal, map[string]bool{"button": true, "static_select": true}, &actionIDs, fieldPath+".accessory"); err != nil {
					return err
				}
			}
		case "input":
			if !modal {
				return fmt.Errorf("input blocks are not valid on message surface")
			}
			if block.Label == nil || block.Element == nil {
				return fmt.Errorf("%s requires label and element", fieldPath)
			}
			if err := validateTemplateTextLimit(doc.Name, block.Label, fieldPath+".label", maxRendererModalLabelLength); err != nil {
				return err
			}
			if block.Hint != nil {
				if err := validateTemplateTextLimit(doc.Name, block.Hint, fieldPath+".hint", maxRendererModalHintLength); err != nil {
					return err
				}
			}
			if err := validateBuilderInputLimit(doc.Name, block); err != nil {
				return err
			}
			if err := validateElement(doc, block.Element, modal, map[string]bool{"plain_text_input": true, "static_select": true}, &actionIDs, fieldPath+".element"); err != nil {
				return err
			}
		case "actions":
			if modal && len(block.Elements) == 0 {
				return fmt.Errorf("%s.elements must not be empty", fieldPath)
			}
			if len(block.Elements) > maxRendererActionElements {
				return fmt.Errorf("%s.elements exceeds %d items", fieldPath, maxRendererActionElements)
			}
			for elementIndex, element := range block.Elements {
				if err := validateElement(doc, element, modal, map[string]bool{"button": true, "static_select": true}, &actionIDs, fmt.Sprintf("%s.elements[%d]", fieldPath, elementIndex)); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("%s has unsupported block type %q", fieldPath, block.Type)
		}
	}
	return nil
}

func validateBuilderInputLimit(templateName string, block templateBlock) error {
	if templateName != "builder_modal" || block.Element == nil || block.Element.Type != "plain_text_input" {
		return nil
	}
	want := 0
	switch block.BlockID {
	case "description":
		want = agentdef.MaxDescriptionLength
	case "instruction":
		want = agentdef.MaxInstructionLength
	case "timeout_seconds":
		want = len(strconv.Itoa(domain.MaxACPTimeoutSeconds))
	}
	if want > 0 && block.Element.MaxLength != want {
		return fmt.Errorf("builder %s max_length must be %d", block.BlockID, want)
	}
	return nil
}

func validateBlockText(templateName string, text *templateText, fieldPath string) error {
	if text == nil {
		return fmt.Errorf("%s is required", fieldPath)
	}
	if text.Type != "plain_text" && text.Type != "mrkdwn" {
		return fmt.Errorf("%s has invalid text type %q", fieldPath, text.Type)
	}
	if literal, ok := literalTemplateString(text.Text); ok {
		limit := maxRendererCompositionTextLength
		if templateName == "agent_preview" && text.Type == "mrkdwn" {
			limit = builderBlockTextLimit
		}
		if utf8.RuneCountInString(literal) > limit {
			return fmt.Errorf("%s exceeds %d character limit", fieldPath, limit)
		}
	}
	return nil
}

func validateTemplateTextLimit(templateName string, text *templateText, fieldPath string, limit int) error {
	if err := validateBlockText(templateName, text, fieldPath); err != nil {
		return err
	}
	if literal, ok := literalTemplateString(text.Text); ok && utf8.RuneCountInString(literal) > limit {
		return fmt.Errorf("%s exceeds %d character limit", fieldPath, limit)
	}
	return nil
}

func validateElement(doc templateDocument, element *templateElement, modal bool, allowedTypes map[string]bool, actionIDs *map[string]struct{}, fieldPath string) error {
	if element == nil {
		return fmt.Errorf("%s is required", fieldPath)
	}
	if !allowedTypes[element.Type] {
		return fmt.Errorf("%s element type %q is not valid for its parent block", fieldPath, element.Type)
	}
	if !modal && element.Type == "plain_text_input" {
		return fmt.Errorf("%s input element is not valid on message surface", fieldPath)
	}
	if element.ActionID == "" {
		return fmt.Errorf("%s.action_id is required", fieldPath)
	}
	if _, exists := (*actionIDs)[element.ActionID]; exists {
		return fmt.Errorf("duplicate action ID %q", element.ActionID)
	}
	(*actionIDs)[element.ActionID] = struct{}{}
	if modal {
		if !allowedModalActionID(element.ActionID) {
			return fmt.Errorf("unregistered modal action ID %q", element.ActionID)
		}
	} else if !allowedMessageActionIDs[element.ActionID] {
		return fmt.Errorf("unregistered message action ID %q", element.ActionID)
	}

	switch element.Type {
	case "plain_text_input":
		if element.Placeholder != nil {
			if err := validateTemplateTextLimit(doc.Name, element.Placeholder, fieldPath+".placeholder", maxRendererPlaceholderLength); err != nil {
				return err
			}
		}
		if element.MaxLength > maxRendererPlainTextInputLength {
			return fmt.Errorf("%s.max_length exceeds %d", fieldPath, maxRendererPlainTextInputLength)
		}
		if element.InitialValuePresent {
			limit := maxRendererPlainTextInputLength
			if element.MaxLength > 0 {
				limit = element.MaxLength
			}
			if literal, ok := literalTemplateString(element.InitialValue); ok && utf8.RuneCountInString(literal) > limit {
				return fmt.Errorf("%s.initial_value exceeds input length limit", fieldPath)
			}
		}
	case "static_select":
		if element.Placeholder != nil {
			if err := validateTemplateTextLimit(doc.Name, element.Placeholder, fieldPath+".placeholder", maxRendererPlaceholderLength); err != nil {
				return err
			}
		}
		for index, option := range element.Options {
			if err := validateOption(option, fmt.Sprintf("%s.options[%d]", fieldPath, index)); err != nil {
				return err
			}
		}
		if element.InitialOption != nil {
			if err := validateOption(*element.InitialOption, fieldPath+".initial_option"); err != nil {
				return err
			}
		}
	case "button":
		if element.Text == nil {
			return fmt.Errorf("%s.text is required", fieldPath)
		}
		if err := validateTemplateTextLimit(doc.Name, element.Text, fieldPath+".text", maxRendererButtonTextLength); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%s has unsupported element type %q", fieldPath, element.Type)
	}
	return nil
}

func validateOption(option templateOption, fieldPath string) error {
	if option.Text == nil {
		return fmt.Errorf("%s.text is required", fieldPath)
	}
	if literal, ok := literalTemplateString(option.Value); ok && literal == "" {
		return fmt.Errorf("%s.value must not be empty", fieldPath)
	}
	if literal, ok := literalTemplateString(option.Value); ok && utf8.RuneCountInString(literal) > maxRendererOptionValueLength {
		return fmt.Errorf("%s.value exceeds %d character limit", fieldPath, maxRendererOptionValueLength)
	}
	if literal, ok := literalTemplateString(option.Text.Text); ok && utf8.RuneCountInString(literal) > maxRendererOptionTextLength {
		return fmt.Errorf("%s.text exceeds %d character limit", fieldPath, maxRendererOptionTextLength)
	}
	if option.Description != nil {
		if literal, ok := literalTemplateString(option.Description.Text); ok && utf8.RuneCountInString(literal) > maxRendererOptionTextLength {
			return fmt.Errorf("%s.description exceeds %d character limit", fieldPath, maxRendererOptionTextLength)
		}
	}
	return nil
}

func validateTemplateRepresentatives(doc templateDocument) error {
	if doc.Name == "builder_modal" {
		profiles := []BuilderProviderProfile{
			{Reference: "openai/fast", ProviderType: agentdef.ProviderTypeOpenAICompatible},
			{Reference: "opencode/default", ProviderType: agentdef.ProviderTypeACP},
		}
		for _, kind := range []domain.AgentKind{domain.AgentKindLLM, domain.AgentKindACP} {
			ctx := TemplateContext{
				Kind:     kind,
				Profiles: profiles,
				Values: map[string]string{
					"name": "incident_analyst", "description": "A bounded description", "instruction": "Inspect the incident and report findings.",
					"agent_type": string(kind), "model": "", "execution_mode": domain.ExecutionModeForeground,
					"timeout_seconds": "7200",
				},
			}
			if _, err := compileTemplateDocument(doc, ctx); err != nil {
				return err
			}
		}
		return nil
	}
	values := map[string]string{}
	switch doc.Name {
	case "confirmation_message":
		values = map[string]string{
			"summary": "A confirmation summary", "original_call_id": "call-1",
			"expires_at": "2030-01-01T00:00:00Z", "wrapper_call_id": "wrapper-1",
			"fallback_text": "Confirmation required: A confirmation summary",
		}
	case "agent_preview":
		values = map[string]string{
			"name": "incident_analyst", "agent_class": "LlmAgent", "provider_profile": "openai/fast",
			"execution_mode": domain.ExecutionModeForeground, "timeout": "no aplica",
			"sha256": "digest", "draft_id": "draft-1", "fallback_text": "Agent preview",
		}
		_, err := compileTemplateDocument(doc, TemplateContext{Values: values, PreviewYAMLParts: []string{"```yaml\nname: incident_analyst\n```"}})
		return err
	case "onboarding_message":
		values = map[string]string{
			"builder_context": `{"v":1,"actor_id":"U12345678","conversation_key":"slack:T12345678:dm:D12345678"}`,
			"intro":           "Welcome", "describe_prompt": "Describe a need",
		}
		_, err := compileTemplateDocument(doc, TemplateContext{Values: values, SuggestedPrompts: []string{"Ask one thing"}})
		return err
	}
	_, err := compileTemplateDocument(doc, TemplateContext{Values: values})
	return err
}

func allowedModalActionID(id string) bool {
	return allowedInteractiveActionIDs[id] || allowedBuilderBlockIDs[id]
}

var allowedInteractiveActionIDs = map[string]bool{
	"agent_type":                          true,
	"local_agent.builder.open":            true,
	"local_agent.builder.request_install": true,
	"local_agent.confirm.approve":         true,
	"local_agent.confirm.reject":          true,
	"local_agent.onboarding.describe":     true,
}

var allowedModalCallbackIDs = map[string]bool{
	"local_agent.builder.submit": true,
}

var allowedBuilderBlockIDs = map[string]bool{
	"name":            true,
	"agent_type":      true,
	"description":     true,
	"instruction":     true,
	"model":           true,
	"execution_mode":  true,
	"timeout_seconds": true,
}

var allowedMessageBlockIDs = map[string]bool{
	"confirmation_buttons":    true,
	"builder_preview_actions": true,
	"builder_launcher":        true,
	"onboarding_actions":      true,
}

var allowedMessageActionIDs = map[string]bool{
	"local_agent.builder.open":            true,
	"local_agent.builder.request_install": true,
	"local_agent.confirm.approve":         true,
	"local_agent.confirm.reject":          true,
	"local_agent.onboarding.describe":     true,
}

func validateLiteralID(value, fieldPath string) error {
	if value == "" {
		return fmt.Errorf("%s must not be empty", fieldPath)
	}
	if _, isToken, err := parseTemplateString(value); err != nil {
		return fmt.Errorf("%s may not contain a placeholder or script: %w", fieldPath, err)
	} else if isToken {
		return fmt.Errorf("%s may not contain a placeholder", fieldPath)
	}
	if utf8.RuneCountInString(value) > maxRendererIDLength {
		return fmt.Errorf("%s exceeds %d character limit", fieldPath, maxRendererIDLength)
	}
	for _, r := range value {
		if r < 0x21 || r > 0x7e {
			return fmt.Errorf("%s must contain printable ASCII only", fieldPath)
		}
	}
	return nil
}

func validateLiteralString(value, templateName, fieldPath string) error {
	return validateTemplateString(templateName, fieldPath, value, false, false)
}

func validateTemplateString(templateName, fieldPath, value string, allowScalar, allowOptions bool) error {
	token, isToken, err := parseTemplateString(value)
	if err != nil {
		return fmt.Errorf("%s: %w", fieldPath, err)
	}
	if !isToken {
		return nil
	}
	if token.Kind == tokenValue {
		if !allowScalar {
			return fmt.Errorf("%s does not allow scalar placeholders", fieldPath)
		}
		if _, ok := scalarKeysByTemplate[templateName][token.Key]; !ok {
			return fmt.Errorf("%s uses unknown scalar placeholder %q", fieldPath, token.Key)
		}
		return nil
	}
	if !allowOptions {
		return fmt.Errorf("%s does not allow collection placeholders", fieldPath)
	}
	if templateName != "builder_modal" || token.Key != "model" {
		return fmt.Errorf("%s uses unknown collection placeholder %q", fieldPath, token.Key)
	}
	return nil
}

func allowedCollectionBlockToken(templateName, key string) bool {
	switch templateName {
	case "agent_preview":
		return key == "preview_yaml_parts"
	case "onboarding_message":
		return key == "suggested_prompts"
	default:
		return false
	}
}

func parseTemplateString(value string) (templateToken, bool, error) {
	if strings.HasPrefix(value, "{{") && strings.HasSuffix(value, "}}") {
		body := strings.TrimSuffix(strings.TrimPrefix(value, "{{"), "}}")
		if strings.Count(value, "{{") != 1 || strings.Count(value, "}}") != 1 {
			return templateToken{}, false, errors.New("placeholder must be one exact token")
		}
		parts := strings.Split(body, ".")
		if len(parts) != 2 || parts[1] == "" || !validTokenKey(parts[1]) {
			return templateToken{}, false, errors.New("placeholder expressions are not supported")
		}
		switch parts[0] {
		case "value":
			return templateToken{Kind: tokenValue, Key: parts[1]}, true, nil
		case "options":
			return templateToken{Kind: tokenOptions, Key: parts[1]}, true, nil
		default:
			return templateToken{}, false, fmt.Errorf("unknown placeholder namespace %q", parts[0])
		}
	}
	lower := strings.ToLower(value)
	if strings.Contains(value, "{{") || strings.Contains(value, "}}") || strings.Contains(value, "${") || strings.Contains(value, "<%") || strings.Contains(lower, "javascript:") || strings.Contains(lower, "<script") || strings.Contains(lower, "function(") || strings.Contains(lower, "eval(") || strings.Contains(value, "=>") {
		return templateToken{}, false, errors.New("partial placeholders, expressions, and scripts are not supported")
	}
	return templateToken{}, false, nil
}

func validTokenKey(value string) bool {
	if value == "" {
		return false
	}
	for index, r := range value {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9' || index == 0) && r != '_' {
			return false
		}
	}
	return true
}

func literalTemplateString(value string) (string, bool) {
	_, ok, err := parseTemplateString(value)
	return value, !ok && err == nil
}

func objectKeys(data []byte) (map[string]struct{}, error) {
	var object map[string]json.RawMessage
	if err := decodeStrictJSON(data, &object); err != nil {
		return nil, err
	}
	if object == nil {
		return nil, errors.New("expected JSON object")
	}
	keys := make(map[string]struct{}, len(object))
	for key := range object {
		keys[key] = struct{}{}
	}
	return keys, nil
}

func containsObjectKey(keys map[string]struct{}, key string) bool {
	_, ok := keys[key]
	return ok
}

func rejectUnknownObjectKeys(keys map[string]struct{}, allowed map[string]struct{}) error {
	unknown := make([]string, 0)
	for key := range keys {
		if _, ok := allowed[key]; !ok {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return fmt.Errorf("unknown field %q", unknown[0])
}

func decodeStrictJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("expected one JSON document")
		}
		return err
	}
	return nil
}

func rejectDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := walkJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return errors.New("expected one JSON document")
		}
		return err
	}
	return nil
}

func walkJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, isDelim := token.(json.Delim)
	if !isDelim {
		return nil
	}
	switch delim {
	case '{':
		keys := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("JSON object key is not a string")
			}
			if _, exists := keys[key]; exists {
				return fmt.Errorf("duplicate JSON object key %q", key)
			}
			keys[key] = struct{}{}
			if err := walkJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim('}') {
			return errors.New("malformed JSON object")
		}
	case '[':
		for decoder.More() {
			if err := walkJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim(']') {
			return errors.New("malformed JSON array")
		}
	}
	return nil
}
