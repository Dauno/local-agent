package slack

import (
	"sort"

	"github.com/Dauno/slack-local-agent/internal/agentdef"
	"github.com/Dauno/slack-local-agent/internal/domain"
	slackapi "github.com/slack-go/slack"
)

type BuilderProviderProfile struct {
	Reference    string
	ProviderType string
}

// BuilderModalPresenter builds the Block Kit modal for creating an agent.
type BuilderModalPresenter struct {
	profiles []BuilderProviderProfile
}

func NewBuilderModalPresenter(allowedProfiles []string) *BuilderModalPresenter {
	profiles := make([]BuilderProviderProfile, 0, len(allowedProfiles))
	for _, profile := range allowedProfiles {
		profiles = append(profiles, BuilderProviderProfile{Reference: profile, ProviderType: "openai_compatible"})
	}
	return NewBuilderModalPresenterWithProviders(profiles)
}

func NewBuilderModalPresenterWithProviders(profiles []BuilderProviderProfile) *BuilderModalPresenter {
	return &BuilderModalPresenter{profiles: append([]BuilderProviderProfile(nil), profiles...)}
}

// BuildView returns a ModalViewRequest for the agent creation form.
func (p *BuilderModalPresenter) BuildView() slackapi.ModalViewRequest {
	return p.BuildViewForKind(domain.AgentKindLLM, nil)
}

// BuildViewForCallback preserves the current modal values while changing the
// provider/profile controls after a type selection.
func (p *BuilderModalPresenter) BuildViewForCallback(callback slackapi.InteractionCallback) slackapi.ModalViewRequest {
	values := make(map[string]string)
	for _, action := range callback.ActionCallback.BlockActions {
		if action == nil {
			continue
		}
		value := action.Value
		if value == "" {
			value = action.SelectedOption.Value
		}
		values[action.BlockID] = value
		values[action.ActionID] = value
	}
	if callback.BlockActionState != nil {
		for blockID, actions := range callback.BlockActionState.Values {
			for actionID, action := range actions {
				value := action.Value
				if value == "" {
					value = action.SelectedOption.Value
				}
				values[blockID] = value
				values[actionID] = value
			}
		}
	} else if callback.View.State != nil {
		for blockID, actions := range callback.View.State.Values {
			for actionID, action := range actions {
				value := action.Value
				if value == "" {
					value = action.SelectedOption.Value
				}
				values[blockID] = value
				values[actionID] = value
			}
		}
	}
	kind := domain.AgentKind(values["agent_type"])
	if kind == "" {
		kind = domain.AgentKindLLM
	}
	view := p.BuildViewForKind(kind, values)
	view.PrivateMetadata = callback.View.PrivateMetadata
	return view
}

// BuildViewForKind renders the form for a selected kind, preserving values when
// Slack asks the modal to update after a type change.
func (p *BuilderModalPresenter) BuildViewForKind(kind domain.AgentKind, values map[string]string) slackapi.ModalViewRequest {
	if kind != domain.AgentKindACP {
		kind = domain.AgentKindLLM
	}
	var modelOptions []*slackapi.OptionBlockObject
	for _, profile := range p.profilesForKind(kind) {
		modelOptions = append(modelOptions, slackapi.NewOptionBlockObject(
			profile.Reference,
			slackapi.NewTextBlockObject("plain_text", profile.Reference, false, false),
			nil,
		))
	}

	modelSelect := slackapi.NewOptionsSelectBlockElement(
		slackapi.OptTypeStatic,
		slackapi.NewTextBlockObject("plain_text", "Elige un modelo", false, false),
		"model",
		modelOptions...,
	)
	if len(modelOptions) > 0 {
		modelSelect.WithInitialOption(modelOptions[0])
	}
	if selected := values["model"]; selected != "" {
		for _, option := range modelOptions {
			if option.Value == selected {
				modelSelect.WithInitialOption(option)
				break
			}
		}
	}

	typeOptions := []*slackapi.OptionBlockObject{
		slackapi.NewOptionBlockObject("llm", slackapi.NewTextBlockObject("plain_text", "LLM", false, false), nil),
		slackapi.NewOptionBlockObject("acp", slackapi.NewTextBlockObject("plain_text", "ACP", false, false), nil),
	}
	typeSelect := slackapi.NewOptionsSelectBlockElement(
		slackapi.OptTypeStatic,
		slackapi.NewTextBlockObject("plain_text", "Elige un tipo", false, false),
		"agent_type",
		typeOptions...,
	)
	selectedKind := string(kind)
	if values["agent_type"] == string(domain.AgentKindACP) {
		selectedKind = string(domain.AgentKindACP)
	}
	for _, option := range typeOptions {
		if option.Value == selectedKind {
			typeSelect.WithInitialOption(option)
			break
		}
	}

	descriptionElement := slackapi.NewPlainTextInputBlockElement(
		slackapi.NewTextBlockObject("plain_text", "Breve descripcion del proposito del agente", false, false),
		"description",
	).WithMaxLength(agentdef.MaxDescriptionLength).WithMultiline(false)
	if values["description"] != "" {
		descriptionElement.WithInitialValue(values["description"])
	}
	instructionElement := slackapi.NewPlainTextInputBlockElement(
		slackapi.NewTextBlockObject("plain_text", "Instruccion completa del agente...", false, false),
		"instruction",
	).WithMaxLength(agentdef.MaxInstructionLength).WithMultiline(true)
	if values["instruction"] != "" {
		instructionElement.WithInitialValue(values["instruction"])
	}
	nameElement := slackapi.NewPlainTextInputBlockElement(
		slackapi.NewTextBlockObject("plain_text", "incident_analyst", false, false),
		"name",
	)
	if values["name"] != "" {
		nameElement.WithInitialValue(values["name"])
	}

	view := slackapi.ModalViewRequest{
		Type:       slackapi.VTModal,
		Title:      slackapi.NewTextBlockObject("plain_text", "Crear nuevo agente", false, false),
		Submit:     slackapi.NewTextBlockObject("plain_text", "Previsualizar", false, false),
		Close:      slackapi.NewTextBlockObject("plain_text", "Cancelar", false, false),
		CallbackID: "local_agent.builder.submit",
		Blocks: slackapi.Blocks{BlockSet: []slackapi.Block{
			slackapi.NewSectionBlock(
				slackapi.NewTextBlockObject("mrkdwn", "Completa los campos para definir un nuevo agente.", false, false),
				nil,
				nil,
			),
			slackapi.NewInputBlock(
				"name",
				slackapi.NewTextBlockObject("plain_text", "Nombre", false, false),
				slackapi.NewTextBlockObject("plain_text", "3-64 caracteres, solo minusculas, numeros, _ y -", false, false),
				nameElement,
			).WithOptional(false),
			slackapi.NewInputBlock(
				"agent_type",
				slackapi.NewTextBlockObject("plain_text", "Tipo", false, false),
				nil,
				typeSelect,
			).WithOptional(false).WithDispatchAction(true),
			slackapi.NewInputBlock(
				"description",
				slackapi.NewTextBlockObject("plain_text", "Descripcion", false, false),
				nil,
				descriptionElement,
			).WithOptional(false),
			slackapi.NewInputBlock(
				"instruction",
				slackapi.NewTextBlockObject("plain_text", "Instruccion", false, false),
				nil,
				instructionElement,
			).WithOptional(false),
			slackapi.NewInputBlock(
				"model",
				slackapi.NewTextBlockObject("plain_text", "Modelo", false, false),
				nil,
				modelSelect,
			).WithOptional(false),
		}},
	}
	if kind == domain.AgentKindACP {
		modeOptions := []*slackapi.OptionBlockObject{
			slackapi.NewOptionBlockObject(domain.ExecutionModeForeground, slackapi.NewTextBlockObject("plain_text", domain.ExecutionModeForeground, false, false), nil),
			slackapi.NewOptionBlockObject(domain.ExecutionModeDurableJob, slackapi.NewTextBlockObject("plain_text", domain.ExecutionModeDurableJob, false, false), nil),
		}
		modeSelect := slackapi.NewOptionsSelectBlockElement(
			slackapi.OptTypeStatic,
			slackapi.NewTextBlockObject("plain_text", "Elige la ejecucion", false, false),
			"execution_mode",
			modeOptions...,
		)
		selectedMode := values["execution_mode"]
		if selectedMode == "" {
			selectedMode = domain.ExecutionModeForeground
		}
		for _, option := range modeOptions {
			if option.Value == selectedMode {
				modeSelect.WithInitialOption(option)
				break
			}
		}
		timeout := slackapi.NewPlainTextInputBlockElement(
			slackapi.NewTextBlockObject("plain_text", "7200", false, false),
			"timeout_seconds",
		).WithInitialValue(values["timeout_seconds"]).WithMaxLength(5)
		if timeout.InitialValue == "" {
			timeout.WithInitialValue("7200")
		}
		rest := []slackapi.Block{
			slackapi.NewInputBlock(
				"execution_mode",
				slackapi.NewTextBlockObject("plain_text", "Ejecucion", false, false),
				nil,
				modeSelect,
			).WithOptional(false),
			slackapi.NewInputBlock(
				"timeout_seconds",
				slackapi.NewTextBlockObject("plain_text", "Timeout (segundos)", false, false),
				slackapi.NewTextBlockObject("plain_text", "Maximo 86400", false, false),
				timeout,
			).WithOptional(false),
		}
		view.Blocks.BlockSet = append(view.Blocks.BlockSet, rest...)
	}
	return view
}

func (p *BuilderModalPresenter) profilesForKind(kind domain.AgentKind) []BuilderProviderProfile {
	profiles := make([]BuilderProviderProfile, 0, len(p.profiles))
	for _, profile := range p.profiles {
		if kind == domain.AgentKindLLM && profile.ProviderType == agentdef.ProviderTypeOpenAICompatible {
			profiles = append(profiles, profile)
		}
		if kind == domain.AgentKindACP && profile.ProviderType == agentdef.ProviderTypeACP && len(profile.Reference) > len("opencode/") && profile.Reference[:len("opencode/")] == "opencode/" {
			profiles = append(profiles, profile)
		}
	}
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].Reference < profiles[j].Reference })
	return profiles
}
