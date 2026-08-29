package blockkit

import (
	"errors"
	"fmt"
)

type stringRenderer func(string, renderValues, renderContext, string) (string, error)

func renderNode(node any, doc templateDocument, values renderValues, context renderContext) (any, error) {
	return renderNodeWith(node, doc, values, context, renderString)
}

func renderNodeWith(node any, doc templateDocument, values renderValues, context renderContext, renderText stringRenderer) (any, error) {
	switch typed := node.(type) {
	case nil, bool, float64:
		return typed, nil
	case string:
		return renderText(typed, values, context, "plain_text")
	case []any:
		result := make([]any, 0, len(typed))
		for _, child := range typed {
			rendered, err := renderNodeWith(child, doc, values, context, renderText)
			if err != nil {
				return nil, err
			}
			if expanded, ok := rendered.([]any); ok && isExpansionNode(child) {
				result = append(result, expanded...)
				continue
			}
			result = append(result, rendered)
		}
		return result, nil
	case map[string]any:
		if _, ok := typed["region"]; ok {
			return renderRegion(typed, doc, values, context, renderText)
		}
		if _, ok := typed["actions"]; ok {
			for key := range typed {
				if key != "actions" {
					return nil, fmt.Errorf("actions pseudo-block has unknown key %q", key)
				}
			}
			rendered, err := renderActions(typed, doc, values)
			if err != nil {
				return nil, err
			}
			return renderNodeWith(rendered, doc, values, context, renderText)
		}
		result := make(map[string]any, len(typed))
		slot := textSlot(typed)
		for key, child := range typed {
			if isFixedTemplateKey(key) {
				result[key] = child
				continue
			}
			if text, ok := child.(string); ok {
				// Only values inside a text object inherit its declared slot. Other
				// strings still use plain_text escaping for semantic inputs.
				childSlot := "plain_text"
				if key == "text" {
					childSlot = slot
				}
				rendered, err := renderText(text, values, context, childSlot)
				if err != nil {
					return nil, fmt.Errorf("%s: %w", key, err)
				}
				result[key] = rendered
				continue
			}
			rendered, err := renderNodeWith(child, doc, values, context, renderText)
			if err != nil {
				return nil, err
			}
			result[key] = rendered
		}
		return result, nil
	default:
		return nil, fmt.Errorf("unsupported JSON value %T", node)
	}
}

func isExpansionNode(node any) bool {
	object, ok := node.(map[string]any)
	if !ok {
		return false
	}
	_, region := object["region"]
	return region
}

func renderRegion(object map[string]any, doc templateDocument, values renderValues, context renderContext, renderText stringRenderer) ([]any, error) {
	regionName, _ := object["region"].(string)
	region, ok := values[regionName]
	if !ok {
		return nil, fmt.Errorf("region input %q is unavailable", regionName)
	}
	if whenName, hasWhen := object["when"].(string); hasWhen {
		when, ok := values[whenName]
		if !ok || !isPresent(when.value) {
			return nil, nil
		}
	}
	if !isPresent(region.value) {
		return nil, nil
	}
	if each, ok := object["each"]; ok {
		items, err := regionItems(region)
		if err != nil {
			return nil, err
		}
		result := make([]any, 0, len(items))
		for _, item := range items {
			rendered, err := renderNodeWith(each, doc, values, renderContext{
				item: item, itemType: region.input.Type, itemInput: regionName, hasItem: true,
			}, renderText)
			if err != nil {
				return nil, err
			}
			result = append(result, rendered)
		}
		return result, nil
	}
	block := object["block"]
	rendered, err := renderNodeWith(block, doc, values, context, renderText)
	if err != nil {
		return nil, err
	}
	return []any{rendered}, nil
}

func regionItems(value inputValue) ([]any, error) {
	switch value.input.Type {
	case InputTypeLongText:
		text, _ := value.value.(string)
		return stringsToAny(splitLongText(text, value.input.Chunk)), nil
	case InputTypeListPair:
		pairs, _ := value.value.([]Pair)
		items := make([]any, len(pairs))
		for index, pair := range pairs {
			items[index] = pair
		}
		return items, nil
	default:
		return nil, fmt.Errorf("input %q cannot be repeated", value.input.Type)
	}
}

func stringsToAny(values []string) []any {
	result := make([]any, len(values))
	for index, value := range values {
		result[index] = value
	}
	return result
}

func renderActions(object map[string]any, doc templateDocument, values renderValues) (map[string]any, error) {
	names, ok := object["actions"].([]any)
	if !ok {
		return nil, errors.New("actions pseudo-block must contain an array")
	}
	elements := make([]any, 0, len(names))
	for _, rawName := range names {
		name, _ := rawName.(string)
		action, ok := doc.Actions[name]
		if !ok {
			return nil, fmt.Errorf("unknown action %q", name)
		}
		button := map[string]any{
			"type":      "button",
			"action_id": action.ID,
			"text": map[string]any{
				"type": "plain_text",
				"text": action.Text,
			},
		}
		if action.Style != "" {
			button["style"] = action.Style
		}
		if action.Carries != "" {
			carried := values[action.Carries]
			if carried.value != nil {
				text, err := rawValueString(carried.value, carried.input.Type)
				if err != nil {
					return nil, fmt.Errorf("action %q carries %q: %w", name, action.Carries, err)
				}
				button["value"] = text
			}
		}
		elements = append(elements, button)
	}
	return map[string]any{
		"type":     "actions",
		"block_id": doc.Name + ".actions",
		"elements": elements,
	}, nil
}

func rawValueString(value any, inputType InputType) (string, error) {
	switch inputType {
	case InputTypeText, InputTypeID, InputTypeCode, InputTypeLongText, InputTypeEnum:
		return value.(string), nil
	case InputTypeNumber:
		return fmt.Sprint(value), nil
	case InputTypeTimestamp:
		return value.(interface{ Format(string) string }).Format("2006-01-02T15:04:05Z07:00"), nil
	case InputTypeBool:
		return fmt.Sprint(value), nil
	default:
		return "", fmt.Errorf("type %q is not valid for an action value", inputType)
	}
}
