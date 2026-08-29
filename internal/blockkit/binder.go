package blockkit

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"
)

func bindView(view View, doc templateDocument) (viewBinding, error) {
	typeOf := reflect.TypeOf(view)
	if typeOf == nil {
		return viewBinding{}, errors.New("view type is nil")
	}
	if typeOf.Kind() == reflect.Pointer {
		typeOf = typeOf.Elem()
	}
	if typeOf.Kind() != reflect.Struct {
		return viewBinding{}, fmt.Errorf("view type %s must be a struct", typeOf)
	}
	binding := viewBinding{typeOf: typeOf, fields: make(map[string]fieldBinding)}
	for index := 0; index < typeOf.NumField(); index++ {
		field := typeOf.Field(index)
		name, tagged, err := fieldLabel(field)
		if err != nil {
			return viewBinding{}, err
		}
		if !tagged {
			continue
		}
		if field.PkgPath != "" {
			return viewBinding{}, fmt.Errorf("field %s is not exported", field.Name)
		}
		input, ok := doc.Inputs[name]
		if !ok {
			return viewBinding{}, fmt.Errorf("field %s names unknown input %q", field.Name, name)
		}
		if _, exists := binding.fields[name]; exists {
			return viewBinding{}, fmt.Errorf("input %q has more than one field", name)
		}
		if err := validateGoType(field.Type, input.Type); err != nil {
			return viewBinding{}, fmt.Errorf("field %s for input %q: %w", field.Name, name, err)
		}
		omitempty := strings.HasSuffix(field.Tag.Get("bk"), ",omitempty")
		if input.Required == omitempty {
			return viewBinding{}, fmt.Errorf("field %s omitempty does not match required=%t", field.Name, input.Required)
		}
		binding.fields[name] = fieldBinding{index: field.Index, input: name, omitempty: omitempty}
	}
	for name, input := range doc.Inputs {
		if _, ok := binding.fields[name]; !ok {
			return viewBinding{}, fmt.Errorf("required contract input %q has no field", name)
		}
		if !input.Required && !binding.fields[name].omitempty {
			return viewBinding{}, fmt.Errorf("optional input %q must use omitempty", name)
		}
	}
	return binding, nil
}

func validateGoType(fieldType reflect.Type, inputType InputType) error {
	switch inputType {
	case InputTypeTimestamp:
		if !isTimeType(fieldType) {
			return fmt.Errorf("type %s does not match timestamp", fieldType)
		}
	case InputTypeListPair:
		if !isPairSliceType(fieldType) {
			return fmt.Errorf("type %s does not match list<pair>", fieldType)
		}
	case InputTypeNumber:
		if fieldType.Kind() != reflect.Int && fieldType.Kind() != reflect.Int64 {
			return fmt.Errorf("type %s does not match number", fieldType)
		}
	case InputTypeBool:
		if fieldType.Kind() != reflect.Bool {
			return fmt.Errorf("type %s does not match bool", fieldType)
		}
	default:
		if fieldType.Kind() != reflect.String {
			return fmt.Errorf("type %s does not match %s", fieldType, inputTypeName(inputType))
		}
	}
	return nil
}

func viewValues(view View, binding viewBinding, doc templateDocument) (renderValues, error) {
	viewType := reflect.TypeOf(view)
	if viewType.Kind() == reflect.Pointer {
		viewType = viewType.Elem()
	}
	if viewType != binding.typeOf {
		return nil, fmt.Errorf("view type %s does not match registered type %s", viewType, binding.typeOf)
	}
	value := reflect.ValueOf(view)
	if value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return nil, errors.New("view pointer is nil")
		}
		value = value.Elem()
	}
	values := make(renderValues, len(doc.Inputs))
	for name, input := range doc.Inputs {
		field := binding.fields[name]
		fieldValue := value.FieldByIndex(field.index)
		if field.omitempty && isZeroValue(fieldValue) {
			if input.Default == "" {
				values[name] = inputValue{input: input}
				continue
			}
			defaultValue, err := parseInputValue(input, input.Default)
			if err != nil {
				return nil, fmt.Errorf("input %q default: %w", name, err)
			}
			values[name] = inputValue{input: input, value: defaultValue}
			continue
		}
		converted, err := normalizeFieldValue(fieldValue, input.Type)
		if err != nil {
			return nil, fmt.Errorf("input %q: %w", name, err)
		}
		values[name] = inputValue{input: input, value: converted}
	}
	return values, validateInputValues(values)
}

func normalizeFieldValue(value reflect.Value, inputType InputType) (any, error) {
	switch inputType {
	case InputTypeTimestamp:
		return value.Interface().(time.Time), nil
	case InputTypeListPair:
		if value.IsNil() {
			return []Pair(nil), nil
		}
		pairs := make([]Pair, value.Len())
		for index := range pairs {
			pairs[index] = value.Index(index).Interface().(Pair)
		}
		return pairs, nil
	case InputTypeNumber:
		return value.Int(), nil
	case InputTypeBool:
		return value.Bool(), nil
	default:
		return value.String(), nil
	}
}
