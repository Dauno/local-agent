package slack

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/agentdef"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/usecase/agentbuilder"
	slackapi "github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
)

func TestBuilderModalPresenterRendersLLMParity(t *testing.T) {
	presenter := NewBuilderModalPresenterWithProviders([]BuilderProviderProfile{
		{Reference: "opencode/default", ProviderType: agentdef.ProviderTypeAgentCLI},
		{Reference: "openai/z", ProviderType: agentdef.ProviderTypeOpenAICompatible},
		{Reference: "openai/a", ProviderType: agentdef.ProviderTypeOpenAICompatible},
		{Reference: "codex/b", ProviderType: agentdef.ProviderTypeAgentCLI},
	})
	view := presenter.BuildViewForKind(domain.AgentKindLLM, map[string]string{
		"name":        "incident_analyst",
		"description": "Description",
		"instruction": "Instruction",
		"agent_type":  string(domain.AgentKindLLM),
		"model":       "openai/z",
	})

	assertBuilderModalChrome(t, view)
	if got := blockIDs(view.Blocks.BlockSet); !reflect.DeepEqual(got, []string{"name", "agent_type", "description", "instruction", "model"}) {
		t.Fatalf("LLM block IDs = %v", got)
	}
	if len(view.Blocks.BlockSet) != 6 {
		t.Fatalf("LLM block count = %d, want 6", len(view.Blocks.BlockSet))
	}
	section, ok := view.Blocks.BlockSet[0].(*slackapi.SectionBlock)
	if !ok || section.Text == nil || section.Text.Text != "Completa los campos para definir un nuevo agente." {
		t.Fatalf("LLM introductory section = %#v", view.Blocks.BlockSet[0])
	}

	name := builderInputBlock(t, view, "name")
	assertBuilderInput(t, name, "Nombre", "3-64 caracteres, solo minusculas, numeros, _ y -")
	nameElement := name.Element.(*slackapi.PlainTextInputBlockElement)
	if nameElement.Placeholder == nil || nameElement.Placeholder.Text != "incident_analyst" || nameElement.InitialValue != "incident_analyst" {
		t.Fatalf("name element = %#v", nameElement)
	}

	description := builderInputBlock(t, view, "description")
	assertBuilderInput(t, description, "Descripcion", "")
	descriptionElement := description.Element.(*slackapi.PlainTextInputBlockElement)
	if descriptionElement.MaxLength != agentdef.MaxDescriptionLength || descriptionElement.Placeholder == nil || descriptionElement.Placeholder.Text != "Breve descripcion del proposito del agente" {
		t.Fatalf("description element = %#v", descriptionElement)
	}

	instruction := builderInputBlock(t, view, "instruction")
	assertBuilderInput(t, instruction, "Instruccion", "")
	instructionElement := instruction.Element.(*slackapi.PlainTextInputBlockElement)
	if instructionElement.MaxLength != agentdef.MaxInstructionLength || !instructionElement.Multiline || instructionElement.Placeholder == nil || instructionElement.Placeholder.Text != "Instruccion completa del agente..." {
		t.Fatalf("instruction element = %#v", instructionElement)
	}

	typeBlock := builderInputBlock(t, view, "agent_type")
	if !typeBlock.DispatchAction {
		t.Fatal("agent_type input does not dispatch actions")
	}
	typeSelect := typeBlock.Element.(*slackapi.SelectBlockElement)
	assertStaticSelect(t, typeSelect, "agent_type", []string{"llm", "acp"}, "llm")

	model := builderInputBlock(t, view, "model").Element.(*slackapi.SelectBlockElement)
	assertStaticSelect(t, model, "model", []string{"openai/a", "openai/z"}, "openai/z")
}

func TestBuilderModalPresenterRendersExternalAgentParity(t *testing.T) {
	presenter := NewBuilderModalPresenterWithProviders([]BuilderProviderProfile{
		{Reference: "opencode/z", ProviderType: agentdef.ProviderTypeAgentCLI},
		{Reference: "openai/ignored", ProviderType: agentdef.ProviderTypeOpenAICompatible},
		{Reference: "opencode/a", ProviderType: agentdef.ProviderTypeAgentCLI},
		{Reference: "other/ignored", ProviderType: "unsupported"},
	})
	view := presenter.BuildViewForKind(domain.AgentKindAgentCLI, map[string]string{
		"name":            "incident_analyst",
		"description":     "Description",
		"instruction":     "Instruction",
		"agent_type":      string(domain.AgentKindAgentCLI),
		"model":           "opencode/z",
		"execution_mode":  domain.ExecutionModeDurableJob,
		"timeout_seconds": "86400",
	})

	assertBuilderModalChrome(t, view)
	if got := blockIDs(view.Blocks.BlockSet); !reflect.DeepEqual(got, []string{"name", "agent_type", "description", "instruction", "model", "execution_mode", "timeout_seconds"}) {
		t.Fatalf("ACP block IDs = %v", got)
	}
	if len(view.Blocks.BlockSet) != 8 {
		t.Fatalf("ACP block count = %d, want 8", len(view.Blocks.BlockSet))
	}

	model := builderInputBlock(t, view, "model").Element.(*slackapi.SelectBlockElement)
	assertStaticSelect(t, model, "model", []string{"opencode/a", "opencode/z"}, "opencode/z")

	modeBlock := builderInputBlock(t, view, "execution_mode")
	assertBuilderInput(t, modeBlock, "Ejecucion", "")
	mode := modeBlock.Element.(*slackapi.SelectBlockElement)
	assertStaticSelect(t, mode, "execution_mode", []string{domain.ExecutionModeForeground, domain.ExecutionModeDurableJob}, domain.ExecutionModeDurableJob)

	timeoutBlock := builderInputBlock(t, view, "timeout_seconds")
	assertBuilderInput(t, timeoutBlock, "Timeout (segundos)", "Maximo 86400")
	timeout := timeoutBlock.Element.(*slackapi.PlainTextInputBlockElement)
	if timeout.InitialValue != "86400" || timeout.MaxLength != len("86400") || timeout.Placeholder == nil || timeout.Placeholder.Text != "7200" {
		t.Fatalf("timeout element = %#v", timeout)
	}
}

func TestBuilderModalPresenterPreservesValuesAndPrivateMetadata(t *testing.T) {
	const metadata = `{"v":1,"actor_id":"U12345678","conversation_key":"slack:T12345678:dm:D12345678"}`
	presenter := NewBuilderModalPresenterWithProviders([]BuilderProviderProfile{
		{Reference: "openai/fast", ProviderType: agentdef.ProviderTypeOpenAICompatible},
		{Reference: "opencode/default", ProviderType: agentdef.ProviderTypeAgentCLI},
	})
	callback := slackapi.InteractionCallback{
		View: slackapi.View{
			PrivateMetadata: metadata,
			State:           &slackapi.ViewState{Values: map[string]map[string]slackapi.BlockAction{}},
		},
		ActionCallback: slackapi.ActionCallbacks{
			BlockActions: []*slackapi.BlockAction{{BlockID: "agent_type", ActionID: "agent_type", SelectedOption: slackapi.OptionBlockObject{Value: string(domain.AgentKindAgentCLI)}}},
		},
		BlockActionState: &slackapi.BlockActionStates{Values: map[string]map[string]slackapi.BlockAction{
			"name":            {"name": {BlockID: "name", ActionID: "name", Value: "preserved_name"}},
			"description":     {"description": {BlockID: "description", ActionID: "description", Value: "preserved description"}},
			"instruction":     {"instruction": {BlockID: "instruction", ActionID: "instruction", Value: "preserved instruction"}},
			"agent_type":      {"agent_type": {BlockID: "agent_type", ActionID: "agent_type", SelectedOption: slackapi.OptionBlockObject{Value: string(domain.AgentKindAgentCLI)}}},
			"model":           {"model": {BlockID: "model", ActionID: "model", SelectedOption: slackapi.OptionBlockObject{Value: "opencode/default"}}},
			"execution_mode":  {"execution_mode": {BlockID: "execution_mode", ActionID: "execution_mode", SelectedOption: slackapi.OptionBlockObject{Value: domain.ExecutionModeDurableJob}}},
			"timeout_seconds": {"timeout_seconds": {BlockID: "timeout_seconds", ActionID: "timeout_seconds", Value: "1234"}},
		}},
	}

	view := presenter.BuildViewForCallback(callback)
	if view.PrivateMetadata != metadata {
		t.Fatalf("private metadata = %q, want byte-identical metadata", view.PrivateMetadata)
	}
	if got := blockIDs(view.Blocks.BlockSet); !reflect.DeepEqual(got, []string{"name", "agent_type", "description", "instruction", "model", "execution_mode", "timeout_seconds"}) {
		t.Fatalf("updated block IDs = %v", got)
	}
	if got := builderInputBlock(t, view, "name").Element.(*slackapi.PlainTextInputBlockElement).InitialValue; got != "preserved_name" {
		t.Fatalf("preserved name = %q", got)
	}
	if got := builderInputBlock(t, view, "description").Element.(*slackapi.PlainTextInputBlockElement).InitialValue; got != "preserved description" {
		t.Fatalf("preserved description = %q", got)
	}
	if got := builderInputBlock(t, view, "instruction").Element.(*slackapi.PlainTextInputBlockElement).InitialValue; got != "preserved instruction" {
		t.Fatalf("preserved instruction = %q", got)
	}
	if got := builderInputBlock(t, view, "model").Element.(*slackapi.SelectBlockElement).InitialOption.Value; got != "opencode/default" {
		t.Fatalf("preserved model = %q", got)
	}
	if got := builderInputBlock(t, view, "execution_mode").Element.(*slackapi.SelectBlockElement).InitialOption.Value; got != domain.ExecutionModeDurableJob {
		t.Fatalf("preserved execution mode = %q", got)
	}
	if got := builderInputBlock(t, view, "timeout_seconds").Element.(*slackapi.PlainTextInputBlockElement).InitialValue; got != "1234" {
		t.Fatalf("preserved timeout = %q", got)
	}
}

func TestBuilderModalPresenterRejectsEmptyProviderCatalog(t *testing.T) {
	presenter := NewBuilderModalPresenterWithProviders(nil)
	if err := presenter.InitializationError(); err == nil {
		t.Fatal("empty provider catalog was accepted")
	}
	if _, err := presenter.BuildViewResult(); err == nil {
		t.Fatal("empty provider catalog produced a modal")
	}
}

func TestBuilderModalInvalidSubmissionPathRemainsErrors(t *testing.T) {
	handler := NewBuilderSubmissionHandler(nil, agentbuilder.New(), nil, nil)
	response := handler.HandleSubmission(context.Background(), builderSubmissionCallback("unsupported", "openai/fast"))
	if response == nil || response.ResponseAction != slackapi.RAErrors {
		t.Fatalf("response = %#v, want validation errors", response)
	}
	if _, ok := response.Errors["agent_type"]; !ok {
		t.Fatalf("validation errors = %#v, want agent_type", response.Errors)
	}
}

func TestBuilderModalUpdateRejectsMissingViewIdentity(t *testing.T) {
	tests := []struct {
		name string
		id   string
		hash string
	}{
		{name: "missing view ID", hash: "hash-1"},
		{name: "missing view hash", id: "V123"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &fakeViewSocketClient{fakeSocketClient: newFakeSocketClient(), updates: make(chan viewUpdate, 1)}
			listener := newListener(client, NewRouter(testBot), nil).WithBuilderPresenter(NewBuilderModalPresenterWithProviders(testBuilderProviderProfiles()))
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- listener.Run(ctx, func(context.Context, domain.Invocation) {}) }()

			client.events <- socketmode.Event{
				Type: socketmode.EventTypeInteractive,
				Data: slackapi.InteractionCallback{
					Type:           slackapi.InteractionTypeBlockActions,
					View:           slackapi.View{CallbackID: builderSubmitCallbackID, ID: test.id, Hash: test.hash},
					ActionCallback: slackapi.ActionCallbacks{BlockActions: []*slackapi.BlockAction{{ActionID: "agent_type", Value: "acp"}}},
				},
				Request: &socketmode.Request{Type: socketmode.RequestTypeInteractive, EnvelopeID: "builder-action-" + strings.ReplaceAll(test.name, " ", "-")},
			}

			deadline := time.After(time.Second)
			for !client.wasAcked("builder-action-" + strings.ReplaceAll(test.name, " ", "-")) {
				select {
				case <-deadline:
					t.Fatal("builder action was not acknowledged")
				default:
					time.Sleep(time.Millisecond)
				}
			}
			select {
			case update := <-client.updates:
				t.Fatalf("unexpected view update = %#v", update)
			case <-time.After(25 * time.Millisecond):
			}

			cancel()
			if err := <-done; err != nil {
				t.Fatalf("Run() shutdown error = %v", err)
			}
		})
	}
}

func testBuilderProviderProfiles() []BuilderProviderProfile {
	return []BuilderProviderProfile{
		{Reference: "openai/fast", ProviderType: agentdef.ProviderTypeOpenAICompatible},
		{Reference: "opencode/default", ProviderType: agentdef.ProviderTypeAgentCLI},
	}
}

func TestBuilderModalUpdateRejectsUnavailableKindBeforeSlack(t *testing.T) {
	client := &fakeViewSocketClient{fakeSocketClient: newFakeSocketClient(), updates: make(chan viewUpdate, 1)}
	presenter := NewBuilderModalPresenterWithProviders([]BuilderProviderProfile{{
		Reference: "openai/fast", ProviderType: agentdef.ProviderTypeOpenAICompatible,
	}})
	listener := newListener(client, NewRouter(testBot), nil).WithBuilderPresenter(presenter)
	callback := slackapi.InteractionCallback{
		Type: slackapi.InteractionTypeBlockActions,
		View: slackapi.View{CallbackID: builderSubmitCallbackID, ID: "V123", Hash: "hash-1"},
		ActionCallback: slackapi.ActionCallbacks{BlockActions: []*slackapi.BlockAction{{
			BlockID: "agent_type", ActionID: "agent_type", Value: string(domain.AgentKindAgentCLI),
		}}},
	}

	if err := listener.handleBuilderTypeAction(t.Context(), callback); err == nil {
		t.Fatal("builder update without ACP profiles returned nil")
	}
	select {
	case update := <-client.updates:
		t.Fatalf("partial builder view reached Slack: %#v", update)
	default:
	}
}

func TestBuilderModalTemplateCopyAndOrderChangesDriveRenderer(t *testing.T) {
	files := embeddedTemplateFiles(t)
	path := "templates/builder_modal.json"
	var document map[string]any
	if err := json.Unmarshal(files[path], &document); err != nil {
		t.Fatalf("decode builder template: %v", err)
	}
	payload := document["payload"].(map[string]any)
	blocks := payload["blocks"].([]any)
	section := blocks[0].(map[string]any)
	sectionText := section["text"].(map[string]any)
	sectionText["text"] = "Plantilla personalizada."
	blocks[0], blocks[1] = blocks[1], blocks[0]
	payload["blocks"] = blocks
	files[path], _ = json.Marshal(document)

	catalog, err := LoadTemplateCatalogFromFS(templateMapFS(files))
	if err != nil {
		t.Fatalf("load edited catalog: %v", err)
	}
	renderer, err := NewTemplateRenderer(catalog)
	if err != nil {
		t.Fatalf("new renderer: %v", err)
	}
	view, err := renderer.CompileModal("builder_modal", TemplateContext{
		Kind:     domain.AgentKindLLM,
		Profiles: []BuilderProviderProfile{{Reference: "openai/fast", ProviderType: agentdef.ProviderTypeOpenAICompatible}},
		Values:   map[string]string{"agent_type": "llm", "model": "openai/fast"},
	})
	if err != nil {
		t.Fatalf("compile edited builder template: %v", err)
	}
	if got, ok := view.Blocks.BlockSet[0].(*slackapi.InputBlock); !ok || got.BlockID != "name" {
		t.Fatalf("edited first block = %#v, want name input", view.Blocks.BlockSet[0])
	}
	if got, ok := view.Blocks.BlockSet[1].(*slackapi.SectionBlock); !ok || got.Text == nil || got.Text.Text != "Plantilla personalizada." {
		t.Fatalf("edited second block = %#v", view.Blocks.BlockSet[1])
	}
}

func TestBuilderModalPresenterIsDeterministic(t *testing.T) {
	profiles := []BuilderProviderProfile{
		{Reference: "opencode/z", ProviderType: agentdef.ProviderTypeAgentCLI},
		{Reference: "openai/z", ProviderType: agentdef.ProviderTypeOpenAICompatible},
		{Reference: "opencode/a", ProviderType: agentdef.ProviderTypeAgentCLI},
		{Reference: "openai/a", ProviderType: agentdef.ProviderTypeOpenAICompatible},
	}
	first := NewBuilderModalPresenterWithProviders(profiles)
	second := NewBuilderModalPresenterWithProviders([]BuilderProviderProfile{profiles[3], profiles[1], profiles[2], profiles[0]})
	for _, kind := range []domain.AgentKind{domain.AgentKindLLM, domain.AgentKindAgentCLI} {
		values := map[string]string{"agent_type": string(kind), "model": "openai/a"}
		if kind == domain.AgentKindAgentCLI {
			values["model"] = "opencode/a"
		}
		firstJSON, err := json.Marshal(first.BuildViewForKind(kind, values))
		if err != nil {
			t.Fatal(err)
		}
		secondJSON, err := json.Marshal(second.BuildViewForKind(kind, values))
		if err != nil {
			t.Fatal(err)
		}
		if string(firstJSON) != string(secondJSON) {
			t.Fatalf("%s renders are not deterministic\nfirst: %s\nsecond: %s", kind, firstJSON, secondJSON)
		}
	}
}

func assertBuilderModalChrome(t *testing.T, view slackapi.ModalViewRequest) {
	t.Helper()
	if view.Type != slackapi.VTModal || view.Title == nil || view.Title.Text != "Crear nuevo agente" || view.Submit == nil || view.Submit.Text != "Previsualizar" || view.Close == nil || view.Close.Text != "Cancelar" || view.CallbackID != builderSubmitCallbackID {
		t.Fatalf("modal chrome = %#v", view)
	}
}

func assertBuilderInput(t *testing.T, block *slackapi.InputBlock, label, hint string) {
	t.Helper()
	if block.Label == nil || block.Label.Text != label || block.Optional || (block.Hint == nil) != (hint == "") || (block.Hint != nil && block.Hint.Text != hint) {
		t.Fatalf("input %q = %#v", block.BlockID, block)
	}
}

func assertStaticSelect(t *testing.T, selectElement *slackapi.SelectBlockElement, actionID string, wantOptions []string, wantInitial string) {
	t.Helper()
	if selectElement.Type != slackapi.OptTypeStatic || selectElement.ActionID != actionID {
		t.Fatalf("select = %#v", selectElement)
	}
	if got := optionValues(selectElement.Options); !reflect.DeepEqual(got, wantOptions) {
		t.Fatalf("%s options = %v, want %v", actionID, got, wantOptions)
	}
	if selectElement.InitialOption == nil || selectElement.InitialOption.Value != wantInitial {
		t.Fatalf("%s initial option = %#v, want %q", actionID, selectElement.InitialOption, wantInitial)
	}
}

func builderInputBlock(t *testing.T, view slackapi.ModalViewRequest, blockID string) *slackapi.InputBlock {
	t.Helper()
	for _, block := range view.Blocks.BlockSet {
		input, ok := block.(*slackapi.InputBlock)
		if ok && input.BlockID == blockID {
			return input
		}
	}
	t.Fatalf("input block %q not found", blockID)
	return nil
}
