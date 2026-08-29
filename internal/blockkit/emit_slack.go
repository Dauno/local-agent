package blockkit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	slackapi "github.com/slack-go/slack"
)

type compiledTemplate struct {
	blocks       []slackapi.Block
	layoutJSON   []byte
	layoutSHA256 string
	fallback     string
	inputSlots   map[string]string
}

// Message renders a message-surface template.
func (e *Engine) Message(view View) (Message, error) {
	if e == nil {
		return Message{}, errors.New("engine is nil")
	}
	name, err := templateNameForView(view)
	if err != nil {
		return Message{}, err
	}
	doc, ok := e.templates[name]
	if !ok {
		return Message{}, fmt.Errorf("view template %q is not registered", name)
	}
	if doc.Surface != "message" {
		return Message{}, fmt.Errorf("template %q is not a message template", name)
	}
	binding, ok := e.bindings[name]
	if !ok {
		return Message{}, fmt.Errorf("view template %q has no registered view type", name)
	}
	values, err := viewValues(view, binding, doc)
	if err != nil {
		return Message{}, fmt.Errorf("template %q: %w", name, err)
	}
	compiled, err := renderCompiled(doc, values)
	if err != nil {
		return Message{}, fmt.Errorf("template %q: %w", name, err)
	}
	if err := verifyCompiled(doc, compiled.blocks); err != nil {
		return Message{}, fmt.Errorf("template %q: %w", name, err)
	}
	return Message{
		Blocks: compiled.blocks, FallbackText: compiled.fallback,
		LayoutSHA256: doc.LayoutSHA256, inputSlots: compiled.inputSlots,
	}, nil
}

// Modal renders a modal-surface template.
func (e *Engine) Modal(view View) (slackapi.ModalViewRequest, error) {
	if e == nil {
		return slackapi.ModalViewRequest{}, errors.New("engine is nil")
	}
	name, err := templateNameForView(view)
	if err != nil {
		return slackapi.ModalViewRequest{}, err
	}
	doc, ok := e.templates[name]
	if !ok {
		return slackapi.ModalViewRequest{}, fmt.Errorf("view template %q is not registered", name)
	}
	if doc.Surface != "modal" {
		return slackapi.ModalViewRequest{}, fmt.Errorf("template %q is not a modal template", name)
	}
	binding, ok := e.bindings[name]
	if !ok {
		return slackapi.ModalViewRequest{}, fmt.Errorf("view template %q has no registered view type", name)
	}
	values, err := viewValues(view, binding, doc)
	if err != nil {
		return slackapi.ModalViewRequest{}, fmt.Errorf("template %q: %w", name, err)
	}
	compiled, err := renderCompiled(doc, values)
	if err != nil {
		return slackapi.ModalViewRequest{}, fmt.Errorf("template %q: %w", name, err)
	}
	if err := verifyCompiled(doc, compiled.blocks); err != nil {
		return slackapi.ModalViewRequest{}, fmt.Errorf("template %q: %w", name, err)
	}
	modal := slackapi.ModalViewRequest{
		Type:       slackapi.VTModal,
		CallbackID: doc.CallbackID,
		Blocks:     slackapi.Blocks{BlockSet: compiled.blocks},
	}
	modal.Title, err = renderTextObject(doc.Title, doc, values, "title")
	if err != nil {
		return slackapi.ModalViewRequest{}, fmt.Errorf("template %q: %w", name, err)
	}
	if doc.Submit != nil {
		modal.Submit, err = renderTextObject(doc.Submit, doc, values, "submit")
		if err != nil {
			return slackapi.ModalViewRequest{}, fmt.Errorf("template %q: %w", name, err)
		}
	}
	if doc.Close != nil {
		modal.Close, err = renderTextObject(doc.Close, doc, values, "close")
		if err != nil {
			return slackapi.ModalViewRequest{}, fmt.Errorf("template %q: %w", name, err)
		}
	}
	return modal, nil
}

func renderCompiled(doc templateDocument, values renderValues) (compiledTemplate, error) {
	rendered, err := renderNode(doc.Layout, doc, values, renderContext{})
	if err != nil {
		return compiledTemplate{}, fmt.Errorf("render layout: %w", err)
	}
	layout, ok := rendered.([]any)
	if !ok {
		return compiledTemplate{}, errors.New("rendered layout is not an array")
	}
	layoutJSON, err := json.Marshal(layout)
	if err != nil {
		return compiledTemplate{}, fmt.Errorf("marshal compiled layout: %w", err)
	}
	var blocks slackapi.Blocks
	if err := json.Unmarshal(layoutJSON, &blocks); err != nil {
		return compiledTemplate{}, fmt.Errorf("decode compiled layout: %w", err)
	}
	fallback, err := renderFallback(doc, values)
	if err != nil {
		return compiledTemplate{}, err
	}
	digest := sha256.Sum256(layoutJSON)
	inputSlots := make(map[string]string)
	collectInputSlots(doc.Layout, doc, values, renderContext{}, inputSlots)
	return compiledTemplate{
		blocks: blocks.BlockSet, layoutJSON: layoutJSON,
		layoutSHA256: hex.EncodeToString(digest[:]), fallback: fallback,
		inputSlots: inputSlots,
	}, nil
}

func validateRepresentativeMetadata(doc templateDocument, values renderValues) error {
	if doc.Surface != "modal" {
		return nil
	}
	title, err := renderTextObject(doc.Title, doc, values, "title")
	if err != nil {
		return err
	}
	if err := validateTextLength(*title, 24, "title"); err != nil {
		return err
	}
	for field, value := range map[string]any{"submit": doc.Submit, "close": doc.Close} {
		if value == nil {
			continue
		}
		text, err := renderTextObject(value, doc, values, field)
		if err != nil {
			return err
		}
		if err := validateTextLength(*text, 24, field); err != nil {
			return err
		}
	}
	return nil
}

func renderFallback(doc templateDocument, values renderValues) (string, error) {
	if doc.Fallback != nil {
		fallback, err := renderString(*doc.Fallback, values, renderContext{}, "plain_text")
		if err != nil {
			return "", fmt.Errorf("render fallback: %w", err)
		}
		return truncateCodePoints(fallback, 3000), nil
	}
	resolved, err := renderNodeWith(doc.Layout, doc, values, renderContext{}, renderFallbackStringForSlot)
	if err != nil {
		return "", fmt.Errorf("render fallback: %w", err)
	}
	layout, ok := resolved.([]any)
	if !ok {
		return "", errors.New("rendered fallback layout is not an array")
	}
	var texts []string
	collectFallbackText(layout, &texts)
	return truncateCodePoints(strings.Join(texts, "\n"), 3000), nil
}

func renderFallbackStringForSlot(value string, values renderValues, context renderContext, _ string) (string, error) {
	return renderFallbackString(value, values, context)
}

func collectFallbackText(node any, texts *[]string) {
	switch typed := node.(type) {
	case []any:
		for _, child := range typed {
			collectFallbackText(child, texts)
		}
	case map[string]any:
		if typeName, ok := typed["type"].(string); ok && (typeName == "plain_text" || typeName == "mrkdwn") {
			if text, ok := typed["text"].(string); ok {
				*texts = append(*texts, text)
			}
			return
		}
		for _, key := range orderedObjectKeys(typed) {
			collectFallbackText(typed[key], texts)
		}
	}
}

func renderFallbackString(value string, values renderValues, context renderContext) (string, error) {
	placeholders, err := parsePlaceholders(value)
	if err != nil {
		return "", err
	}
	if len(placeholders) == 0 {
		return cleanFallbackLiteral(value), nil
	}
	var result strings.Builder
	position := 0
	for _, token := range placeholders {
		open := strings.Index(value[position:], "{{") + position
		close := strings.Index(value[open+2:], "}}") + open + 2
		result.WriteString(cleanFallbackLiteral(value[position:open]))
		replacement, err := renderFallbackPlaceholder(token, values, context)
		if err != nil {
			return "", err
		}
		result.WriteString(replacement)
		position = close + 2
	}
	result.WriteString(cleanFallbackLiteral(value[position:]))
	return result.String(), nil
}

func renderFallbackPlaceholder(token placeholder, values renderValues, context renderContext) (string, error) {
	if token.modifier == "code" || token.modifier == "bold" {
		token.modifier = ""
	}
	return renderPlaceholder(token, values, context, "plain_text")
}

func cleanFallbackLiteral(value string) string {
	return neutralizeUnsafeControls(stripSlackMarkup(value))
}

func orderedObjectKeys(object map[string]any) []string {
	priority := map[string]int{
		"title": 1, "subtitle": 2, "text": 3, "fields": 4,
		"elements": 5, "accessory": 6, "child_blocks": 7,
	}
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.SliceStable(keys, func(left, right int) bool {
		leftPriority, leftKnown := priority[keys[left]]
		rightPriority, rightKnown := priority[keys[right]]
		if leftKnown && rightKnown && leftPriority != rightPriority {
			return leftPriority < rightPriority
		}
		if leftKnown != rightKnown {
			return leftKnown
		}
		return keys[left] < keys[right]
	})
	return keys
}

var (
	fallbackLinkPattern    = regexp.MustCompile(`<[^>|]+\|([^>]+)>`)
	fallbackTagPattern     = regexp.MustCompile(`<[^>]+>`)
	fallbackHeadingPattern = regexp.MustCompile(`(?m)^[ \t]{0,3}#{1,6}[ \t]+`)
	fallbackBulletPattern  = regexp.MustCompile(`(?m)^[ \t]{0,3}[-+•][ \t]+`)
)

func stripSlackMarkup(value string) string {
	value = fallbackLinkPattern.ReplaceAllString(value, "$1")
	value = fallbackTagPattern.ReplaceAllString(value, "")
	value = fallbackHeadingPattern.ReplaceAllString(value, "")
	value = fallbackBulletPattern.ReplaceAllString(value, "")
	value = strings.ReplaceAll(value, "```", "")
	value = strings.ReplaceAll(value, "`", "")
	value = strings.ReplaceAll(value, "*", "")
	value = strings.ReplaceAll(value, "_", "")
	value = strings.ReplaceAll(value, "~", "")
	return value
}

func truncateCodePoints(value string, limit int) string {
	if limit <= 0 || utf8.RuneCountInString(value) <= limit {
		return value
	}
	return string([]rune(value)[:limit])
}

func renderTextObject(value any, doc templateDocument, values renderValues, field string) (*slackapi.TextBlockObject, error) {
	var raw any
	switch typed := value.(type) {
	case string:
		raw = map[string]any{"type": "plain_text", "text": typed}
	case map[string]any:
		raw = typed
	default:
		return nil, fmt.Errorf("%s must be a text object or string", field)
	}
	rendered, err := renderNode(raw, doc, values, renderContext{})
	if err != nil {
		return nil, fmt.Errorf("render %s: %w", field, err)
	}
	data, err := json.Marshal(rendered)
	if err != nil {
		return nil, fmt.Errorf("marshal %s: %w", field, err)
	}
	var text slackapi.TextBlockObject
	if err := json.Unmarshal(data, &text); err != nil {
		return nil, fmt.Errorf("decode %s: %w", field, err)
	}
	if text.Type != "plain_text" {
		return nil, fmt.Errorf("%s must use plain_text", field)
	}
	if err := text.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", field, err)
	}
	return &text, nil
}

func validateTextLength(text slackapi.TextBlockObject, limit int, field string) error {
	if utf8.RuneCountInString(text.Text) > limit {
		return fmt.Errorf("%s exceeds %d code points", field, limit)
	}
	return nil
}
