package blockkit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	templateSchemaVersion = 2
	maxIdentifierLength   = 255
)

func loadTemplates(fsys fs.FS) ([]templateDocument, error) {
	if fsys == nil {
		return nil, errors.New("template filesystem is required")
	}
	var filenames []string
	walkErr := fs.WalkDir(fsys, "templates", func(filename string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if strings.HasSuffix(filename, ".json") {
			filenames = append(filenames, filename)
		}
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("discover templates: %w", walkErr)
	}
	sort.Strings(filenames)

	documents := make([]templateDocument, 0, len(filenames))
	seen := make(map[string]struct{}, len(filenames))
	for _, filename := range filenames {
		name, err := templateName(filename)
		if err != nil {
			return nil, fmt.Errorf("template %q: %w", filename, err)
		}
		if _, exists := seen[name]; exists {
			return nil, fmt.Errorf("template %q: duplicate template name %q", filename, name)
		}
		data, err := fs.ReadFile(fsys, filename)
		if err != nil {
			return nil, fmt.Errorf("read template %q: %w", filename, err)
		}
		doc, err := parseDocument(data, name)
		if err != nil {
			return nil, fmt.Errorf("parse template %q: %w", filename, err)
		}
		seen[name] = struct{}{}
		doc.Name = name
		doc.fileName = filename
		documents = append(documents, doc)
	}
	return documents, nil
}

func templateName(filename string) (string, error) {
	if !strings.HasPrefix(filename, "templates/") || !strings.HasSuffix(filename, ".json") {
		return "", errors.New("template path must be below templates and end in .json")
	}
	relative := strings.TrimSuffix(strings.TrimPrefix(filename, "templates/"), ".json")
	if relative == "" {
		return "", errors.New("template filename is empty")
	}
	segments := strings.Split(relative, "/")
	for _, segment := range segments {
		if !validName(segment) {
			return "", fmt.Errorf("path segment %q must match [a-z0-9_]+", segment)
		}
	}
	return strings.Join(segments, "."), nil
}

func parseDocument(data []byte, name string) (templateDocument, error) {
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return templateDocument{}, err
	}
	var raw rawTemplateDocument
	if err := decodeStrictJSON(data, &raw); err != nil {
		return templateDocument{}, err
	}
	if raw.SchemaVersion != templateSchemaVersion {
		return templateDocument{}, fmt.Errorf("schema_version must be %d", templateSchemaVersion)
	}
	if raw.Surface != "message" && raw.Surface != "modal" {
		return templateDocument{}, fmt.Errorf("surface must be message or modal, got %q", raw.Surface)
	}
	if raw.Contract == nil {
		return templateDocument{}, errors.New("contract is required")
	}
	if raw.Layout == nil {
		return templateDocument{}, errors.New("layout is required")
	}
	layout, ok := raw.Layout.([]any)
	if !ok {
		return templateDocument{}, errors.New("layout must be an array")
	}
	if len(layout) == 0 {
		return templateDocument{}, errors.New("layout must contain at least one block")
	}

	fallback, fallbackSet, err := decodeOptionalValue(raw.Fallback, "fallback")
	if err != nil {
		return templateDocument{}, err
	}
	title, titleSet, err := decodeOptionalValue(raw.Title, "title")
	if err != nil {
		return templateDocument{}, err
	}
	submit, submitSet, err := decodeOptionalValue(raw.Submit, "submit")
	if err != nil {
		return templateDocument{}, err
	}
	closeValue, closeSet, err := decodeOptionalValue(raw.Close, "close")
	if err != nil {
		return templateDocument{}, err
	}
	fallbackText, err := optionalString(fallback, fallbackSet)
	if err != nil {
		return templateDocument{}, err
	}
	var callbackID string
	callbackSet := len(raw.CallbackID) != 0
	if callbackSet {
		if string(raw.CallbackID) == "null" {
			return templateDocument{}, errors.New("callback_id must be a string")
		}
		if err := json.Unmarshal(raw.CallbackID, &callbackID); err != nil {
			return templateDocument{}, fmt.Errorf("callback_id: %w", err)
		}
	}
	doc := templateDocument{
		Name:       name,
		Surface:    raw.Surface,
		Inputs:     make(map[string]Input, len(raw.Contract.Inputs)),
		Actions:    make(map[string]Action, len(raw.Contract.Actions)),
		Layout:     raw.Layout,
		Fallback:   fallbackText,
		Title:      optionalValue(title, titleSet),
		Submit:     optionalValue(submit, submitSet),
		Close:      optionalValue(closeValue, closeSet),
		CallbackID: callbackID,
	}

	for inputName, input := range raw.Contract.Inputs {
		doc.Inputs[inputName] = Input{
			Type: input.Type, Required: input.Required, Max: input.Max,
			Default: input.Default, Chunk: input.Chunk,
			OneOf: append([]string(nil), input.OneOf...),
		}
	}
	for actionName, action := range raw.Contract.Actions {
		doc.Actions[actionName] = Action(action)
	}
	if raw.Surface == "message" && (titleSet || submitSet || closeSet || callbackSet) {
		return templateDocument{}, errors.New("modal fields are not valid on message surface")
	}
	if raw.Surface == "modal" && fallbackSet {
		return templateDocument{}, errors.New("fallback is only valid on message surface")
	}
	return doc, nil
}

func decodeOptionalValue(data json.RawMessage, field string) (any, bool, error) {
	if len(data) == 0 {
		return nil, false, nil
	}
	if string(data) == "null" {
		return nil, true, fmt.Errorf("%s must not be null", field)
	}
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, true, fmt.Errorf("%s: %w", field, err)
	}
	return value, true, nil
}

func optionalValue(value any, set bool) any {
	if !set {
		return nil
	}
	return value
}

func optionalString(value any, set bool) (*string, error) {
	if !set {
		return nil, nil
	}
	text, ok := value.(string)
	if !ok {
		return nil, errors.New("fallback must be a string")
	}
	return &text, nil
}

func validateDocument(doc templateDocument) error {
	if doc.Surface == "modal" {
		if doc.Title == nil {
			return errors.New("title is required for modal surface")
		}
		if doc.CallbackID == "" {
			return errors.New("callback_id is required for modal surface")
		}
	}
	if doc.Fallback != nil {
		if err := validateTemplateString(*doc.Fallback, "fallback", doc, renderContext{}); err != nil {
			return err
		}
	}
	for name, input := range doc.Inputs {
		if !validName(name) {
			return fmt.Errorf("input %q must match [a-z0-9_]+", name)
		}
		if err := validateInput(input); err != nil {
			return fmt.Errorf("input %q: %w", name, err)
		}
	}
	for name, action := range doc.Actions {
		if !validName(name) {
			return fmt.Errorf("action %q must match [a-z0-9_]+", name)
		}
		if action.Text == "" {
			return fmt.Errorf("action %q text must not be empty", name)
		}
		if utf8.RuneCountInString(action.Text) > 75 {
			return fmt.Errorf("action %q text exceeds 75 code points", name)
		}
		if err := validateIdentifier(action.ID, fmt.Sprintf("action %q id", name)); err != nil {
			return err
		}
		switch action.Style {
		case "", "primary", "danger":
		default:
			return fmt.Errorf("action %q style %q is invalid", name, action.Style)
		}
		if action.Carries != "" {
			if _, ok := doc.Inputs[action.Carries]; !ok {
				return fmt.Errorf("action %q carries unknown input %q", name, action.Carries)
			}
		}
	}
	if doc.Surface == "modal" {
		if err := validateIdentifier(doc.CallbackID, "callback_id"); err != nil {
			return err
		}
		for fieldName, value := range map[string]any{"title": doc.Title, "submit": doc.Submit, "close": doc.Close} {
			if value == nil {
				continue
			}
			if err := validateTemplateString(value, fieldName, doc, renderContext{}); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateInput(input Input) error {
	if input.Required && input.Default != "" {
		return errors.New("required input must not define a default")
	}
	switch input.Type {
	case InputTypeText, InputTypeID, InputTypeCode, InputTypeLongText, InputTypeTimestamp,
		InputTypeNumber, InputTypeEnum, InputTypeBool, InputTypeListPair:
	default:
		return fmt.Errorf("unsupported type %q", input.Type)
	}
	if input.Max < 0 {
		return errors.New("max must not be negative")
	}
	if input.Chunk != 0 && input.Type != InputTypeLongText {
		return errors.New("chunk is only valid for longtext")
	}
	if input.Chunk < 0 {
		return errors.New("chunk must not be negative")
	}
	if input.Type != InputTypeEnum && len(input.OneOf) != 0 {
		return errors.New("one_of is only valid for enum")
	}
	if input.Type == InputTypeEnum && len(input.OneOf) == 0 {
		return errors.New("enum requires one_of")
	}
	if input.Type == InputTypeEnum {
		seen := make(map[string]struct{}, len(input.OneOf))
		for _, item := range input.OneOf {
			if item == "" {
				return errors.New("enum one_of values must not be empty")
			}
			if input.Max > 0 && utf8.RuneCountInString(item) > input.Max {
				return errors.New("enum one_of value exceeds max")
			}
			if _, exists := seen[item]; exists {
				return fmt.Errorf("enum one_of contains duplicate value %q", item)
			}
			seen[item] = struct{}{}
		}
	}
	if input.Default != "" {
		if _, err := parseInputValue(input, input.Default); err != nil {
			return fmt.Errorf("default is invalid: %w", err)
		}
		if input.Type == InputTypeEnum && !contains(input.OneOf, input.Default) {
			return fmt.Errorf("default %q is not in one_of", input.Default)
		}
		if input.Max > 0 && input.Type != InputTypeListPair && utf8.RuneCountInString(input.Default) > input.Max {
			return fmt.Errorf("default exceeds max %d code points", input.Max)
		}
	}
	return nil
}

func representativeValues(inputs map[string]Input, includeOptional bool) (renderValues, error) {
	values := make(renderValues, len(inputs))
	for name, input := range inputs {
		if !includeOptional && !input.Required && input.Default == "" {
			values[name] = inputValue{input: input}
			continue
		}
		value, err := representativeValue(input)
		if err != nil {
			return nil, fmt.Errorf("input %q: %w", name, err)
		}
		values[name] = inputValue{input: input, value: value}
	}
	return values, nil
}

func representativeValue(input Input) (any, error) {
	var value any
	switch input.Type {
	case InputTypeText:
		value = "representative text"
	case InputTypeID:
		value = "id-1"
	case InputTypeCode:
		value = "code-1"
	case InputTypeLongText:
		value = "representative long text"
	case InputTypeTimestamp:
		value = time.Date(2030, time.January, 2, 15, 4, 5, 0, time.UTC)
	case InputTypeNumber:
		value = int64(42)
	case InputTypeEnum:
		value = input.OneOf[0]
	case InputTypeBool:
		value = true
	case InputTypeListPair:
		value = []Pair{{Label: "label", Value: "value"}}
	default:
		return nil, fmt.Errorf("unsupported type %q", input.Type)
	}
	if input.Chunk > 0 && input.Type == InputTypeLongText {
		length := input.Chunk + 1
		if input.Max > 0 && length > input.Max {
			length = input.Max
		}
		value = strings.Repeat("x", length)
	}
	if text, ok := value.(string); ok && input.Max > 0 {
		value = truncateCodePoints(text, input.Max)
	}
	return value, nil
}

func parseInputValue(input Input, text string) (any, error) {
	switch input.Type {
	case InputTypeText, InputTypeID, InputTypeCode, InputTypeLongText, InputTypeEnum:
		return text, nil
	case InputTypeTimestamp:
		parsed, err := time.Parse(time.RFC3339, text)
		if err != nil {
			return nil, err
		}
		return parsed, nil
	case InputTypeNumber:
		parsed, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return nil, err
		}
		return parsed, nil
	case InputTypeBool:
		parsed, err := strconv.ParseBool(text)
		if err != nil {
			return nil, err
		}
		return parsed, nil
	case InputTypeListPair:
		return nil, errors.New("list<pair> does not support a default")
	default:
		return nil, fmt.Errorf("unsupported input type %q", input.Type)
	}
}

func validateInputValues(values renderValues) error {
	for name, item := range values {
		if item.value == nil {
			if item.input.Required {
				return fmt.Errorf("input %q is required", name)
			}
			continue
		}
		if item.input.Max > 0 && item.input.Type != InputTypeListPair {
			text, err := rawValueString(item.value, item.input.Type)
			if err != nil {
				return fmt.Errorf("input %q: %w", name, err)
			}
			if utf8.RuneCountInString(text) > item.input.Max {
				return fmt.Errorf("input %q exceeds max %d code points", name, item.input.Max)
			}
		}
		switch item.input.Type {
		case InputTypeText, InputTypeID, InputTypeCode, InputTypeLongText, InputTypeEnum:
			text := item.value.(string)
			if item.input.Required && text == "" {
				return fmt.Errorf("input %q must not be empty", name)
			}
			if item.input.Max > 0 && utf8.RuneCountInString(text) > item.input.Max {
				return fmt.Errorf("input %q exceeds max %d code points", name, item.input.Max)
			}
			if item.input.Type == InputTypeEnum && !contains(item.input.OneOf, text) {
				return fmt.Errorf("input %q value %q is not in one_of", name, text)
			}
		case InputTypeTimestamp:
			if item.input.Required && item.value.(time.Time).IsZero() {
				return fmt.Errorf("input %q must not be zero", name)
			}
		case InputTypeListPair:
			if item.input.Required && item.value.([]Pair) == nil {
				return fmt.Errorf("input %q is required", name)
			}
		}
	}
	return nil
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func validName(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '_' {
			return false
		}
	}
	return true
}

func validateIdentifier(value, field string) error {
	if hasPlaceholderMarker(value) {
		return fmt.Errorf("%s may not contain a placeholder", field)
	}
	if value == "" {
		return fmt.Errorf("%s must not be empty", field)
	}
	if utf8.RuneCountInString(value) > maxIdentifierLength {
		return fmt.Errorf("%s exceeds %d code points", field, maxIdentifierLength)
	}
	for _, character := range value {
		if character < 0x21 || character > 0x7e {
			return fmt.Errorf("%s must contain printable ASCII only", field)
		}
	}
	return nil
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
	delim, isDelimiter := token.(json.Delim)
	if !isDelimiter {
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
			key := keyToken.(string)
			if _, exists := keys[key]; exists {
				return fmt.Errorf("duplicate JSON key %q", key)
			}
			keys[key] = struct{}{}
			if err := walkJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err := decoder.Token()
		return err
	case '[':
		for decoder.More() {
			if err := walkJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err := decoder.Token()
		return err
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
}
