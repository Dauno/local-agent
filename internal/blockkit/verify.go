package blockkit

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"unicode/utf8"

	slackapi "github.com/slack-go/slack"
)

const (
	maxMessageBlocks = 50
	maxModalBlocks   = 100
	maxTextLength    = 3000
	maxSectionFields = 10
	maxSectionField  = 2000
	maxHeaderText    = 150
	maxButtonText    = 75
	maxOptionText    = 75
	maxValueLength   = 2000
)

type verificationState struct {
	doc        templateDocument
	blockCount int
	blockIDs   map[string]struct{}
	actionIDs  map[string]struct{}
}

func verifyCompiled(doc templateDocument, blocks []slackapi.Block) error {
	state := verificationState{
		doc: doc, blockIDs: make(map[string]struct{}), actionIDs: make(map[string]struct{}),
	}
	for index, block := range blocks {
		if block == nil {
			return fmt.Errorf("blocks[%d] is nil", index)
		}
		if err := walkSlackValue(reflect.ValueOf(block), fmt.Sprintf("blocks[%d]", index), &state, maxTextLength, false); err != nil {
			return err
		}
	}
	limit := maxMessageBlocks
	if doc.Surface == "modal" {
		limit = maxModalBlocks
	}
	if state.blockCount > limit {
		return fmt.Errorf("compiled layout contains %d blocks, maximum is %d", state.blockCount, limit)
	}
	return nil
}

func walkSlackValue(value reflect.Value, field string, state *verificationState, textLimit int, allowStateActionID bool) error {
	if !value.IsValid() {
		return nil
	}
	if value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return nil
		}
		return walkSlackValue(value.Elem(), field, state, textLimit, allowStateActionID)
	}
	if value.CanInterface() {
		if unknown, ok := value.Interface().(slackapi.UnknownBlock); ok {
			return fmt.Errorf("%s has unknown block type %q", field, unknown.Type)
		}
		if unknown, ok := value.Interface().(slackapi.UnknownBlockElement); ok {
			return fmt.Errorf("%s has unknown block element type %q", field, unknown.Type)
		}
		if block, ok := value.Interface().(slackapi.Block); ok {
			if err := verifyBlock(block, field, state); err != nil {
				return err
			}
		}
		isTextObject := false
		if text, ok := value.Interface().(slackapi.TextBlockObject); ok {
			isTextObject = true
			if err := text.Validate(); err != nil {
				byteLimitError := strings.Contains(err.Error(), "text cannot be longer")
				if !byteLimitError || utf8.RuneCountInString(text.Text) > maxTextLength {
					return fmt.Errorf("%s: %w", field, err)
				}
			}
			if err := validateTextLength(text, textLimit, field); err != nil {
				return err
			}
		}
		if err := validateSlackFieldLimits(value.Interface(), field); err != nil {
			return err
		}
		if !isTextObject {
			if validator, ok := value.Interface().(interface{ Validate() error }); ok {
				if err := validator.Validate(); err != nil {
					return fmt.Errorf("%s: %w", field, err)
				}
			}
		}
	}
	switch value.Kind() {
	case reflect.Struct:
		input, isInput := value.Interface().(slackapi.InputBlock)
		for index := 0; index < value.NumField(); index++ {
			fieldType := value.Type().Field(index)
			if !fieldType.IsExported() {
				continue
			}
			childLimit := textLimitForField(value, fieldType.Name, textLimit)
			child := value.Field(index)
			if fieldType.Name == "ActionID" && child.Kind() == reflect.String && child.String() != "" {
				if err := verifyActionID(child.String(), field+"."+fieldType.Name, state, allowStateActionID); err != nil {
					return err
				}
			}
			childAllowStateActionID := isInput && fieldType.Name == "Element" && !inputElementDispatches(input)
			if err := walkSlackValue(child, field+"."+fieldType.Name, state, childLimit, childAllowStateActionID); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		for index := 0; index < value.Len(); index++ {
			if err := walkSlackValue(value.Index(index), fmt.Sprintf("%s[%d]", field, index), state, textLimit, false); err != nil {
				return err
			}
		}
	case reflect.Map:
		keys := value.MapKeys()
		for _, key := range keys {
			if err := walkSlackValue(value.MapIndex(key), field+"[map]", state, textLimit, false); err != nil {
				return err
			}
		}
	}
	return nil
}

func verifyBlock(block slackapi.Block, field string, state *verificationState) error {
	state.blockCount++
	if blockType := block.BlockType(); blockType == slackapi.MBTInput && state.doc.Surface == "message" {
		return fmt.Errorf("%s: input blocks are not valid on message surface", field)
	}
	switch section := block.(type) {
	case slackapi.SectionBlock:
		if len(section.Fields) > maxSectionFields {
			return fmt.Errorf("%s.fields exceeds %d", field, maxSectionFields)
		}
	case *slackapi.SectionBlock:
		if len(section.Fields) > maxSectionFields {
			return fmt.Errorf("%s.fields exceeds %d", field, maxSectionFields)
		}
	}
	blockID := block.ID()
	if blockID != "" {
		if err := validateIdentifier(blockID, field+".block_id"); err != nil {
			return err
		}
		if _, exists := state.blockIDs[blockID]; exists {
			return fmt.Errorf("%s has duplicate block_id %q", field, blockID)
		}
		state.blockIDs[blockID] = struct{}{}
	}
	return nil
}

func verifyActionID(actionID, field string, state *verificationState, allowStateActionID bool) error {
	if err := validateIdentifier(actionID, field); err != nil {
		return err
	}
	if _, exists := state.actionIDs[actionID]; exists {
		return fmt.Errorf("%s is a duplicate action_id %q", field, actionID)
	}
	state.actionIDs[actionID] = struct{}{}
	declared := false
	for _, action := range state.doc.Actions {
		if action.ID == actionID {
			declared = true
			break
		}
	}
	if !declared && !allowStateActionID {
		return fmt.Errorf("%s %q is not declared in contract.actions", field, actionID)
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

func validateSlackFieldLimits(value any, field string) error {
	switch typed := value.(type) {
	case slackapi.ButtonBlockElement:
		if utf8.RuneCountInString(typed.Value) > maxValueLength {
			return fmt.Errorf("%s.value exceeds %d code points", field, maxValueLength)
		}
	case slackapi.OptionBlockObject:
		if utf8.RuneCountInString(typed.Value) > maxValueLength {
			return fmt.Errorf("%s.value exceeds %d code points", field, maxValueLength)
		}
	case slackapi.ImageBlock:
		if utf8.RuneCountInString(typed.AltText) > 2000 {
			return fmt.Errorf("%s.alt_text exceeds 2000 code points", field)
		}
	case slackapi.ImageBlockElement:
		if utf8.RuneCountInString(typed.AltText) > 2000 {
			return fmt.Errorf("%s.alt_text exceeds 2000 code points", field)
		}
	case slackapi.PlainTextInputBlockElement:
		if typed.MaxLength > maxTextLength {
			return fmt.Errorf("%s.max_length exceeds %d", field, maxTextLength)
		}
		if typed.MaxLength > 0 && utf8.RuneCountInString(typed.InitialValue) > typed.MaxLength {
			return fmt.Errorf("%s.initial_value exceeds max_length", field)
		}
	}
	return nil
}

func textLimitForField(parent reflect.Value, field string, inherited int) int {
	if !parent.IsValid() || parent.Kind() != reflect.Struct || !parent.CanInterface() {
		return inherited
	}
	switch parent.Interface().(type) {
	case slackapi.SectionBlock:
		if field == "Fields" {
			return maxSectionField
		}
	case slackapi.HeaderBlock:
		if field == "Text" {
			return maxHeaderText
		}
	case slackapi.ContainerBlock:
		if field == "Title" || field == "Subtitle" {
			return maxHeaderText
		}
	case slackapi.ButtonBlockElement:
		if field == "Text" {
			return maxButtonText
		}
	case slackapi.OptionBlockObject:
		if field == "Text" || field == "Description" {
			return maxOptionText
		}
	}
	return inherited
}

// Reachable reports whether value reached the compiled tree, escaped as the
// slot it landed in requires. It walks the tree; it does not index into it.
func Reachable(msg Message, value string) bool {
	if value == "" {
		return false
	}
	data, err := json.Marshal(msg.Blocks)
	if err != nil {
		return false
	}
	var tree any
	if json.Unmarshal(data, &tree) != nil {
		return false
	}
	return reachableNode(tree, value)
}

func reachableNode(node any, value string) bool {
	switch typed := node.(type) {
	case string:
		return typed == value || typed == neutralizeUnsafeControls(value) ||
			typed == escapeMrkdwn(value, true) || typed == escapeMrkdwn(value, false) ||
			strings.Contains(typed, neutralizeUnsafeControls(value)) ||
			strings.Contains(typed, escapeMrkdwn(value, true))
	case []any:
		for _, child := range typed {
			if reachableNode(child, value) {
				return true
			}
		}
	case map[string]any:
		for _, child := range typed {
			if reachableNode(child, value) {
				return true
			}
		}
	}
	return false
}

// SlotOf returns the text type of the slot that received the input.
func SlotOf(msg Message, input string) (string, bool) {
	if input == "" {
		return "", false
	}
	if msg.inputSlots != nil {
		if slot, ok := msg.inputSlots[input]; ok {
			return slot, true
		}
	}
	data, err := json.Marshal(msg.Blocks)
	if err != nil {
		return "", false
	}
	var tree any
	if json.Unmarshal(data, &tree) != nil {
		return "", false
	}
	return findSlot(tree, input)
}

func findSlot(node any, value string) (string, bool) {
	switch typed := node.(type) {
	case []any:
		for _, child := range typed {
			if slot, ok := findSlot(child, value); ok {
				return slot, true
			}
		}
	case map[string]any:
		if typeName, ok := typed["type"].(string); ok && (typeName == "plain_text" || typeName == "mrkdwn") {
			if text, ok := typed["text"].(string); ok {
				matches := strings.Contains(text, value) ||
					strings.Contains(text, neutralizeUnsafeControls(value)) ||
					strings.Contains(text, escapeMrkdwn(value, true)) ||
					strings.Contains(text, escapeMrkdwn(value, false))
				if matches {
					return typeName, true
				}
			}
		}
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if slot, ok := findSlot(typed[key], value); ok {
				return slot, true
			}
		}
	}
	return "", false
}

func collectInputSlots(node any, doc templateDocument, values renderValues, context renderContext, slots map[string]string) {
	switch typed := node.(type) {
	case []any:
		for _, child := range typed {
			collectInputSlots(child, doc, values, context, slots)
		}
	case map[string]any:
		if regionName, ok := typed["region"].(string); ok {
			region, exists := doc.Inputs[regionName]
			regionValue, present := values[regionName]
			if !exists || !present || !isPresent(regionValue.value) {
				return
			}
			if when, ok := typed["when"].(string); ok {
				whenValue, present := values[when]
				if !present || !isPresent(whenValue.value) {
					return
				}
			}
			if each, ok := typed["each"]; ok {
				collectInputSlots(each, doc, values, renderContext{
					itemType: region.Type, itemInput: regionName, hasItem: true,
				}, slots)
			} else if block, ok := typed["block"]; ok {
				collectInputSlots(block, doc, values, context, slots)
			}
			return
		}
		if _, ok := typed["actions"]; ok {
			return
		}
		slot := textSlot(typed)
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			child := typed[key]
			if text, ok := child.(string); ok {
				placeholders, _ := parsePlaceholders(text)
				for _, token := range placeholders {
					if token.name == "item" {
						if context.hasItem && context.itemInput != "" {
							itemValue, present := values[context.itemInput]
							if present && isPresent(itemValue.value) {
								if _, exists := slots[context.itemInput]; !exists {
									slots[context.itemInput] = slot
								}
							}
						}
					} else if _, exists := slots[token.name]; !exists {
						inputValue, present := values[token.name]
						if present && isPresent(inputValue.value) {
							slots[token.name] = slot
						}
					}
				}
			}
			collectInputSlots(child, doc, values, context, slots)
		}
	}
}
