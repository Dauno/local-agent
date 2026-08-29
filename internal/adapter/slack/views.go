package slack

import (
	"embed"
	"io/fs"
	"time"

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

func newJobEngine() (*blockkit.Engine, error) {
	engine, err := newViewEngine()
	if err != nil {
		return nil, err
	}
	if err := engine.Register(jobAcceptedView{}, jobStatusView{}, jobStatusErrorView{}); err != nil {
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

type jobAcceptedView struct {
	JobID          string    `bk:"job_id"`
	Status         string    `bk:"status"`
	CreatedAt      time.Time `bk:"created_at"`
	UpdatedAt      time.Time `bk:"updated_at"`
	StatusSentence string    `bk:"status_sentence"`
}

func (jobAcceptedView) Template() string { return "job.accepted" }

type jobStatusView struct {
	JobID      string `bk:"job_id"`
	Status     string `bk:"status"`
	CreatedAt  string `bk:"created_at"`
	UpdatedAt  string `bk:"updated_at"`
	HostStatus string `bk:"host_status"`
}

func (jobStatusView) Template() string { return "job.status" }

type jobStatusErrorView struct {
	Message string `bk:"message"`
}

func (jobStatusErrorView) Template() string { return "job.status_error" }
