package blockkit

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"
)

type placeholder struct {
	name     string
	modifier string
	itemPart string
}

type renderContext struct {
	item      any
	itemType  InputType
	itemInput string
	hasItem   bool
}

func validateLayoutContract(doc templateDocument) error {
	used := make(map[string]struct{}, len(doc.Inputs))
	if err := inspectNode(doc.Layout, doc, renderContext{}, used, "layout"); err != nil {
		return err
	}
	for name := range doc.Inputs {
		if _, ok := used[name]; !ok {
			return fmt.Errorf("input %q is declared but unused in layout", name)
		}
	}
	return nil
}

func inspectNode(node any, doc templateDocument, context renderContext, used map[string]struct{}, field string) error {
	switch typed := node.(type) {
	case nil, bool, float64:
		return nil
	case string:
		return inspectString(typed, doc, context, used, field, "plain_text")
	case []any:
		for index, child := range typed {
			if err := inspectNode(child, doc, context, used, fmt.Sprintf("%s[%d]", field, index)); err != nil {
				return err
			}
		}
	case map[string]any:
		markInputStateActionID(typed, doc, used)
		if region, ok := typed["region"]; ok {
			return inspectRegion(typed, region, doc, context, used, field)
		}
		if actions, ok := typed["actions"]; ok {
			for key := range typed {
				if key != "actions" {
					return fmt.Errorf("%s has unknown actions key %q", field, key)
				}
			}
			return inspectActions(actions, doc, used, field)
		}
		slot := textSlot(typed)
		for key, child := range typed {
			childField := field + "." + key
			if isFixedTemplateKey(key) {
				if text, ok := child.(string); ok && hasPlaceholderMarker(text) {
					return fmt.Errorf("%s may not contain a placeholder", childField)
				}
				if err := inspectStringValue(child, doc, context, used, childField, slot); err != nil {
					return err
				}
				continue
			}
			if text, ok := child.(string); ok {
				if err := inspectString(text, doc, context, used, childField, slot); err != nil {
					return err
				}
				continue
			}
			if err := inspectNode(child, doc, context, used, childField); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("%s contains unsupported JSON value %T", field, node)
	}
	return nil
}

func markInputStateActionID(node map[string]any, doc templateDocument, used map[string]struct{}) {
	if node["type"] != "input" {
		return
	}
	element, ok := node["element"].(map[string]any)
	if !ok {
		return
	}
	actionID, ok := element["action_id"].(string)
	if ok {
		if _, exists := doc.Inputs[actionID]; exists {
			used[actionID] = struct{}{}
		}
	}
}

func inspectStringValue(value any, doc templateDocument, context renderContext, used map[string]struct{}, field, slot string) error {
	text, ok := value.(string)
	if !ok {
		return inspectNode(value, doc, context, used, field)
	}
	return inspectString(text, doc, context, used, field, slot)
}

func inspectRegion(node map[string]any, regionValue any, doc templateDocument, context renderContext, used map[string]struct{}, field string) error {
	regionName, ok := regionValue.(string)
	if !ok || !validName(regionName) {
		return fmt.Errorf("%s.region must name an input", field)
	}
	input, exists := doc.Inputs[regionName]
	if !exists {
		return fmt.Errorf("%s.region uses unknown input %q", field, regionName)
	}
	used[regionName] = struct{}{}
	whenValue, hasWhen := node["when"]
	if hasWhen {
		whenName, ok := whenValue.(string)
		if !ok || !validName(whenName) {
			return fmt.Errorf("%s.when must name an input", field)
		}
		_, exists := doc.Inputs[whenName]
		if !exists {
			return fmt.Errorf("%s.when uses unknown input %q", field, whenName)
		}
		used[whenName] = struct{}{}
	}
	_, hasEach := node["each"]
	_, hasBlock := node["block"]
	if hasEach == hasBlock {
		return fmt.Errorf("%s must contain exactly one of each or block", field)
	}
	for key := range node {
		if key != "region" && key != "when" && key != "each" && key != "block" {
			return fmt.Errorf("%s has unknown region key %q", field, key)
		}
	}
	if hasEach {
		if input.Type != InputTypeLongText && input.Type != InputTypeListPair {
			return fmt.Errorf("%s.each requires longtext or list<pair>, got %q", field, input.Type)
		}
		child, ok := node["each"].(map[string]any)
		if !ok {
			return fmt.Errorf("%s.each must contain an object", field)
		}
		return inspectNode(child, doc, renderContext{
			itemType: input.Type, itemInput: regionName, hasItem: true,
		}, used, field+".each")
	}
	child, ok := node["block"].(map[string]any)
	if !ok {
		return fmt.Errorf("%s.block must contain an object", field)
	}
	return inspectNode(child, doc, context, used, field+".block")
}

func inspectActions(value any, doc templateDocument, used map[string]struct{}, field string) error {
	items, ok := value.([]any)
	if !ok || len(items) == 0 {
		return fmt.Errorf("%s.actions must be a non-empty array", field)
	}
	for index, item := range items {
		name, ok := item.(string)
		if !ok {
			return fmt.Errorf("%s.actions[%d] must be an action name", field, index)
		}
		action, exists := doc.Actions[name]
		if !exists {
			return fmt.Errorf("%s.actions[%d] uses unknown action %q", field, index, name)
		}
		if action.Carries != "" {
			used[action.Carries] = struct{}{}
		}
	}
	return nil
}

func inspectString(value string, doc templateDocument, context renderContext, used map[string]struct{}, field, slot string) error {
	placeholders, err := parsePlaceholders(value)
	if err != nil {
		return fmt.Errorf("%s: %w", field, err)
	}
	for _, token := range placeholders {
		if err := validatePlaceholderUse(token, doc, context, slot, used, field); err != nil {
			return err
		}
	}
	return nil
}

func validateTemplateString(value any, field string, doc templateDocument, context renderContext) error {
	text, ok := value.(string)
	if !ok {
		return inspectNode(value, doc, context, make(map[string]struct{}), field)
	}
	return inspectString(text, doc, context, make(map[string]struct{}), field, "plain_text")
}

func validatePlaceholderUse(token placeholder, doc templateDocument, context renderContext, slot string, used map[string]struct{}, field string) error {
	inputType := tokenInputType(token, doc, context)
	if token.itemPart != "" {
		if !context.hasItem {
			return fmt.Errorf("%s uses item outside each region", field)
		}
		if context.itemType != InputTypeListPair && token.itemPart != "" {
			return fmt.Errorf("%s uses item.%s for a non-pair region", field, token.itemPart)
		}
		if token.name != "item" {
			return fmt.Errorf("%s has invalid item placeholder", field)
		}
	} else if token.name == "item" {
		if !context.hasItem {
			return fmt.Errorf("%s uses item outside each region", field)
		}
		if context.itemType == InputTypeListPair {
			return fmt.Errorf("%s cannot render list<pair> as {{item}}", field)
		}
	} else {
		if _, ok := doc.Inputs[token.name]; !ok {
			return fmt.Errorf("%s uses unknown input %q", field, token.name)
		}
		used[token.name] = struct{}{}
	}
	if inputType == InputTypeBool || inputType == InputTypeListPair {
		return fmt.Errorf("%s cannot render %q input in a text slot", field, inputType)
	}
	if inputType == InputTypeLongText {
		if input, ok := doc.Inputs[token.name]; ok && input.Chunk > 0 && !context.hasItem {
			return fmt.Errorf("%s must use chunked longtext %q through each", field, token.name)
		}
	}
	if token.modifier == "code" || token.modifier == "bold" {
		if slot != "mrkdwn" {
			return fmt.Errorf("%s modifier :%s requires an mrkdwn slot", field, token.modifier)
		}
	}
	if token.modifier == "hhmm" || token.modifier == "rfc3339" {
		if inputType != InputTypeTimestamp {
			return fmt.Errorf("%s modifier :%s requires a timestamp input", field, token.modifier)
		}
	}
	if token.modifier == "upper" || token.modifier == "lower" {
		if inputType == InputTypeBool || inputType == InputTypeListPair {
			return fmt.Errorf("%s modifier :%s cannot format %q input", field, token.modifier, inputType)
		}
	}
	return nil
}

func tokenInputType(token placeholder, doc templateDocument, context renderContext) InputType {
	if token.name == "item" {
		if token.itemPart != "" {
			return InputTypeText
		}
		return context.itemType
	}
	return doc.Inputs[token.name].Type
}

func parsePlaceholders(value string) ([]placeholder, error) {
	if hasBlockedTemplateContent(value) {
		return nil, errors.New("contains a blocked expression or script")
	}
	if strings.Count(value, "{{") != strings.Count(value, "}}") {
		return nil, errors.New("unbalanced placeholder braces")
	}
	var result []placeholder
	for start := 0; start < len(value); {
		open := strings.Index(value[start:], "{{")
		closeBeforeOpen := strings.Index(value[start:], "}}")
		if open < 0 {
			if closeBeforeOpen >= 0 {
				return nil, errors.New("unbalanced placeholder braces")
			}
			break
		}
		if closeBeforeOpen >= 0 && closeBeforeOpen < open {
			return nil, errors.New("unbalanced placeholder braces")
		}
		open += start
		close := strings.Index(value[open+2:], "}}")
		if close < 0 {
			return nil, errors.New("unbalanced placeholder braces")
		}
		close += open + 2
		if strings.Contains(value[open+2:close], "{{") || strings.Contains(value[open+2:close], "}}") {
			return nil, errors.New("nested placeholder braces are not valid")
		}
		token, err := parsePlaceholderBody(value[open+2 : close])
		if err != nil {
			return nil, err
		}
		result = append(result, token)
		start = close + 2
	}
	return result, nil
}

func parsePlaceholderBody(body string) (placeholder, error) {
	parts := strings.Split(body, ":")
	if len(parts) > 2 || parts[0] == "" {
		return placeholder{}, fmt.Errorf("invalid placeholder %q", body)
	}
	token := placeholder{name: parts[0]}
	if len(parts) == 2 {
		token.modifier = parts[1]
		switch token.modifier {
		case "code", "bold", "hhmm", "rfc3339", "upper", "lower":
		default:
			return placeholder{}, fmt.Errorf("unknown placeholder modifier %q", token.modifier)
		}
	}
	if token.name == "item" {
		return token, nil
	}
	if strings.HasPrefix(token.name, "item.") {
		part := strings.TrimPrefix(token.name, "item.")
		if part != "label" && part != "value" {
			return placeholder{}, fmt.Errorf("unknown item field %q", part)
		}
		return placeholder{name: "item", itemPart: part, modifier: token.modifier}, nil
	}
	if !validName(token.name) {
		return placeholder{}, fmt.Errorf("invalid placeholder name %q", token.name)
	}
	return token, nil
}

func hasPlaceholderMarker(value string) bool {
	return strings.Contains(value, "{{") || strings.Contains(value, "}}")
}

func hasBlockedTemplateContent(value string) bool {
	lower := strings.ToLower(value)
	return strings.Contains(value, "${") || strings.Contains(value, "<%") ||
		strings.Contains(lower, "javascript:") || strings.Contains(lower, "<script") ||
		strings.Contains(lower, "function(") || strings.Contains(lower, "eval(") ||
		strings.Contains(value, "=>")
}

func isFixedTemplateKey(key string) bool {
	switch key {
	case "type", "block_id", "action_id", "callback_id":
		return true
	default:
		return false
	}
}

func textSlot(object map[string]any) string {
	typeName, _ := object["type"].(string)
	if typeName == "plain_text" || typeName == "mrkdwn" {
		return typeName
	}
	return "plain_text"
}

func renderString(value string, values renderValues, context renderContext, slot string) (string, error) {
	placeholders, err := parsePlaceholders(value)
	if err != nil {
		return "", err
	}
	if len(placeholders) == 0 {
		return value, nil
	}
	var result strings.Builder
	position := 0
	for _, token := range placeholders {
		open := strings.Index(value[position:], "{{") + position
		close := strings.Index(value[open+2:], "}}") + open + 2
		result.WriteString(value[position:open])
		replacement, err := renderPlaceholder(token, values, context, slot)
		if err != nil {
			return "", err
		}
		result.WriteString(replacement)
		position = close + 2
	}
	result.WriteString(value[position:])
	return result.String(), nil
}

func renderPlaceholder(token placeholder, values renderValues, context renderContext, slot string) (string, error) {
	if token.name == "item" {
		if !context.hasItem {
			return "", errors.New("item placeholder is outside an each region")
		}
		if token.itemPart != "" {
			pair, ok := context.item.(Pair)
			if !ok {
				return "", errors.New("item pair is unavailable")
			}
			if token.itemPart == "label" {
				return renderTypedValue(pair.Label, InputTypeText, token.modifier, slot)
			}
			return renderTypedValue(pair.Value, InputTypeText, token.modifier, slot)
		}
		return renderTypedValue(context.item, context.itemType, token.modifier, slot)
	}
	item, ok := values[token.name]
	if !ok {
		return "", fmt.Errorf("input %q is unavailable", token.name)
	}
	return renderTypedValue(item.value, item.input.Type, token.modifier, slot)
}

func renderTypedValue(value any, inputType InputType, modifier, slot string) (string, error) {
	if value == nil {
		return "", nil
	}
	var text string
	switch inputType {
	case InputTypeText, InputTypeID, InputTypeCode, InputTypeLongText, InputTypeEnum:
		text = value.(string)
	case InputTypeTimestamp:
		timestamp, ok := value.(time.Time)
		if !ok {
			return "", errors.New("timestamp value has an invalid Go type")
		}
		text = timestamp.Format(time.RFC3339)
	case InputTypeNumber:
		text = fmt.Sprint(value)
	case InputTypeBool:
		return "", errors.New("bool input is not renderable in text")
	case InputTypeListPair:
		return "", errors.New("list<pair> input is not renderable in text")
	default:
		return "", fmt.Errorf("unsupported input type %q", inputType)
	}
	if inputType == InputTypeTimestamp {
		timestamp := value.(time.Time)
		switch modifier {
		case "hhmm":
			text = timestamp.Format("15:04")
		case "rfc3339":
			text = timestamp.Format(time.RFC3339)
		}
	}
	switch modifier {
	case "upper":
		text = strings.ToUpper(text)
	case "lower":
		text = strings.ToLower(text)
	}
	return escapeSlot(text, inputType, slot, modifier), nil
}

func splitLongText(value string, chunk int) []string {
	if chunk <= 0 {
		return []string{value}
	}
	runes := []rune(value)
	if len(runes) == 0 {
		return []string{""}
	}
	parts := make([]string, 0, (len(runes)+chunk-1)/chunk)
	for start := 0; start < len(runes); start += chunk {
		end := start + chunk
		if end > len(runes) {
			end = len(runes)
		}
		parts = append(parts, string(runes[start:end]))
	}
	return parts
}

func isPresent(value any) bool {
	if value == nil {
		return false
	}
	switch typed := value.(type) {
	case string:
		return typed != ""
	case bool:
		return typed
	case []Pair:
		return len(typed) > 0
	case reflect.Value:
		return typed.IsValid() && !typed.IsZero()
	default:
		return true
	}
}
