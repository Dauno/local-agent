package slack

import (
	"embed"
	"io/fs"

	"github.com/Dauno/slack-local-agent/internal/blockkit"
)

//go:embed views
var viewsFS embed.FS

func newViewEngine() (*blockkit.Engine, error) {
	rooted, err := fs.Sub(viewsFS, "views")
	if err != nil {
		return nil, err
	}
	return blockkit.New(rooted)
}

func newAgentPreviewEngine() (*blockkit.Engine, error) {
	engine, err := newViewEngine()
	if err != nil {
		return nil, err
	}
	if err := engine.Register(agentPreviewView{}); err != nil {
		return nil, err
	}
	return engine, nil
}

func newOnboardingEngine() (*blockkit.Engine, error) {
	engine, err := newViewEngine()
	if err != nil {
		return nil, err
	}
	if err := engine.Register(onboardingWelcomeView{}); err != nil {
		return nil, err
	}
	return engine, nil
}

type agentPreviewView struct {
	Name            string `bk:"name"`
	AgentClass      string `bk:"agent_class"`
	ProviderProfile string `bk:"provider_profile"`
	ExecutionMode   string `bk:"execution_mode"`
	Timeout         string `bk:"timeout"`
	SHA256          string `bk:"sha256"`
	DraftID         string `bk:"draft_id"`
	PreviewYAML     string `bk:"preview_yaml_parts"`
}

func (agentPreviewView) Template() string { return "agent.preview" }

type onboardingWelcomeView struct {
	BuilderContext   string          `bk:"builder_context"`
	Intro            string          `bk:"intro"`
	DescribePrompt   string          `bk:"describe_prompt"`
	SuggestedPrompts []blockkit.Pair `bk:"suggested_prompts,omitempty"`
}

func (onboardingWelcomeView) Template() string { return "onboarding.welcome" }
