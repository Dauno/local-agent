package app

import (
	"context"
	"errors"
	"fmt"

	slackadapter "github.com/Dauno/slack-local-agent/internal/adapter/slack"
)

// LintTemplates validates the embedded template registry and returns one line
// for each linked template.
func (a *Application) LintTemplates(ctx context.Context) ([]string, error) {
	if a == nil {
		return nil, errors.New("application is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	engine, err := slackadapter.NewViewEngine()
	if err != nil {
		return nil, fmt.Errorf("load embedded Slack templates: %w", err)
	}
	names := engine.Names()
	lines := make([]string, 0, len(names))
	for _, name := range names {
		description, ok := engine.Describe(name)
		if !ok {
			return nil, fmt.Errorf("describe template %q: template is unavailable", name)
		}
		lines = append(lines, fmt.Sprintf("%s: %s", description.Name, description.Surface))
	}
	return lines, nil
}

// PreviewTemplate renders one embedded template without reading project state
// or contacting Slack.
func (a *Application) PreviewTemplate(ctx context.Context, name string, includeOptional bool) (string, error) {
	if a == nil {
		return "", errors.New("application is required")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	engine, err := slackadapter.NewViewEngine()
	if err != nil {
		return "", fmt.Errorf("load embedded Slack templates: %w", err)
	}
	preview, err := engine.Preview(name, includeOptional)
	if err != nil {
		return "", err
	}
	return string(preview.JSON), nil
}
