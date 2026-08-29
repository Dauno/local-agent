package blockkit

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"reflect"
	"sort"
	"strings"
	"time"

	slackapi "github.com/slack-go/slack"
)

// InputType declares the semantic type of a template input.
type InputType string

const (
	InputTypeText      InputType = "text"
	InputTypeID        InputType = "id"
	InputTypeCode      InputType = "code"
	InputTypeLongText  InputType = "longtext"
	InputTypeTimestamp InputType = "timestamp"
	InputTypeNumber    InputType = "number"
	InputTypeEnum      InputType = "enum"
	InputTypeBool      InputType = "bool"
	InputTypeListPair  InputType = "list<pair>"
)

// Input declares one value accepted by a template.
type Input struct {
	Type     InputType
	Required bool
	Max      int
	Default  string
	Chunk    int
	OneOf    []string
}

// Action declares one interactive action in a template.
type Action struct {
	ID      string
	Text    string
	Style   string
	Carries string
}

// Pair is one label and value item for a list<pair> input.
type Pair struct {
	Label string
	Value string
}

// View is implemented by every view model. Template returns the registry name.
type View interface {
	Template() string
}

// Message contains the rendered blocks and their derived metadata.
type Message struct {
	Blocks       []slackapi.Block
	FallbackText string
	LayoutSHA256 string
	inputSlots   map[string]string
}

type templateDocument struct {
	Name         string
	Surface      string
	Inputs       map[string]Input
	Actions      map[string]Action
	Layout       any
	Fallback     *string
	Title        any
	Submit       any
	Close        any
	CallbackID   string
	LayoutSHA256 string
	fileName     string
}

type templateContract struct {
	Inputs  map[string]rawInput  `json:"inputs"`
	Actions map[string]rawAction `json:"actions"`
}

type rawInput struct {
	Type     InputType `json:"type"`
	Required bool      `json:"required"`
	Max      int       `json:"max"`
	Default  string    `json:"default"`
	Chunk    int       `json:"chunk"`
	OneOf    []string  `json:"one_of"`
}

type rawAction struct {
	ID      string `json:"id"`
	Text    string `json:"text"`
	Style   string `json:"style"`
	Carries string `json:"carries"`
}

type rawTemplateDocument struct {
	SchemaVersion int               `json:"schema_version"`
	Surface       string            `json:"surface"`
	Contract      *templateContract `json:"contract"`
	Layout        any               `json:"layout"`
	Fallback      json.RawMessage   `json:"fallback"`
	Title         json.RawMessage   `json:"title"`
	Submit        json.RawMessage   `json:"submit"`
	Close         json.RawMessage   `json:"close"`
	CallbackID    json.RawMessage   `json:"callback_id"`
}

type fieldBinding struct {
	index     []int
	input     string
	omitempty bool
}

type viewBinding struct {
	typeOf reflect.Type
	fields map[string]fieldBinding
}

type inputValue struct {
	input Input
	value any
}

type renderValues map[string]inputValue

// Engine is a loaded, validated template registry.
type Engine struct {
	templates map[string]templateDocument
	bindings  map[string]viewBinding
}

// New loads and validates every template in fsys. It fails if any template is
// malformed, so a broken layout stops the process at startup.
func New(fsys fs.FS) (*Engine, error) {
	documents, err := loadTemplates(fsys)
	if err != nil {
		return nil, err
	}

	engine := &Engine{
		templates: make(map[string]templateDocument, len(documents)),
		bindings:  make(map[string]viewBinding),
	}
	for _, loaded := range documents {
		if err := validateDocument(loaded); err != nil {
			return nil, fmt.Errorf("template %q: %w", loaded.fileName, err)
		}
		if err := validateLayoutContract(loaded); err != nil {
			return nil, fmt.Errorf("template %q: %w", loaded.fileName, err)
		}
		variants := []struct {
			name            string
			includeOptional bool
		}{
			{name: "minimal", includeOptional: false},
			{name: "maximal", includeOptional: true},
		}
		for _, variant := range variants {
			values, err := representativeValues(loaded.Inputs, variant.includeOptional)
			if err != nil {
				return nil, fmt.Errorf("template %q %s variant: %w", loaded.fileName, variant.name, err)
			}
			if err := validateInputValues(values); err != nil {
				return nil, fmt.Errorf("template %q %s variant: %w", loaded.fileName, variant.name, err)
			}
			compiled, err := renderCompiled(loaded, values)
			if err != nil {
				return nil, fmt.Errorf("template %q %s variant: %w", loaded.fileName, variant.name, err)
			}
			if err := verifyCompiled(loaded, compiled.blocks); err != nil {
				return nil, fmt.Errorf("template %q %s variant: %w", loaded.fileName, variant.name, err)
			}
			if err := validateRepresentativeMetadata(loaded, values); err != nil {
				return nil, fmt.Errorf("template %q %s variant: %w", loaded.fileName, variant.name, err)
			}
			if variant.includeOptional {
				loaded.LayoutSHA256 = compiled.layoutSHA256
			}
		}
		engine.templates[loaded.Name] = loaded
	}
	return engine, nil
}

// Register binds view model types to their templates. It fails when a struct
// and its contract disagree, so the mismatch surfaces at startup.
func (e *Engine) Register(views ...View) error {
	if e == nil {
		return fmt.Errorf("engine is nil")
	}
	pending := make(map[string]viewBinding, len(views))
	for _, view := range views {
		name, err := templateNameForView(view)
		if err != nil {
			return err
		}
		doc, ok := e.templates[name]
		if !ok {
			return fmt.Errorf("view template %q is not registered", name)
		}
		if _, exists := e.bindings[name]; exists {
			return fmt.Errorf("view template %q is registered more than once", name)
		}
		if _, exists := pending[name]; exists {
			return fmt.Errorf("view template %q is registered more than once", name)
		}
		binding, err := bindView(view, doc)
		if err != nil {
			return fmt.Errorf("template %q: %w", name, err)
		}
		pending[name] = binding
	}
	for name, binding := range pending {
		e.bindings[name] = binding
	}
	return nil
}

// Names returns the registered template names in deterministic order.
func (e *Engine) Names() []string {
	if e == nil {
		return nil
	}
	names := make([]string, 0, len(e.templates))
	for name := range e.templates {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// LayoutSHA256 returns the derived layout fingerprint of one template.
func (e *Engine) LayoutSHA256(name string) (string, bool) {
	if e == nil {
		return "", false
	}
	doc, ok := e.templates[name]
	if !ok {
		return "", false
	}
	return doc.LayoutSHA256, true
}

// ActionIDs returns every action ID the templates declare, deduplicated and sorted.
func (e *Engine) ActionIDs() []string {
	if e == nil {
		return nil
	}
	seen := make(map[string]struct{})
	for _, doc := range e.templates {
		for _, action := range doc.Actions {
			seen[action.ID] = struct{}{}
		}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// CallbackIDs returns every modal callback ID the templates declare, deduplicated and sorted.
func (e *Engine) CallbackIDs() []string {
	if e == nil {
		return nil
	}
	seen := make(map[string]struct{})
	for _, doc := range e.templates {
		if doc.CallbackID != "" {
			seen[doc.CallbackID] = struct{}{}
		}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func templateNameForView(view View) (string, error) {
	if view == nil {
		return "", errors.New("view is nil")
	}
	viewValue := reflect.ValueOf(view)
	if viewValue.Kind() == reflect.Pointer && viewValue.IsNil() {
		return "", errors.New("view pointer is nil")
	}
	return view.Template(), nil
}

func inputTypeName(input InputType) string {
	return string(input)
}

func isZeroValue(value reflect.Value) bool {
	return value.IsZero()
}

func isTimeType(value reflect.Type) bool {
	return value == reflect.TypeOf(time.Time{})
}

func isPairSliceType(value reflect.Type) bool {
	return value.Kind() == reflect.Slice && value.Elem() == reflect.TypeOf(Pair{})
}

func fieldLabel(field reflect.StructField) (string, bool, error) {
	tag, exists := field.Tag.Lookup("bk")
	if !exists {
		return "", false, nil
	}
	if tag == "-" {
		return "", false, nil
	}
	parts := strings.Split(tag, ",")
	if parts[0] == "" {
		return "", false, fmt.Errorf("field %s has an empty bk name", field.Name)
	}
	if len(parts) > 2 || (len(parts) == 2 && parts[1] != "omitempty") {
		return "", false, fmt.Errorf("field %s has an invalid bk tag", field.Name)
	}
	return parts[0], true, nil
}
