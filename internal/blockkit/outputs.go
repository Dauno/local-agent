package blockkit

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"

	slackapi "github.com/slack-go/slack"
)

// SubmitError reports a value that could not be read from one modal block.
type SubmitError struct {
	BlockID string
	Err     error
}

func (e *SubmitError) Error() string {
	if e == nil {
		return "modal submit value is invalid"
	}
	return fmt.Sprintf("modal block %q: %v", e.BlockID, e.Err)
}

func (e *SubmitError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func validateOutputContract(doc *templateDocument) error {
	if doc.Surface != "modal" {
		if len(doc.Outputs) != 0 {
			return errors.New("outputs are only valid on modal surface")
		}
		return nil
	}

	inputBlocks := make(map[string]submitField)
	if err := collectInputBlocks(doc.Layout, "layout", inputBlocks); err != nil {
		return err
	}
	doc.inputBlocks = inputBlocks

	declaredBlocks := make(map[string]string, len(doc.Outputs))
	outputNames := make([]string, 0, len(doc.Outputs))
	for name := range doc.Outputs {
		outputNames = append(outputNames, name)
	}
	sort.Strings(outputNames)
	for _, name := range outputNames {
		output := doc.Outputs[name]
		if !validName(name) {
			return fmt.Errorf("output %q must match [a-z0-9_]+", name)
		}
		if err := validateOutput(output); err != nil {
			return fmt.Errorf("output %q: %w", name, err)
		}
		blockID := output.Block
		if blockID == "" {
			blockID = name
		}
		actionID := output.Action
		if actionID == "" {
			actionID = name
		}
		inputBlock, ok := inputBlocks[blockID]
		if !ok {
			return fmt.Errorf("output %q names unknown input block %q", name, blockID)
		}
		if inputBlock.actionID != actionID {
			return fmt.Errorf("output %q action %q does not match input block %q action %q", name, actionID, blockID, inputBlock.actionID)
		}
		if err := validateOutputElement(output.Type, inputBlock.elementType); err != nil {
			return fmt.Errorf("output %q: %w", name, err)
		}
		if previous, exists := declaredBlocks[blockID]; exists {
			return fmt.Errorf("input block %q is declared by outputs %q and %q", blockID, previous, name)
		}
		declaredBlocks[blockID] = name
	}

	blockIDs := make([]string, 0, len(inputBlocks))
	for blockID := range inputBlocks {
		blockIDs = append(blockIDs, blockID)
	}
	sort.Strings(blockIDs)
	for _, blockID := range blockIDs {
		if _, declared := declaredBlocks[blockID]; !declared {
			return fmt.Errorf("input block %q has no declared output", blockID)
		}
	}
	return nil
}

func validateOutput(output Output) error {
	switch output.Type {
	case OutputTypeText, OutputTypeNumber, OutputTypeEnum, OutputTypeEnumOpen:
	default:
		return fmt.Errorf("unsupported type %q", output.Type)
	}
	if output.Type != OutputTypeEnum && len(output.OneOf) != 0 {
		return errors.New("one_of is only valid for enum")
	}
	if output.Type == OutputTypeEnum && len(output.OneOf) == 0 {
		return errors.New("enum requires one_of")
	}
	if output.Type == OutputTypeEnumOpen && len(output.OneOf) != 0 {
		return errors.New("one_of is not valid for enum_open")
	}
	if output.Type == OutputTypeEnum {
		seen := make(map[string]struct{}, len(output.OneOf))
		for _, item := range output.OneOf {
			if item == "" {
				return errors.New("enum one_of values must not be empty")
			}
			if _, exists := seen[item]; exists {
				return fmt.Errorf("enum one_of contains duplicate value %q", item)
			}
			seen[item] = struct{}{}
		}
	}
	return nil
}

func validateOutputElement(outputType OutputType, elementType string) error {
	switch outputType {
	case OutputTypeText, OutputTypeNumber:
		if elementType != string(slackapi.METPlainTextInput) {
			return fmt.Errorf("type %q requires a plain_text_input, got %q", outputType, elementType)
		}
	case OutputTypeEnum, OutputTypeEnumOpen:
		if !isSingleSelectElement(elementType) {
			return fmt.Errorf("type %q requires a select, got %q", outputType, elementType)
		}
	}
	return nil
}

func isSingleSelectElement(elementType string) bool {
	switch elementType {
	case slackapi.OptTypeStatic, slackapi.OptTypeExternal:
		return true
	default:
		return false
	}
}

func collectInputBlocks(node any, field string, blocks map[string]submitField) error {
	switch typed := node.(type) {
	case nil, bool, float64, string:
		return nil
	case []any:
		for index, child := range typed {
			if err := collectInputBlocks(child, fmt.Sprintf("%s[%d]", field, index), blocks); err != nil {
				return err
			}
		}
	case map[string]any:
		if _, isRegion := typed["region"]; isRegion {
			for _, key := range []string{"each", "block"} {
				if child, exists := typed[key]; exists {
					if err := collectInputBlocks(child, field+"."+key, blocks); err != nil {
						return err
					}
				}
			}
			return nil
		}
		if typeName, _ := typed["type"].(string); typeName == string(slackapi.MBTInput) {
			return collectInputBlock(typed, field, blocks)
		}
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if err := collectInputBlocks(typed[key], field+"."+key, blocks); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("%s contains unsupported JSON value %T", field, node)
	}
	return nil
}

func collectInputBlock(node map[string]any, field string, blocks map[string]submitField) error {
	blockID, ok := node["block_id"].(string)
	if !ok || blockID == "" {
		return fmt.Errorf("%s must define a block_id", field)
	}
	element, ok := node["element"].(map[string]any)
	if !ok {
		return fmt.Errorf("%s must define an element object", field)
	}
	elementType, ok := element["type"].(string)
	if !ok || elementType == "" {
		return fmt.Errorf("%s.element must define a type", field)
	}
	actionID, ok := element["action_id"].(string)
	if !ok || actionID == "" {
		return fmt.Errorf("%s.element must define an action_id", field)
	}
	if _, exists := blocks[blockID]; exists {
		return fmt.Errorf("%s has duplicate input block_id %q", field, blockID)
	}
	blocks[blockID] = submitField{blockID: blockID, actionID: actionID, elementType: elementType}
	return nil
}

// RegisterSubmit binds modal output targets to their templates.
func (e *Engine) RegisterSubmit(targets ...View) error {
	if e == nil {
		return errors.New("engine is nil")
	}
	pending := make(map[string]submitBinding, len(targets))
	for _, target := range targets {
		name, err := templateNameForView(target)
		if err != nil {
			return err
		}
		doc, ok := e.templates[name]
		if !ok {
			return fmt.Errorf("submit target template %q is not registered", name)
		}
		if doc.Surface != "modal" {
			return fmt.Errorf("submit target template %q is not a modal template", name)
		}
		if _, exists := e.submitBindings[name]; exists {
			return fmt.Errorf("submit target template %q is registered more than once", name)
		}
		if _, exists := pending[name]; exists {
			return fmt.Errorf("submit target template %q is registered more than once", name)
		}
		binding, err := bindSubmit(target, doc)
		if err != nil {
			return fmt.Errorf("template %q: %w", name, err)
		}
		pending[name] = binding
	}
	for name, binding := range pending {
		e.submitBindings[name] = binding
	}
	return nil
}

// Submit decodes modal state into a registered output target.
func (e *Engine) Submit(target any, state map[string]map[string]slackapi.BlockAction) error {
	if e == nil {
		return errors.New("engine is nil")
	}
	view, ok := target.(View)
	if !ok {
		return errors.New("submit target must implement View")
	}
	name, err := templateNameForView(view)
	if err != nil {
		return err
	}
	doc, ok := e.templates[name]
	if !ok {
		return fmt.Errorf("submit target template %q is not registered", name)
	}
	binding, ok := e.submitBindings[name]
	if !ok {
		return fmt.Errorf("submit target template %q has no registered submit type", name)
	}
	value := reflect.ValueOf(target)
	if value.Kind() != reflect.Pointer || value.IsNil() {
		return errors.New("submit target must be a non-nil pointer to a struct")
	}
	value = value.Elem()
	if value.Type() != binding.typeOf {
		return fmt.Errorf("submit target type %s does not match registered type %s", value.Type(), binding.typeOf)
	}

	outputNames := make([]string, 0, len(doc.Outputs))
	for outputName := range doc.Outputs {
		outputNames = append(outputNames, outputName)
	}
	sort.Strings(outputNames)
	for _, outputName := range outputNames {
		field := binding.fields[outputName]
		value.FieldByIndex(field.index).Set(reflect.Zero(value.FieldByIndex(field.index).Type()))
	}
	for _, outputName := range outputNames {
		output := doc.Outputs[outputName]
		blockID := output.Block
		if blockID == "" {
			blockID = outputName
		}
		actionID := output.Action
		if actionID == "" {
			actionID = outputName
		}
		inputBlock := doc.inputBlocks[blockID]
		action, found := state[blockID][actionID]
		raw, present := submitActionValue(action, inputBlock.elementType)
		if !found || !present || raw == "" {
			if output.Required {
				return &SubmitError{BlockID: blockID, Err: errors.New("required output is missing")}
			}
			continue
		}
		parsed, err := parseOutputValue(output, raw)
		if err != nil {
			return &SubmitError{BlockID: blockID, Err: err}
		}
		fieldValue := value.FieldByIndex(binding.fields[outputName].index)
		if err := assignOutputValue(fieldValue, parsed, output.Type); err != nil {
			return &SubmitError{BlockID: blockID, Err: err}
		}
	}
	return nil
}

func submitActionValue(action slackapi.BlockAction, elementType string) (string, bool) {
	if elementType == string(slackapi.METPlainTextInput) {
		return action.Value, action.Value != ""
	}
	if isSingleSelectElement(elementType) {
		return action.SelectedOption.Value, action.SelectedOption.Value != ""
	}
	return "", false
}

func parseOutputValue(output Output, raw string) (any, error) {
	switch output.Type {
	case OutputTypeText, OutputTypeEnum, OutputTypeEnumOpen:
		if output.Type == OutputTypeEnum && !contains(output.OneOf, raw) {
			return nil, errors.New("output value is not in one_of")
		}
		return raw, nil
	case OutputTypeNumber:
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, errors.New("output number must be an integer")
		}
		return parsed, nil
	default:
		return nil, fmt.Errorf("unsupported output type %q", output.Type)
	}
}

func assignOutputValue(field reflect.Value, parsed any, outputType OutputType) error {
	switch outputType {
	case OutputTypeText, OutputTypeEnum, OutputTypeEnumOpen:
		field.SetString(parsed.(string))
	case OutputTypeNumber:
		value := parsed.(int64)
		if field.OverflowInt(value) {
			return errors.New("output number is outside the target integer range")
		}
		field.SetInt(value)
	default:
		return fmt.Errorf("unsupported output type %q", outputType)
	}
	return nil
}

func bindSubmit(target View, doc templateDocument) (submitBinding, error) {
	typeOf := reflect.TypeOf(target)
	if typeOf == nil {
		return submitBinding{}, errors.New("submit target type is nil")
	}
	if typeOf.Kind() == reflect.Pointer {
		typeOf = typeOf.Elem()
	}
	if typeOf.Kind() != reflect.Struct {
		return submitBinding{}, fmt.Errorf("submit target type %s must be a struct", typeOf)
	}
	binding := submitBinding{typeOf: typeOf, fields: make(map[string]outputFieldBinding)}
	for index := 0; index < typeOf.NumField(); index++ {
		field := typeOf.Field(index)
		name, tagged, err := fieldLabel(field)
		if err != nil {
			return submitBinding{}, err
		}
		if !tagged {
			continue
		}
		if field.PkgPath != "" {
			return submitBinding{}, fmt.Errorf("field %s is not exported", field.Name)
		}
		output, ok := doc.Outputs[name]
		if !ok {
			return submitBinding{}, fmt.Errorf("field %s names unknown output %q", field.Name, name)
		}
		if _, exists := binding.fields[name]; exists {
			return submitBinding{}, fmt.Errorf("output %q has more than one field", name)
		}
		if err := validateOutputGoType(field.Type, output.Type); err != nil {
			return submitBinding{}, fmt.Errorf("field %s for output %q: %w", field.Name, name, err)
		}
		omitempty := strings.HasSuffix(field.Tag.Get("bk"), ",omitempty")
		if output.Required == omitempty {
			return submitBinding{}, fmt.Errorf("field %s omitempty does not match required=%t", field.Name, output.Required)
		}
		binding.fields[name] = outputFieldBinding{index: field.Index, output: name, omitempty: omitempty}
	}
	for name, output := range doc.Outputs {
		if _, ok := binding.fields[name]; !ok {
			if output.Required {
				return submitBinding{}, fmt.Errorf("required contract output %q has no field", name)
			}
			return submitBinding{}, fmt.Errorf("optional contract output %q has no field", name)
		}
	}
	return binding, nil
}

func validateOutputGoType(fieldType reflect.Type, outputType OutputType) error {
	switch outputType {
	case OutputTypeNumber:
		if fieldType.Kind() != reflect.Int && fieldType.Kind() != reflect.Int64 {
			return fmt.Errorf("type %s does not match number", fieldType)
		}
	case OutputTypeText, OutputTypeEnum, OutputTypeEnumOpen:
		if fieldType.Kind() != reflect.String {
			return fmt.Errorf("type %s does not match %s", fieldType, outputType)
		}
	default:
		return fmt.Errorf("unsupported output type %q", outputType)
	}
	return nil
}
