package domain

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// ContentRole is the provider-neutral role used by the model-facing history
// projection. It intentionally does not expose an SDK role type.
type ContentRole string

const (
	ContentRoleUser  ContentRole = "user"
	ContentRoleModel ContentRole = "model"
)

const ConfirmationFunctionName = "adk_request_confirmation"

const MaxPersistedSummaryChars = 8000

type FunctionCall struct {
	ID   string         `json:"id,omitempty"`
	Name string         `json:"name,omitempty"`
	Args map[string]any `json:"args,omitempty"`
}

type FunctionResponse struct {
	ID           string         `json:"id,omitempty"`
	Name         string         `json:"name,omitempty"`
	Response     map[string]any `json:"response,omitempty"`
	WillContinue *bool          `json:"will_continue,omitempty"`
}

// ContentPart preserves the structured protocol parts needed by ADK. Text is
// measured as text; structured values are measured as canonical JSON.
type ContentPart struct {
	Text             string
	FunctionCall     *FunctionCall
	FunctionResponse *FunctionResponse
	StructuredJSON   json.RawMessage
}

type Content struct {
	Role  ContentRole
	Parts []ContentPart
}

func (c Content) HasPlainText() bool {
	if c.Role != ContentRoleUser || len(c.Parts) == 0 {
		return false
	}
	for _, part := range c.Parts {
		if part.FunctionCall != nil || part.FunctionResponse != nil || len(part.StructuredJSON) != 0 || part.Text == "" {
			return false
		}
	}
	return true
}

func (c Content) HasStructuredPart() bool {
	for _, part := range c.Parts {
		if part.FunctionCall != nil || part.FunctionResponse != nil || len(part.StructuredJSON) != 0 {
			return true
		}
	}
	return false
}

func (c Content) Clone() Content {
	clone := Content{Role: c.Role, Parts: make([]ContentPart, len(c.Parts))}
	for i, part := range c.Parts {
		clone.Parts[i] = ContentPart{Text: part.Text, StructuredJSON: slices.Clone(part.StructuredJSON)}
		if part.FunctionCall != nil {
			clone.Parts[i].FunctionCall = &FunctionCall{ID: part.FunctionCall.ID, Name: part.FunctionCall.Name, Args: cloneMap(part.FunctionCall.Args)}
		}
		if part.FunctionResponse != nil {
			clone.Parts[i].FunctionResponse = &FunctionResponse{ID: part.FunctionResponse.ID, Name: part.FunctionResponse.Name, Response: cloneMap(part.FunctionResponse.Response)}
			if part.FunctionResponse.WillContinue != nil {
				value := *part.FunctionResponse.WillContinue
				clone.Parts[i].FunctionResponse.WillContinue = &value
			}
		}
	}
	return clone
}

func cloneMap(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	return mapsClone(input)
}

func mapsClone(input map[string]any) map[string]any {
	output := make(map[string]any, len(input))
	for key, value := range input {
		output[key] = cloneValue(value)
	}
	return output
}

func cloneValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return mapsClone(typed)
	case []any:
		output := make([]any, len(typed))
		for index, item := range typed {
			output[index] = cloneValue(item)
		}
		return output
	default:
		// Keep scalar types, especially integers, exactly as supplied by ADK.
		return value
	}
}

// CanonicalJSON returns deterministic JSON for structured protocol data.
// encoding/json sorts string map keys, which is sufficient for this contract.
func CanonicalJSON(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var normalized any
	if err := json.Unmarshal(encoded, &normalized); err != nil {
		return nil, err
	}
	return json.Marshal(normalized)
}

func (p ContentPart) Cost() (int, error) {
	if p.FunctionCall != nil {
		encoded, err := CanonicalJSON(struct {
			FunctionCall *FunctionCall `json:"function_call"`
		}{p.FunctionCall})
		return utf8.RuneCount(encoded), err
	}
	if p.FunctionResponse != nil {
		encoded, err := CanonicalJSON(struct {
			FunctionResponse *FunctionResponse `json:"function_response"`
		}{p.FunctionResponse})
		return utf8.RuneCount(encoded), err
	}
	if len(p.StructuredJSON) > 0 {
		var value any
		if err := json.Unmarshal(p.StructuredJSON, &value); err != nil {
			return 0, err
		}
		encoded, err := CanonicalJSON(value)
		return utf8.RuneCount(encoded), err
	}
	return utf8.RuneCountInString(p.Text), nil
}

func ContentCost(contents []Content) (int, error) {
	total := 0
	for _, content := range contents {
		for _, part := range content.Parts {
			cost, err := part.Cost()
			if err != nil {
				return 0, err
			}
			total += cost
		}
	}
	return total, nil
}

type ConversationTurn struct {
	Ordinal             int64
	Contents            []Content
	CharCount           int
	Closed              bool
	HasOpenConfirmation bool
	HasOpenInvocation   bool
}

func (t ConversationTurn) Clone() ConversationTurn {
	clone := ConversationTurn{Ordinal: t.Ordinal, CharCount: t.CharCount, Closed: t.Closed, HasOpenConfirmation: t.HasOpenConfirmation, HasOpenInvocation: t.HasOpenInvocation, Contents: make([]Content, len(t.Contents))}
	for i, content := range t.Contents {
		clone.Contents[i] = content.Clone()
	}
	return clone
}

// TurnClassificationOptions carries invocation state that is known outside the
// content ledger, such as a confirmation that is pending in a delivery store.
// The variadic form keeps callers that do not have external state simple.
type TurnClassificationOptions struct {
	OpenInvocationIDs map[string]struct{}
}

// ClassifyConversationTurns groups plain user inputs and validates the
// function-call protocol. The active suffix starts at the oldest unmatched or
// externally-open invocation, not necessarily at the last plain user input.
func ClassifyConversationTurns(contents []Content, options ...TurnClassificationOptions) ([]ConversationTurn, int, error) {
	if len(contents) == 0 {
		return nil, 0, nil
	}
	var opts TurnClassificationOptions
	if len(options) > 0 {
		opts = options[0]
	}
	turns := make([]ConversationTurn, 0)
	start := -1
	for index, content := range contents {
		if content.Role != ContentRoleUser && content.Role != ContentRoleModel {
			return nil, 0, fmt.Errorf("unsupported content role %q", content.Role)
		}
		if content.HasPlainText() {
			if start >= 0 {
				turns = append(turns, newTurn(int64(len(turns)+1), contents[start:index], true))
			}
			start = index
		}
	}
	if start < 0 {
		return nil, 0, errors.New("history has no plain user input")
	}
	turns = append(turns, newTurn(int64(len(turns)+1), contents[start:], false))
	if err := ValidateContentProtocol(contents, false); err != nil {
		return nil, 0, err
	}

	callIndexes := make(map[string]int)
	responded := make(map[string]bool)
	for index, content := range contents {
		for _, part := range content.Parts {
			switch {
			case part.FunctionCall != nil:
				callIndexes[part.FunctionCall.ID] = index
			case part.FunctionResponse != nil:
				responded[part.FunctionResponse.ID] = true
			}
		}
	}
	activeStart := start
	openIDs := make(map[string]struct{}, len(opts.OpenInvocationIDs))
	for id := range opts.OpenInvocationIDs {
		openIDs[id] = struct{}{}
	}
	for id, index := range callIndexes {
		if responded[id] {
			continue
		}
		openIDs[id] = struct{}{}
		if invocationStart := invocationStartAt(contents, index); invocationStart < activeStart {
			activeStart = invocationStart
		}
	}
	for id := range opts.OpenInvocationIDs {
		if index, ok := callIndexes[id]; ok {
			if invocationStart := invocationStartAt(contents, index); invocationStart < activeStart {
				activeStart = invocationStart
			}
		}
	}

	contentStart := 0
	for index := range turns {
		contentEnd := contentStart + len(turns[index].Contents)
		if activeStart >= contentStart && activeStart < contentEnd {
			for openID := range openIDs {
				for _, content := range turns[index].Contents {
					for _, part := range content.Parts {
						if part.FunctionCall != nil && part.FunctionCall.ID == openID {
							turns[index].HasOpenInvocation = true
							if part.FunctionCall.Name == ConfirmationFunctionName {
								turns[index].HasOpenConfirmation = true
							}
						}
					}
				}
			}
			turns[index].Closed = false
			break
		}
		contentStart = contentEnd
	}
	return turns, activeStart, nil
}

func invocationStartAt(contents []Content, callIndex int) int {
	for index := callIndex; index >= 0; index-- {
		if contents[index].HasPlainText() {
			return index
		}
	}
	return callIndex
}

func newTurn(ordinal int64, contents []Content, closed bool) ConversationTurn {
	turn := ConversationTurn{Ordinal: ordinal, Closed: closed, Contents: make([]Content, len(contents))}
	for i, content := range contents {
		turn.Contents[i] = content.Clone()
	}
	turn.CharCount, _ = ContentCost(turn.Contents)
	return turn
}

// ValidateContentProtocol validates function-call ordering. ADK confirmation
// flows may emit a placeholder response before the confirmation wrapper and a
// terminal response after the decision.
func ValidateContentProtocol(contents []Content, requireComplete bool) error {
	calls := make(map[string]string)
	responses := make(map[string]int)
	confirmationWrappers := make(map[string]string)
	for index, content := range contents {
		for _, part := range content.Parts {
			switch {
			case part.FunctionCall != nil:
				call := part.FunctionCall
				if strings.TrimSpace(call.ID) == "" || strings.TrimSpace(call.Name) == "" {
					return fmt.Errorf("invalid function call at content %d", index)
				}
				if _, exists := calls[call.ID]; exists {
					return fmt.Errorf("duplicate function call %q", call.ID)
				}
				calls[call.ID] = call.Name
				if call.Name == ConfirmationFunctionName {
					originalID, originalName, ok := confirmationOriginalCall(call)
					if !ok || calls[originalID] != originalName || responses[originalID] != 1 {
						return fmt.Errorf("invalid confirmation call %q", call.ID)
					}
					if _, exists := confirmationWrappers[originalID]; exists {
						return fmt.Errorf("duplicate confirmation for function call %q", originalID)
					}
					confirmationWrappers[originalID] = call.ID
				}
			case part.FunctionResponse != nil:
				response := part.FunctionResponse
				if strings.TrimSpace(response.ID) == "" || strings.TrimSpace(response.Name) == "" {
					return fmt.Errorf("invalid function response at content %d", index)
				}
				name, exists := calls[response.ID]
				if !exists || name != response.Name {
					return fmt.Errorf("function response %q has no matching call", response.ID)
				}
				if responses[response.ID] > 0 {
					wrapperID, confirmed := confirmationWrappers[response.ID]
					if !confirmed || responses[response.ID] != 1 || responses[wrapperID] != 1 {
						return fmt.Errorf("function response %q has no matching call", response.ID)
					}
				}
				responses[response.ID]++
			}
		}
	}
	if requireComplete {
		for id := range calls {
			if responses[id] == 0 {
				return fmt.Errorf("function call %q has no response in completed turn", id)
			}
		}
		for originalID := range confirmationWrappers {
			if responses[originalID] != 2 {
				return fmt.Errorf("function call %q has no terminal response after confirmation", originalID)
			}
		}
	}
	return nil
}

func confirmationOriginalCall(call *FunctionCall) (string, string, bool) {
	if call == nil || call.Name != ConfirmationFunctionName || call.Args == nil {
		return "", "", false
	}
	raw, exists := call.Args["originalFunctionCall"]
	if !exists {
		return "", "", false
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return "", "", false
	}
	var original struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(data, &original); err != nil || strings.TrimSpace(original.ID) == "" || strings.TrimSpace(original.Name) == "" {
		return "", "", false
	}
	return original.ID, original.Name, true
}

type CompactionRequest struct {
	Contents                      []Content
	ActiveSuffixStart             int
	OpenInvocationIDs             map[string]struct{}
	ExistingSummary               string
	ExistingSummaryCoveredOrdinal int64
	MaxHistoryChars               int
	RecentTurns                   int
	SummaryMaxChars               int
	ConversationKey               string
	SessionRevision               int64
	SystemInstructionChars        int
	ToolChars                     int
}

type CompactionDiagnostics struct {
	HistoryContentsBefore  int
	HistoryContentsAfter   int
	HistoryCharsBefore     int
	HistoryCharsAfter      int
	RecentTurnsRetained    int
	SummaryPresent         bool
	SummaryCoveredOrdinal  int64
	CompactionApplied      bool
	Reason                 string
	ConversationKey        string
	SessionRevision        int64
	ActiveSuffixContents   int
	SystemInstructionChars int
	ToolChars              int
}

type CompactionResult struct {
	Contents    []Content
	Diagnostics CompactionDiagnostics
}

var ErrActiveContextTooLarge = errors.New("active_context_too_large")

var summaryXMLLike = regexp.MustCompile(`<[[:alnum:]_/?!][^>]*>`)

type ActiveContextTooLargeError struct {
	Chars  int
	Budget int
}

func (e *ActiveContextTooLargeError) Error() string {
	return fmt.Sprintf("%s: active suffix is %d code points, budget is %d", ErrActiveContextTooLarge, e.Chars, e.Budget)
}

func (e *ActiveContextTooLargeError) Unwrap() error { return ErrActiveContextTooLarge }

// SanitizeConversationSummary accepts only bounded reference prose. Summary
// text is never trusted as an instruction, authorization, or confirmation.
func SanitizeConversationSummary(text string, maxChars int) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", errors.New("conversation summary must not be empty")
	}
	if maxChars <= 0 || utf8.RuneCountInString(text) > maxChars {
		return "", fmt.Errorf("conversation summary exceeds %d Unicode code points", maxChars)
	}
	for _, r := range text {
		if r == '\n' || r == '\t' {
			continue
		}
		if r < 0x20 || r == 0x7f {
			return "", errors.New("conversation summary contains control characters")
		}
	}
	lower := strings.ToLower(text)
	for _, fragment := range []string{
		"ignore previous", "ignore all", "api key", "apikey", "password", "secret", "credential", "bearer ",
		"private key", "approved", "approval", "authorized", "authorised", "permission granted", "policy",
		"aprobado", "aprobada", "aprobación", "autorizado", "autorizada", "autorización", "permiso concedido", "política",
	} {
		if strings.Contains(lower, fragment) {
			return "", errors.New("conversation summary contains prohibited instruction, credential, policy, or approval content")
		}
	}
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if summaryXMLLike.MatchString(line) || strings.ContainsAny(line, "<>") {
			return "", errors.New("conversation summary contains XML-like delimiters")
		}
		if !hasSummaryAttribution(strings.ToLower(line)) {
			return "", errors.New("conversation summary must use attributed declarative statements")
		}
		for _, word := range strings.Fields(strings.ToLower(line)) {
			word = strings.Trim(word, "`*_:-.,;()[]{}")
			switch word {
			case "run", "execute", "delete", "remove", "create", "approve", "allow", "deny", "reveal", "use", "call", "open", "close", "set", "change", "send",
				"ejecuta", "ejecutar", "borra", "borrar", "elimina", "eliminar", "crea", "crear", "aprueba", "aprobar", "permite", "permitir", "revela", "revelar", "usa", "usar", "llama", "llamar", "abre", "abrir", "cierra", "cerrar", "cambia", "cambiar", "envía", "enviar":
				return "", errors.New("conversation summary contains an imperative instruction")
			}
		}
	}
	return text, nil
}

func hasSummaryAttribution(line string) bool {
	for _, prefix := range []string{
		"user ", "the user ", "assistant ", "the assistant ", "model ", "the model ", "system ", "the system ",
		"usuario ", "el usuario ", "asistente ", "el asistente ", "modelo ", "el modelo ", "sistema ", "el sistema ",
		"la persona usuaria ", "用户", "助手",
	} {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

// ConversationSummarySourceDigest binds a summary revision to all source
// material it represents, including the previous accumulated summary.
func ConversationSummarySourceDigest(previous string, turns []ConversationTurn) string {
	data, _ := json.Marshal(struct {
		Previous string             `json:"previous"`
		Turns    []ConversationTurn `json:"turns"`
	}{Previous: previous, Turns: turns})
	digest := sha256.Sum256(data)
	return fmt.Sprintf("%x", digest[:])
}
