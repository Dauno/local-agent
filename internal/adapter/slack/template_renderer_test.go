package slack

import (
	"encoding/json"
	"strings"
	"testing"

	slackapi "github.com/slack-go/slack"

	"github.com/Dauno/slack-local-agent/internal/agentdef"
	"github.com/Dauno/slack-local-agent/internal/domain"
)

func TestBuilderHydrationTokensConditionsAndStableOptions(t *testing.T) {
	renderer := mustEmbeddedRenderer(t)
	profiles := []BuilderProviderProfile{
		{Reference: "openai/z", ProviderType: agentdef.ProviderTypeOpenAICompatible},
		{Reference: "agentcli/z", ProviderType: agentdef.ProviderTypeAgentCLI},
		{Reference: "openai/a", ProviderType: agentdef.ProviderTypeOpenAICompatible},
		{Reference: "agentcli/a", ProviderType: agentdef.ProviderTypeAgentCLI},
		{Reference: "codex/b", ProviderType: agentdef.ProviderTypeAgentCLI},
		{Reference: "other/ignored", ProviderType: "unsupported"},
	}
	llm, err := renderer.CompileModal("builder_modal", TemplateContext{
		Kind:     domain.AgentKindLLM,
		Profiles: profiles,
		Values: map[string]string{
			"name": "incident_analyst", "description": "desc", "instruction": "instruction",
			"agent_type": "llm", "model": "openai/z",
		},
	})
	if err != nil {
		t.Fatalf("compile LLM modal: %v", err)
	}
	if len(llm.Blocks.BlockSet) != 6 {
		t.Fatalf("LLM block count = %d, want 6", len(llm.Blocks.BlockSet))
	}
	if got := blockIDs(llm.Blocks.BlockSet); strings.Join(got, ",") != "name,agent_type,description,instruction,model" {
		t.Fatalf("LLM block IDs = %v", got)
	}
	model := llm.Blocks.BlockSet[5].(*slackapi.InputBlock).Element.(*slackapi.SelectBlockElement)
	if got := optionValues(model.Options); strings.Join(got, ",") != "openai/a,openai/z" {
		t.Fatalf("LLM model options = %v", got)
	}
	if model.InitialOption == nil || model.InitialOption.Value != "openai/z" {
		t.Fatalf("LLM initial model = %#v", model.InitialOption)
	}
	acp, err := renderer.CompileModal("builder_modal", TemplateContext{
		Kind:     domain.AgentKindAgentCLI,
		Profiles: profiles,
		Values: map[string]string{
			"name": "incident_analyst", "description": "desc", "instruction": "instruction",
			"agent_type": "agent_cli", "model": "agentcli/a", "execution_mode": domain.ExecutionModeDurableJob,
			"timeout_seconds": "",
		},
	})
	if err != nil {
		t.Fatalf("compile ACP modal: %v", err)
	}
	if len(acp.Blocks.BlockSet) != 8 {
		t.Fatalf("ACP block count = %d, want 8", len(acp.Blocks.BlockSet))
	}
	if got := blockIDs(acp.Blocks.BlockSet); strings.Join(got, ",") != "name,agent_type,description,instruction,model,execution_mode,timeout_seconds" {
		t.Fatalf("ACP block IDs = %v", got)
	}
	externalAgentModel := acp.Blocks.BlockSet[5].(*slackapi.InputBlock).Element.(*slackapi.SelectBlockElement)
	if got := optionValues(externalAgentModel.Options); strings.Join(got, ",") != "agentcli/a,agentcli/z,codex/b" {
		t.Fatalf("ACP model options = %v", got)
	}
	timeout := acp.Blocks.BlockSet[7].(*slackapi.InputBlock).Element.(*slackapi.PlainTextInputBlockElement)
	if timeout.InitialValue != "7200" {
		t.Fatalf("ACP default timeout = %q, want 7200", timeout.InitialValue)
	}

	first, err := json.Marshal(acp)
	if err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(mustCompileModal(t, renderer, "builder_modal", TemplateContext{
		Kind:     domain.AgentKindAgentCLI,
		Profiles: profiles,
		Values: map[string]string{
			"name": "incident_analyst", "description": "desc", "instruction": "instruction",
			"agent_type": "agent_cli", "model": "agentcli/a", "execution_mode": domain.ExecutionModeDurableJob,
			"timeout_seconds": "",
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("identical render contexts produced different payloads\nfirst: %s\nsecond: %s", first, second)
	}
}

func TestTemplateRendererRejectsClosedLanguageViolations(t *testing.T) {
	tests := []struct {
		name string
		edit func(map[string][]byte)
	}{
		{
			name: "partial token",
			edit: func(files map[string][]byte) {
				replaceBuilder(files, `"Completa los campos para definir un nuevo agente."`, `"Agent {{value.name}} is ready"`)
			},
		},
		{
			name: "unknown placeholder",
			edit: func(files map[string][]byte) { replaceBuilder(files, `"{{value.name}}"`, `"{{value.not_registered}}"`) },
		},
		{
			name: "expression",
			edit: func(files map[string][]byte) { replaceBuilder(files, `"{{value.name}}"`, `"{{value.name | upper}}"`) },
		},
		{
			name: "script",
			edit: func(files map[string][]byte) {
				replaceMessage(files, "confirmation_message", `"{{value.summary}}"`, `"javascript:alert(1)"`)
			},
		},
		{
			name: "loop",
			edit: func(files map[string][]byte) {
				replaceMessage(files, "confirmation_message", `"{{value.summary}}"`, `"{{#each options.model}}"`)
			},
		},
		{
			name: "unknown condition",
			edit: func(files map[string][]byte) { replaceBuilder(files, `"$if": "is_external_agent"`, `"$if": "is_llm"`) },
		},
		{
			name: "dynamic ID",
			edit: func(files map[string][]byte) {
				replaceBuilder(files, `"action_id": "name"`, `"action_id": "{{value.name}}"`)
			},
		},
		{
			name: "duplicate block ID",
			edit: func(files map[string][]byte) {
				replaceBuilder(files, `"block_id": "description"`, `"block_id": "name"`)
			},
		},
		{
			name: "unregistered action ID",
			edit: func(files map[string][]byte) {
				replaceBuilder(files, `"action_id": "model"`, `"action_id": "not_registered"`)
			},
		},
		{
			name: "scalar in collection position",
			edit: func(files map[string][]byte) {
				replaceBuilder(files, `"options": "{{options.model}}"`, `"options": "{{value.model}}"`)
			},
		},
		{
			name: "collection in scalar position",
			edit: func(files map[string][]byte) {
				replaceMessage(files, "confirmation_message", `"{{value.summary}}"`, `"{{options.model}}"`)
			},
		},
		{
			name: "unknown onboarding scalar",
			edit: func(files map[string][]byte) {
				replaceMessage(files, "onboarding_message", `"{{value.builder_context}}"`, `"{{value.missing}}"`)
			},
		},
		{
			name: "modal title limit",
			edit: func(files map[string][]byte) {
				replaceBuilder(files, `"Crear nuevo agente"`, strings.Repeat("x", maxRendererModalTitleLength+1))
			},
		},
		{
			name: "message fallback limit",
			edit: func(files map[string][]byte) {
				replaceMessage(files, "confirmation_message", `"{{value.fallback_text}}"`, strings.Repeat("x", maxFallbackText+1))
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			files := embeddedTemplateFiles(t)
			test.edit(files)
			if _, err := LoadTemplateCatalogFromFS(templateMapFS(files)); err == nil {
				t.Fatal("invalid template was accepted")
			}
		})
	}
}

func TestTemplateRendererRejectsMissingRequiredMessageToken(t *testing.T) {
	renderer := mustEmbeddedRenderer(t)
	_, _, err := renderer.CompileMessageWithFallback("confirmation_message", TemplateContext{Values: map[string]string{
		"subtitle":        "call-1",
		"wrapper_call_id": "wrapper-1",
		"fallback_text":   "Confirmation required",
	}})
	if err == nil {
		t.Fatal("message with missing summary token was accepted")
	}
}

func TestTemplateRendererRejectsMissingBuilderToken(t *testing.T) {
	renderer := mustEmbeddedRenderer(t)
	_, err := renderer.CompileModal("builder_modal", TemplateContext{
		Kind:     domain.AgentKindLLM,
		Profiles: []BuilderProviderProfile{{Reference: "openai/fast", ProviderType: agentdef.ProviderTypeOpenAICompatible}},
		Values: map[string]string{
			"name": "", "description": "", "instruction": "", "agent_type": "llm",
		},
	})
	if err == nil {
		t.Fatal("builder modal with missing model token was accepted")
	}
}

func TestTemplateRendererRejectsSurfaceAndMessageBlockViolations(t *testing.T) {
	files := embeddedTemplateFiles(t)
	message := string(files["templates/confirmation_message.json"])
	message = strings.Replace(message, `"type": "actions",`, `"type": "input",`, 1)
	message = strings.Replace(
		message,
		`"block_id": "confirmation_buttons",`,
		`"block_id": "confirmation_buttons", "label": {"type": "plain_text", "text": "Nombre", "emoji": false}, "element": {"type": "plain_text_input", "action_id": "name"},`,
		1,
	)
	files["templates/confirmation_message.json"] = []byte(message)
	if _, err := LoadTemplateCatalogFromFS(templateMapFS(files)); err == nil {
		t.Fatal("message input block was accepted")
	}
}

func TestTemplateRendererEnforcesHydratedAndDeclaredSlackLimits(t *testing.T) {
	renderer := mustEmbeddedRenderer(t)
	context := TemplateContext{
		Kind:     domain.AgentKindLLM,
		Profiles: []BuilderProviderProfile{{Reference: "openai/fast", ProviderType: agentdef.ProviderTypeOpenAICompatible}},
		Values: map[string]string{
			"name": "incident_analyst", "description": strings.Repeat("d", agentdef.MaxDescriptionLength+1),
			"instruction": "instruction", "agent_type": "llm", "model": "openai/fast",
		},
	}
	if _, err := renderer.CompileModal("builder_modal", context); err == nil {
		t.Fatal("description over agentdef limit was accepted")
	}

	files := embeddedTemplateFiles(t)
	replaceMessage(files, "agent_preview", `"{{value.name}}"`, strings.Repeat("x", builderBlockTextLimit+1))
	if _, err := LoadTemplateCatalogFromFS(templateMapFS(files)); err == nil {
		t.Fatal("agent preview section over builderBlockTextLimit was accepted")
	}

	files = embeddedTemplateFiles(t)
	data := string(files["templates/confirmation_message.json"])
	extraBlock := `{"type":"section","text":{"type":"mrkdwn","text":"x"}}`
	extraBlocks := strings.TrimSuffix(strings.Repeat("        "+extraBlock+",\n", maxBlocksPerMessage), ",\n")
	data = strings.Replace(data, "      }\n    ]", "      },\n"+extraBlocks+"\n    ]", 1)
	files["templates/confirmation_message.json"] = []byte(data)
	if _, err := LoadTemplateCatalogFromFS(templateMapFS(files)); err == nil {
		t.Fatal("message over maxBlocksPerMessage was accepted")
	}
}

func TestTemplateRendererCompilesTypedBlockCollections(t *testing.T) {
	renderer := mustEmbeddedRenderer(t)
	_, preview, err := renderer.CompileMessageWithFallback("agent_preview", TemplateContext{
		Values: map[string]string{
			"name": "*Preview*", "agent_class": "*Clase:* `LlmAgent`", "sha256": "*SHA-256:* `digest`",
			"draft_id": "draft-1", "fallback_text": "Agent preview",
		},
		PreviewYAMLParts: []string{"```yaml\nname: one", "```"},
	})
	if err != nil {
		t.Fatalf("preview collection compile: %v", err)
	}
	if len(preview) != 6 {
		t.Fatalf("preview blocks=%d, want 6", len(preview))
	}
	if preview[2].(*slackapi.SectionBlock).Text.Text != "```yaml\nname: one" || preview[3].(*slackapi.SectionBlock).Text.Text != "```" {
		t.Fatalf("preview parts=%#v", preview[2:4])
	}

	onboarding, err := renderer.CompileMessage("onboarding_message", TemplateContext{
		Values:           map[string]string{"builder_context": "context", "intro": "Intro", "describe_prompt": "Describe"},
		SuggestedPrompts: []string{"First request", "Second request"},
	})
	if err != nil {
		t.Fatalf("onboarding collection compile: %v", err)
	}
	if len(onboarding) != 5 || onboarding[3].(*slackapi.SectionBlock).Text.Text != "- First request" || onboarding[4].(*slackapi.SectionBlock).Text.Text != "- Second request" {
		t.Fatalf("onboarding blocks=%d payload=%#v", len(onboarding), onboarding)
	}
}

func TestTemplateCatalogRejectsUnknownSlackTypes(t *testing.T) {
	tests := []struct {
		name string
		edit func(map[string][]byte)
	}{
		{
			name: "unknown block",
			edit: func(files map[string][]byte) {
				replaceMessage(files, "confirmation_message", `"type": "actions",`, `"type": "future_actions",`)
			},
		},
		{
			name: "unknown accessory element",
			edit: func(files map[string][]byte) {
				replaceMessage(
					files,
					"agent_preview",
					`"text": {"type": "mrkdwn", "text": "{{value.name}}"}`,
					`"text": {"type": "mrkdwn", "text": "{{value.name}}"}, "accessory": {"type": "future_control", "action_id": "not.registered"}`,
				)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			files := embeddedTemplateFiles(t)
			test.edit(files)
			if _, err := LoadTemplateCatalogFromFS(templateMapFS(files)); err == nil {
				t.Fatal("unknown Slack type was accepted")
			}
		})
	}
}

func TestTemplateCatalogRejectsDispatchingModalActionWithStateOnlyID(t *testing.T) {
	files := embeddedTemplateFiles(t)
	path := "templates/builder_modal.json"
	var document map[string]any
	if err := json.Unmarshal(files[path], &document); err != nil {
		t.Fatalf("decode builder template: %v", err)
	}
	payload := document["payload"].(map[string]any)
	blocks := payload["blocks"].([]any)
	filtered := make([]any, 0, len(blocks))
	for _, rawBlock := range blocks {
		block := rawBlock.(map[string]any)
		if block["block_id"] != "name" {
			filtered = append(filtered, block)
		}
	}
	filtered = append(filtered, map[string]any{
		"type": "actions",
		"elements": []any{map[string]any{
			"type": "button", "action_id": "name",
			"text": map[string]any{"type": "plain_text", "text": "Unexpected action"},
		}},
	})
	payload["blocks"] = filtered
	var err error
	files[path], err = json.Marshal(document)
	if err != nil {
		t.Fatalf("encode builder template: %v", err)
	}
	if _, err := LoadTemplateCatalogFromFS(templateMapFS(files)); err == nil {
		t.Fatal("dispatching modal action with state-only ID was accepted")
	}
}

func TestTemplateCatalogRejectsInvalidParentElementCombinations(t *testing.T) {
	tests := []struct {
		name string
		edit func(map[string][]byte)
	}{
		{
			name: "button inside input",
			edit: func(files map[string][]byte) {
				path := "templates/builder_modal.json"
				data := string(files[path])
				old := `"element": {
          "type": "plain_text_input",
          "action_id": "name",
          "placeholder": {"type": "plain_text", "text": "incident_analyst", "emoji": false},
          "initial_value": "{{value.name}}"
        }`
				newValue := `"element": {
          "type": "button",
          "action_id": "name",
          "text": {"type": "plain_text", "text": "Invalid", "emoji": false}
        }`
				files[path] = []byte(strings.Replace(data, old, newValue, 1))
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			files := embeddedTemplateFiles(t)
			test.edit(files)
			if _, err := LoadTemplateCatalogFromFS(templateMapFS(files)); err == nil {
				t.Fatal("invalid parent/element combination was accepted")
			}
		})
	}
}

func TestTemplateRendererKeepsScalarReplacementAsJSONData(t *testing.T) {
	renderer := mustEmbeddedRenderer(t)
	value := `Approved", {"type":"actions"}, {{value.wrapper_call_id}}, {{fake.token}}`
	fallback, blocks, err := renderer.CompileMessageWithFallback("confirmation_message", TemplateContext{Values: map[string]string{
		"summary":         value,
		"subtitle":        "Call ID: call-1 · Expires",
		"wrapper_call_id": "wrapper-1",
		"fallback_text":   "Confirmacion requerida.",
	}})
	if err != nil {
		t.Fatalf("compile scalar fixture: %v", err)
	}
	if fallback != "Confirmacion requerida." {
		t.Fatalf("fallback unexpectedly changed: %q", fallback)
	}
	if len(blocks) != 2 {
		t.Fatalf("replacement changed block count to %d", len(blocks))
	}
	card, ok := blocks[0].(*slackapi.CardBlock)
	if !ok || card.Title == nil || card.Title.Text != value {
		t.Fatalf("hydrated summary = %#v", blocks[0])
	}
	actions, ok := blocks[1].(*slackapi.ActionBlock)
	if !ok || len(actions.Elements.ElementSet) != 2 {
		t.Fatalf("replacement changed actions block: %#v", blocks[1])
	}
}

func mustEmbeddedRenderer(t *testing.T) *TemplateRenderer {
	t.Helper()
	renderer, err := NewEmbeddedTemplateRenderer()
	if err != nil {
		t.Fatalf("new embedded renderer: %v", err)
	}
	return renderer
}

func mustCompileModal(t *testing.T, renderer *TemplateRenderer, name string, context TemplateContext) slackapi.ModalViewRequest {
	t.Helper()
	view, err := renderer.CompileModal(name, context)
	if err != nil {
		t.Fatalf("compile modal: %v", err)
	}
	return view
}

func replaceBuilder(files map[string][]byte, old, new string) {
	files["templates/builder_modal.json"] = []byte(strings.Replace(string(files["templates/builder_modal.json"]), old, new, 1))
}

func replaceMessage(files map[string][]byte, name, old, new string) {
	path := "templates/" + name + ".json"
	files[path] = []byte(strings.Replace(string(files[path]), old, new, 1))
}

func blockIDs(blocks []slackapi.Block) []string {
	ids := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if block.ID() != "" {
			ids = append(ids, block.ID())
		}
	}
	return ids
}

func optionValues(options []*slackapi.OptionBlockObject) []string {
	values := make([]string, 0, len(options))
	for _, option := range options {
		values = append(values, option.Value)
	}
	return values
}
