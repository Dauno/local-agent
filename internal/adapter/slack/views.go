package slack

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"time"

	"github.com/Dauno/slack-local-agent/internal/blockkit"
)

//go:embed views
var viewsFS embed.FS

const confirmationPromptTemplateName = "confirmation.prompt"

func NewViewEngine() (*blockkit.Engine, error) {
	rooted, err := fs.Sub(viewsFS, "views")
	if err != nil {
		return nil, err
	}
	return newCompleteViewEngine(rooted)
}

func newViewEngine() (*blockkit.Engine, error) {
	rooted, err := fs.Sub(viewsFS, "views")
	if err != nil {
		return nil, err
	}
	return blockkit.New(rooted)
}

type templateBinding struct {
	view       blockkit.View
	submission blockkit.View
}

var templateBindings = []templateBinding{
	{view: agentPreviewView{}},
	{view: builderModalView{}, submission: builderModalSubmission{}},
	{view: confirmationPromptView{}},
	{view: confirmationResolvedView{}},
	{view: jobAcceptedView{}},
	{view: jobStatusView{}},
	{view: jobStatusErrorView{}},
	{view: onboardingWelcomeView{}},
}

func newCompleteViewEngine(fsys fs.FS) (*blockkit.Engine, error) {
	engine, err := blockkit.New(fsys)
	if err != nil {
		return nil, err
	}
	if err := registerTemplateBindings(engine); err != nil {
		return nil, err
	}
	if err := validateCompleteViewEngine(engine); err != nil {
		return nil, err
	}
	return engine, nil
}

func newTemplateSubsetEngine(names ...string) (*blockkit.Engine, error) {
	engine, err := newViewEngine()
	if err != nil {
		return nil, err
	}
	if err := registerTemplateBindings(engine, names...); err != nil {
		return nil, err
	}
	return engine, nil
}

func registerTemplateBindings(engine *blockkit.Engine, names ...string) error {
	wanted := make(map[string]struct{}, len(names))
	for _, name := range names {
		wanted[name] = struct{}{}
	}
	all := len(names) == 0
	views := make([]blockkit.View, 0, len(templateBindings))
	submissions := make([]blockkit.View, 0, len(templateBindings))
	for _, binding := range templateBindings {
		name := binding.view.Template()
		if !all {
			if _, ok := wanted[name]; !ok {
				continue
			}
			delete(wanted, name)
		}
		views = append(views, binding.view)
		if binding.submission != nil {
			submissions = append(submissions, binding.submission)
		}
	}
	if len(wanted) != 0 {
		missing := make([]string, 0, len(wanted))
		for name := range wanted {
			missing = append(missing, name)
		}
		slices.Sort(missing)
		return fmt.Errorf("template bindings requested unknown template %q", missing[0])
	}
	if err := engine.Register(views...); err != nil {
		return err
	}
	return engine.RegisterSubmit(submissions...)
}

func validateCompleteViewEngine(engine *blockkit.Engine) error {
	linked := make(map[string]struct{}, len(templateBindings))
	for _, binding := range templateBindings {
		linked[binding.view.Template()] = struct{}{}
	}
	for _, name := range engine.Names() {
		if _, ok := linked[name]; !ok {
			return fmt.Errorf("template %q has no linked view", name)
		}
	}
	return nil
}

func confirmationPromptLayoutSHA256(engine *blockkit.Engine) (string, error) {
	if engine == nil {
		return "", errors.New("slack view engine is required")
	}
	layoutSHA256, ok := engine.LayoutSHA256(confirmationPromptTemplateName)
	if !ok {
		return "", fmt.Errorf("view template %q is not registered", confirmationPromptTemplateName)
	}
	if layoutSHA256 == "" {
		return "", fmt.Errorf("view template %q has an empty layout fingerprint", confirmationPromptTemplateName)
	}
	return layoutSHA256, nil
}

func newAgentPreviewEngine() (*blockkit.Engine, error) {
	return newTemplateSubsetEngine("agent.preview")
}

func newOnboardingEngine() (*blockkit.Engine, error) {
	return newTemplateSubsetEngine("onboarding.welcome")
}

func newJobEngine() (*blockkit.Engine, error) {
	return newTemplateSubsetEngine("job.accepted", "job.status", "job.status_error")
}

const (
	maxInteractiveIDLength    = 255
	maxInteractiveValueLength = 2000
	jobAcceptedSubtitleLimit  = 150
)

func newBuilderModalEngine() (*blockkit.Engine, error) {
	return newTemplateSubsetEngine("agent.builder")
}

type builderModalView struct {
	Name            string          `bk:"name,omitempty"`
	AgentType       string          `bk:"agent_type"`
	AgentTypeLabel  string          `bk:"agent_type_label"`
	Description     string          `bk:"description,omitempty"`
	Instruction     string          `bk:"instruction,omitempty"`
	Models          []blockkit.Pair `bk:"models"`
	Model           string          `bk:"model"`
	IsExternalAgent bool            `bk:"is_external_agent"`
	ExecutionMode   string          `bk:"execution_mode,omitempty"`
	TimeoutSeconds  string          `bk:"timeout_seconds,omitempty"`
}

func (builderModalView) Template() string { return "agent.builder" }

type builderModalSubmission struct {
	Name           string `bk:"name"`
	AgentType      string `bk:"agent_type"`
	Description    string `bk:"description,omitempty"`
	Instruction    string `bk:"instruction,omitempty"`
	Model          string `bk:"model"`
	ExecutionMode  string `bk:"execution_mode,omitempty"`
	TimeoutSeconds int    `bk:"timeout_seconds,omitempty"`
}

func (builderModalSubmission) Template() string { return "agent.builder" }

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
