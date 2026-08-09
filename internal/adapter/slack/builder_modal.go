package slack

import (
	"fmt"

	"github.com/Dauno/slack-local-agent/internal/domain"
	slackapi "github.com/slack-go/slack"
)

type BuilderProviderProfile struct {
	Reference    string
	ProviderType string
}

// BuilderModalPresenter builds the Block Kit modal for creating an agent.
type BuilderModalPresenter struct {
	profiles  []BuilderProviderProfile
	renderer  *TemplateRenderer
	renderErr error
}

func NewBuilderModalPresenterWithProviders(profiles []BuilderProviderProfile) *BuilderModalPresenter {
	renderer, err := NewEmbeddedTemplateRenderer()
	if err == nil {
		for _, kind := range []domain.AgentKind{domain.AgentKindLLM, domain.AgentKindACP} {
			if kind == domain.AgentKindACP && len(builderProfilesForKind(kind, profiles)) == 0 {
				continue
			}
			_, err = renderer.CompileModal("builder_modal", TemplateContext{Kind: kind, Profiles: profiles})
			if err != nil {
				err = fmt.Errorf("validate %s builder provider profiles: %w", kind, err)
				break
			}
		}
	}
	return &BuilderModalPresenter{
		profiles:  append([]BuilderProviderProfile(nil), profiles...),
		renderer:  renderer,
		renderErr: err,
	}
}

// InitializationError exposes template setup failure to the composition root,
// which can reject startup without panicking a long-running process.
func (p *BuilderModalPresenter) InitializationError() error {
	if p == nil {
		return fmt.Errorf("builder modal presenter is required")
	}
	return p.renderErr
}

// BuildViewResult returns the initial builder modal or a bounded hydration
// error before any Slack API call is attempted.
func (p *BuilderModalPresenter) BuildViewResult() (slackapi.ModalViewRequest, error) {
	return p.BuildViewForKindResult(domain.AgentKindLLM, nil)
}

// BuildViewForCallback preserves the current modal values while changing the
// provider/profile controls after a type selection.
func (p *BuilderModalPresenter) BuildViewForCallback(callback slackapi.InteractionCallback) slackapi.ModalViewRequest {
	view, _ := p.BuildViewForCallbackResult(callback)
	return view
}

// BuildViewForCallbackResult preserves current values and surfaces hydration
// failures to the listener instead of returning a partial Slack payload.
func (p *BuilderModalPresenter) BuildViewForCallbackResult(callback slackapi.InteractionCallback) (slackapi.ModalViewRequest, error) {
	values := make(map[string]string)
	setActionValue := func(blockID, actionID string, action slackapi.BlockAction) {
		value := action.Value
		if value == "" {
			value = action.SelectedOption.Value
		}
		if blockID != "" {
			values[blockID] = value
		}
		if actionID != "" {
			values[actionID] = value
		}
	}
	setStateValues := func(state map[string]map[string]slackapi.BlockAction) {
		for blockID, actions := range state {
			for actionID, action := range actions {
				setActionValue(blockID, actionID, action)
			}
		}
	}
	if callback.View.State != nil {
		setStateValues(callback.View.State.Values)
	}
	if callback.BlockActionState != nil {
		setStateValues(callback.BlockActionState.Values)
	}
	for _, action := range callback.ActionCallback.BlockActions {
		if action != nil {
			setActionValue(action.BlockID, action.ActionID, *action)
		}
	}
	kind := domain.AgentKind(values["agent_type"])
	if kind == "" {
		kind = domain.AgentKindLLM
	}
	view, err := p.BuildViewForKindResult(kind, values)
	if err != nil {
		return slackapi.ModalViewRequest{}, err
	}
	view.PrivateMetadata = callback.View.PrivateMetadata
	return view, nil
}

// BuildViewForKind renders the form for a selected kind, preserving values when
// Slack asks the modal to update after a type change.
func (p *BuilderModalPresenter) BuildViewForKind(kind domain.AgentKind, values map[string]string) slackapi.ModalViewRequest {
	view, _ := p.BuildViewForKindResult(kind, values)
	return view
}

// BuildViewForKindResult renders one modal kind and returns any context or
// Slack-limit failure to the owning handler.
func (p *BuilderModalPresenter) BuildViewForKindResult(kind domain.AgentKind, values map[string]string) (slackapi.ModalViewRequest, error) {
	if p == nil || p.renderErr != nil || p.renderer == nil {
		if p != nil && p.renderErr != nil {
			return slackapi.ModalViewRequest{}, p.renderErr
		}
		return slackapi.ModalViewRequest{}, fmt.Errorf("builder modal presenter is not configured")
	}
	if kind != domain.AgentKindACP {
		kind = domain.AgentKindLLM
	}
	view, err := p.renderer.CompileModal("builder_modal", TemplateContext{
		Kind:     kind,
		Values:   values,
		Profiles: p.profiles,
	})
	if err != nil {
		return slackapi.ModalViewRequest{}, err
	}
	return view, nil
}

func (p *BuilderModalPresenter) profilesForKind(kind domain.AgentKind) []BuilderProviderProfile {
	return builderProfilesForKind(kind, p.profiles)
}
