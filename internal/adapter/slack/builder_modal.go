package slack

import (
	"github.com/Dauno/slack-local-agent/internal/agentdef"
	slackapi "github.com/slack-go/slack"
)

// BuilderModalPresenter builds the Block Kit modal for creating an agent.
type BuilderModalPresenter struct {
	allowedProfiles []string
}

func NewBuilderModalPresenter(allowedProfiles []string) *BuilderModalPresenter {
	return &BuilderModalPresenter{allowedProfiles: allowedProfiles}
}

// BuildView returns a ModalViewRequest for the agent creation form.
func (p *BuilderModalPresenter) BuildView() slackapi.ModalViewRequest {
	var modelOptions []*slackapi.OptionBlockObject
	for _, profile := range p.allowedProfiles {
		modelOptions = append(modelOptions, slackapi.NewOptionBlockObject(
			profile,
			slackapi.NewTextBlockObject("plain_text", profile, false, false),
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

	descriptionElement := slackapi.NewPlainTextInputBlockElement(
		slackapi.NewTextBlockObject("plain_text", "Breve descripcion del proposito del agente", false, false),
		"description",
	).WithMaxLength(agentdef.MaxDescriptionLength).WithMultiline(false)
	instructionElement := slackapi.NewPlainTextInputBlockElement(
		slackapi.NewTextBlockObject("plain_text", "Instruccion completa del agente...", false, false),
		"instruction",
	).WithMaxLength(agentdef.MaxInstructionLength).WithMultiline(true)

	return slackapi.ModalViewRequest{
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
				slackapi.NewPlainTextInputBlockElement(
					slackapi.NewTextBlockObject("plain_text", "incident_analyst", false, false),
					"name",
				),
			).WithOptional(false),
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
}
