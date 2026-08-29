package slack

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/url"
	"path"
	"slices"
	"sort"
	"strings"

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
	"job_accepted_message",
	"onboarding_message",
}

var requiredTemplateSet = map[string]struct{}{
	"agent_preview":        {},
	"builder_modal":        {},
	"job_accepted_message": {},
	"onboarding_message":   {},
}

// TemplateCatalog is the validated, immutable set of declarative Slack
// templates. Its contents are intentionally not exposed as mutable JSON.
//
// Block Kit content itself is not re-described here: parsing and validating
// the shape of any block or element is delegated to slack-go's own dynamic
// JSON decoding (slackapi.Blocks / slackapi.ModalViewRequest), which already
// supports the entire Block Kit surface. This catalog only owns what is
// specific to this project: the {{value.x}}/{{options.x}} placeholder
// language, the $if condition, and the interactive-ID allowlists that decide
// which actions the bot will honor.
type TemplateCatalog struct {
	templates      map[string]templateDocument
	interactiveIDs TemplateInteractiveIDs
}

// TemplateInfo describes one catalog entry without exposing its layout.
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
	return slices.Sorted(maps.Keys(c.templates))
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

// InteractiveIDs returns the interactive-ID inventory computed once at
// catalog-load time from a representative compile of every template.
func (c *TemplateCatalog) InteractiveIDs() TemplateInteractiveIDs {
	if c == nil {
		return TemplateInteractiveIDs{}
	}
	return c.interactiveIDs
}

// ValidateDispatcher verifies catalog coverage and action IDs from additional
// view engines before Socket Mode starts. It rejects registered IDs outside
// the allowlists.
func (c *TemplateCatalog) ValidateDispatcher(dispatcher *InteractiveDispatcher, additionalActionIDs ...string) error {
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
	validateActionID := func(actionID, source string) error {
		if !allowedInteractiveActionIDs[actionID] && !allowedMessageActionIDs[actionID] {
			return fmt.Errorf("%s action ID %q is not in the allowlist", source, actionID)
		}
		if !dispatcher.HasAction(actionID) {
			return fmt.Errorf("%s action ID %q has no registered block-action handler", source, actionID)
		}
		return nil
	}
	for _, actionID := range ids.Actions {
		if err := validateActionID(actionID, "template"); err != nil {
			return err
		}
	}
	for _, actionID := range additionalActionIDs {
		if err := validateActionID(actionID, "engine"); err != nil {
			return err
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
		if !allowedInteractiveActionIDs[actionID] && !allowedMessageActionIDs[actionID] {
			return fmt.Errorf("registered action ID %q is not in the allowlist", actionID)
		}
	}
	return nil
}

func appendTemplateID(ids *[]string, value string) {
	if value != "" && !slices.Contains(*ids, value) {
		*ids = append(*ids, value)
	}
}

var embeddedTemplateCatalog, embeddedTemplateCatalogErr = loadTemplateCatalog(embeddedTemplates)

// templateDocument is the parsed envelope around a template. The "blocks"
// payload is kept as raw JSON: its shape is not modeled here at all. It is
// substituted (see substituteTemplateTokens) and handed directly to
// slackapi.Blocks / slackapi.ModalViewRequest for decoding at compile time,
// so any Block Kit type slack-go supports works without a Go change.
type templateDocument struct {
	Name          string
	SchemaVersion int
	Surface       string
	Modal         *templateModalPayload
	Message       *templateMessagePayload
}

type templateModalPayload struct {
	Title           json.RawMessage
	Submit          json.RawMessage
	Close           json.RawMessage
	CallbackID      string
	PrivateMetadata string
	Blocks          json.RawMessage
}

type templateMessagePayload struct {
	FallbackText string
	Blocks       json.RawMessage
}

type rawTemplateDocument struct {
	Name          string          `json:"name"`
	SchemaVersion int             `json:"schema_version"`
	Surface       string          `json:"surface"`
	Payload       json.RawMessage `json:"payload"`
}

type rawModalPayload struct {
	Type            string          `json:"type"`
	Title           json.RawMessage `json:"title"`
	Submit          json.RawMessage `json:"submit"`
	Close           json.RawMessage `json:"close"`
	CallbackID      string          `json:"callback_id"`
	PrivateMetadata string          `json:"private_metadata"`
	Blocks          json.RawMessage `json:"blocks"`
}

type rawMessagePayload struct {
	FallbackText string          `json:"fallback_text"`
	Blocks       json.RawMessage `json:"blocks"`
}

// templateTokenKind distinguishes the two placeholder namespaces this
// project supports: {{value.x}} (a scalar substituted from TemplateContext)
// and {{options.x}} (a call-site-specific collection: provider model
// options, or a dynamic run of message blocks).
type templateTokenKind uint8

const (
	tokenValue templateTokenKind = iota + 1
	tokenOptions
)

type templateToken struct {
	Kind templateTokenKind
	Key  string
}

// scalarKeysByTemplate is the per-template allowlist of legitimate
// {{value.x}} keys. It exists to catch a typo'd or cross-template-confused
// placeholder key; it is not a Block Kit shape check.
var scalarKeysByTemplate = map[string]map[string]struct{}{
	"builder_modal": {
		"name": {}, "description": {}, "instruction": {}, "agent_type": {},
		"model": {}, "execution_mode": {}, "timeout_seconds": {},
	},
	"job_accepted_message": {
		"subtitle": {}, "created_at": {}, "updated_at": {},
		"status_sentence": {}, "fallback_text": {},
	},
	"agent_preview": {
		"name": {}, "agent_class": {}, "sha256": {}, "draft_id": {}, "fallback_text": {},
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
	ids := TemplateInteractiveIDs{}
	for _, name := range requiredTemplateNames {
		representativeIDs, err := validateTemplateRepresentative(catalog.templates[name])
		if err != nil {
			return nil, fmt.Errorf("compile template %q representative: %w", name, err)
		}
		mergeInteractiveIDs(&ids, representativeIDs)
	}
	sort.Strings(ids.ModalCallbacks)
	sort.Strings(ids.Actions)
	sort.Strings(ids.BuilderBlocks)
	sort.Strings(ids.MessageBlocks)
	catalog.interactiveIDs = ids
	return catalog, nil
}

func mergeInteractiveIDs(dst *TemplateInteractiveIDs, src TemplateInteractiveIDs) {
	for _, id := range src.ModalCallbacks {
		appendTemplateID(&dst.ModalCallbacks, id)
	}
	for _, id := range src.Actions {
		appendTemplateID(&dst.Actions, id)
	}
	for _, id := range src.BuilderBlocks {
		appendTemplateID(&dst.BuilderBlocks, id)
	}
	for _, id := range src.MessageBlocks {
		appendTemplateID(&dst.MessageBlocks, id)
	}
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
		var payload rawModalPayload
		if err := decodeStrictJSON(raw.Payload, &payload); err != nil {
			return templateDocument{}, fmt.Errorf("modal payload: %w", err)
		}
		if payload.Type != "modal" {
			return templateDocument{}, fmt.Errorf("modal payload type must be %q", "modal")
		}
		if len(payload.Title) == 0 {
			return templateDocument{}, errors.New("modal title is required")
		}
		if err := validateLiteralID(payload.CallbackID, "payload.callback_id"); err != nil {
			return templateDocument{}, err
		}
		if !allowedModalCallbackIDs[payload.CallbackID] {
			return templateDocument{}, fmt.Errorf("unregistered modal callback ID %q", payload.CallbackID)
		}
		if err := rejectPlaceholder(payload.PrivateMetadata, "payload.private_metadata"); err != nil {
			return templateDocument{}, err
		}
		if len(payload.Blocks) == 0 {
			return templateDocument{}, errors.New("modal blocks are required")
		}
		doc.Modal = &templateModalPayload{
			Title: payload.Title, Submit: payload.Submit, Close: payload.Close,
			CallbackID: payload.CallbackID, PrivateMetadata: payload.PrivateMetadata, Blocks: payload.Blocks,
		}
	case "message":
		var payload rawMessagePayload
		if err := decodeStrictJSON(raw.Payload, &payload); err != nil {
			return templateDocument{}, fmt.Errorf("message payload: %w", err)
		}
		if strings.TrimSpace(payload.FallbackText) == "" {
			return templateDocument{}, errors.New("message fallback_text is required")
		}
		if err := validateTemplateString(raw.Name, "payload.fallback_text", payload.FallbackText, true, false); err != nil {
			return templateDocument{}, err
		}
		if len(payload.Blocks) == 0 {
			return templateDocument{}, errors.New("message blocks are required")
		}
		doc.Message = &templateMessagePayload{FallbackText: payload.FallbackText, Blocks: payload.Blocks}
	}
	return doc, nil
}

func validateTemplateDocument(doc templateDocument) error {
	if _, ok := requiredTemplateSet[doc.Name]; !ok {
		return fmt.Errorf("unknown template name %q", doc.Name)
	}
	wantSurface := "message"
	if doc.Name == "builder_modal" {
		wantSurface = "modal"
	}
	if doc.Surface != wantSurface {
		return fmt.Errorf("template %q must use %s surface", doc.Name, wantSurface)
	}
	if doc.Modal == nil && doc.Message == nil {
		return errors.New("template payload is missing")
	}
	return nil
}

// validateTemplateRepresentative smoke-compiles a template with synthetic
// values at catalog-load time, so a broken template fails at process start
// rather than at first real use, and returns the interactive IDs it declares.
func validateTemplateRepresentative(doc templateDocument) (TemplateInteractiveIDs, error) {
	if doc.Name == "builder_modal" {
		profiles := []BuilderProviderProfile{
			{Reference: "openai/fast", ProviderType: "openai_compatible"},
			{Reference: "codex/default", ProviderType: "agent_cli"},
		}
		ids := TemplateInteractiveIDs{}
		for _, kind := range []domain.AgentKind{domain.AgentKindLLM, domain.AgentKindAgentCLI} {
			ctx := TemplateContext{
				Kind:     kind,
				Profiles: profiles,
				Values: map[string]string{
					"name": "incident_analyst", "description": "A bounded description", "instruction": "Inspect the incident and report findings.",
					"agent_type": string(kind), "model": "", "execution_mode": "foreground",
					"timeout_seconds": "7200",
				},
			}
			view, err := compileModalTemplate(doc, ctx)
			if err != nil {
				return TemplateInteractiveIDs{}, err
			}
			representativeIDs, err := collectInteractiveIDs(doc.Name, view.CallbackID, view.Blocks.BlockSet, true)
			if err != nil {
				return TemplateInteractiveIDs{}, err
			}
			mergeInteractiveIDs(&ids, representativeIDs)
		}
		return ids, nil
	}
	context := TemplateContext{Values: map[string]string{}}
	switch doc.Name {
	case "job_accepted_message":
		context.Values = map[string]string{
			"subtitle": "*Job ID:* `job-1` · *Status:* `queued`", "created_at": "*Created:*\n2030-01-01T00:00:00Z",
			"updated_at": "*Updated:*\n2030-01-01T00:00:00Z", "status_sentence": "The host accepted the job.",
			"fallback_text": "Job accepted / running: job-1 (queued)",
		}
	case "agent_preview":
		context.Values = map[string]string{
			"name": "incident_analyst", "agent_class": "LlmAgent",
			"sha256": "digest", "draft_id": "draft-1", "fallback_text": "Agent preview",
		}
		context.PreviewYAMLParts = []string{"```yaml\nname: incident_analyst\n```"}
	case "onboarding_message":
		context.Values = map[string]string{
			"builder_context": `{"v":1,"actor_id":"U12345678","conversation_key":"slack:T12345678:dm:D12345678"}`,
			"intro":           "Welcome", "describe_prompt": "Describe a need",
		}
		context.SuggestedPrompts = []string{"Ask one thing"}
	}
	_, blocks, err := compileMessageTemplate(doc, context)
	if err != nil {
		return TemplateInteractiveIDs{}, err
	}
	return collectInteractiveIDs(doc.Name, "", blocks, false)
}

var allowedModalCallbackIDs = map[string]bool{
	"local_agent.builder.submit": true,
}

var allowedInteractiveActionIDs = map[string]bool{
	"agent_type":                          true,
	"local_agent.builder.open":            true,
	"local_agent.builder.request_install": true,
	"local_agent.confirm.approve":         true,
	"local_agent.confirm.reject":          true,
	statusActionID:                        true,
	"local_agent.onboarding.describe":     true,
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
	"builder_preview_actions": true,
	"builder_launcher":        true,
	"onboarding_actions":      true,
}

var allowedMessageActionIDs = map[string]bool{
	"local_agent.builder.open":            true,
	"local_agent.builder.request_install": true,
	"local_agent.confirm.approve":         true,
	"local_agent.confirm.reject":          true,
	statusActionID:                        true,
	"local_agent.onboarding.describe":     true,
}

// validateLiteralID checks the small set of strings that must always be a
// fixed, developer-chosen identifier and must never be substituted from a
// placeholder: action_id, block_id, callback_id.
func validateLiteralID(value, fieldPath string) error {
	if value == "" {
		return fmt.Errorf("%s must not be empty", fieldPath)
	}
	if err := rejectPlaceholder(value, fieldPath); err != nil {
		return err
	}
	if len([]rune(value)) > maxRendererIDLength {
		return fmt.Errorf("%s exceeds %d character limit", fieldPath, maxRendererIDLength)
	}
	for _, r := range value {
		if r < 0x21 || r > 0x7e {
			return fmt.Errorf("%s must contain printable ASCII only", fieldPath)
		}
	}
	return nil
}

func rejectPlaceholder(value, fieldPath string) error {
	if _, isToken, err := parseTemplateString(value); err != nil {
		return fmt.Errorf("%s may not contain a placeholder or script: %w", fieldPath, err)
	} else if isToken {
		return fmt.Errorf("%s may not contain a placeholder", fieldPath)
	}
	return nil
}

func validateSlackButtonURL(value, fieldPath string) error {
	if value == "" {
		return nil
	}
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil {
		return fmt.Errorf("%s must be an absolute http or https URL", fieldPath)
	}
	if strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("%s must not contain line breaks", fieldPath)
	}
	return nil
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
	return nil
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
	if strings.Contains(value, "{{") || strings.Contains(value, "}}") || strings.Contains(value, "${") || strings.Contains(value, "<%") || strings.Contains(lower, "javascript:") ||
		strings.Contains(lower, "<script") ||
		strings.Contains(lower, "function(") ||
		strings.Contains(lower, "eval(") ||
		strings.Contains(value, "=>") {
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
