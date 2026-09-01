package toolfactory_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/adapter/toolfactory"
	"github.com/Dauno/slack-local-agent/internal/tooldef"
)

type recordingDeclarativeExecutor struct {
	name    string
	project string
	args    map[string]any
}

func (e *recordingDeclarativeExecutor) Run(_ context.Context, name, project string, args map[string]any) (tooldef.ToolResult, error) {
	e.name = name
	e.project = project
	e.args = args
	return tooldef.ToolResult{Output: "result", Truncated: true}, nil
}

func TestDeclarativeToolByNameBuildsAndRunsDeclaredTool(t *testing.T) {
	var nilFactory *toolfactory.Factory
	if _, err := nilFactory.DeclarativeToolByName("missing"); err == nil {
		t.Fatal("nil factory returned a tool")
	}

	factory := toolfactory.New(&stubConversationStore{}, nil, nil, nil)
	if factory == nil {
		t.Fatal("factory is nil")
	}
	if _, err := factory.DeclarativeToolByName("missing"); err == nil {
		t.Fatal("factory without an executor returned a tool")
	}

	executor := &recordingDeclarativeExecutor{}
	factory.WithRecoverableResults(nil).
		WithAgentBuilder(nil).
		WithAgentWriter(nil).
		WithDraftStore(nil).
		WithAllowedUserIDs([]string{"U12345678"}).
		WithCurrentDefinitions(nil).
		WithExternalAgentJobs(nil).
		WithDeclarativeTools(map[string]tooldef.ToolDef{
			"search_tool": {
				Name: "search_tool", Description: "searches the workspace",
				InputSchema: tooldef.Schema{"type": "object"}, OutputSchema: tooldef.Schema{"type": "object"},
			},
		}, executor)
	if _, err := factory.DeclarativeToolByName("missing"); err == nil {
		t.Fatal("unregistered declarative tool was returned")
	}
	declared, err := factory.DeclarativeToolByName("search_tool")
	if err != nil {
		t.Fatalf("build declarative tool: %v", err)
	}
	fn, ok := declared.(runnableFunctionTool)
	if !ok {
		t.Fatalf("tool type %T does not expose the function tool contract", declared)
	}
	ctx := &stubToolContext{callID: "call-1"}
	if _, err := fn.Run(ctx, map[string]any{}); err == nil {
		t.Fatal("declarative tool accepted a missing project")
	}
	result, err := fn.Run(ctx, map[string]any{"project": "workspace", "query": "term"})
	if err != nil {
		t.Fatalf("run declarative tool: %v", err)
	}
	if result["output"] != "result" || result["truncated"] != true || executor.name != "search_tool" || executor.project != "workspace" || executor.args["query"] != "term" {
		t.Fatalf("result=%#v executor=%+v", result, executor)
	}
}

func TestDeclarativeToolByNameReturnsExecutorErrors(t *testing.T) {
	factory := toolfactory.New(&stubConversationStore{}, nil, nil, nil)
	executor := &failingDeclarativeExecutor{}
	factory.WithDeclarativeTools(map[string]tooldef.ToolDef{"search_tool": {Name: "search_tool"}}, executor)
	declared, err := factory.DeclarativeToolByName("search_tool")
	if err != nil {
		t.Fatal(err)
	}
	fn, ok := declared.(runnableFunctionTool)
	if !ok {
		t.Fatalf("tool type %T does not expose the function tool contract", declared)
	}
	_, err = fn.Run(&stubToolContext{}, map[string]any{"project": "workspace"})
	if !errors.Is(err, errDeclarativeExecutor) {
		t.Fatalf("executor error = %v, want %v", err, errDeclarativeExecutor)
	}
}

var errDeclarativeExecutor = errors.New("declarative executor failed")

type failingDeclarativeExecutor struct{}

func (*failingDeclarativeExecutor) Run(context.Context, string, string, map[string]any) (tooldef.ToolResult, error) {
	return tooldef.ToolResult{}, errDeclarativeExecutor
}
