package agentcli_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/Dauno/slack-local-agent/internal/adapter/agentcli"
	"github.com/Dauno/slack-local-agent/internal/agentdef"
	"github.com/Dauno/slack-local-agent/internal/domain"
)

func TestHelperProcess(t *testing.T) {
	if !strings.Contains(strings.Join(os.Args, " "), "helper-agent-cli") {
		return
	}
	if strings.Contains(strings.Join(os.Args, " "), "--version") {
		_, _ = os.Stdout.WriteString("fake-cli 1.2.3\n")
		os.Exit(0)
	}
	input, _ := io.ReadAll(os.Stdin)
	for _, argument := range os.Args {
		if strings.HasPrefix(argument, "dump=") {
			_ = os.WriteFile(strings.TrimPrefix(argument, "dump="), input, 0o600)
		}
		if strings.HasPrefix(argument, "dumpargv=") {
			_ = os.WriteFile(strings.TrimPrefix(argument, "dumpargv="), []byte(strings.Join(os.Args, "\n")), 0o600)
		}
		if strings.HasPrefix(argument, "dumpcwd=") {
			cwd, _ := os.Getwd()
			_ = os.WriteFile(strings.TrimPrefix(argument, "dumpcwd="), []byte(cwd), 0o600)
		}
	}
	mode := "success"
	for _, argument := range os.Args {
		if strings.HasPrefix(argument, "mode=") {
			mode = strings.TrimPrefix(argument, "mode=")
		}
	}
	switch mode {
	case "failure":
		_, _ = os.Stdout.WriteString(`{"type":"result","is_error":true,"result":"bad"}` + "\n")
	case "after-terminal":
		_, _ = os.Stdout.WriteString(`{"type":"result","is_error":false,"result":"ok"}` + "\n" + `{"type":"later"}` + "\n")
	case "activity":
		// One ignored event, one discarded event, two reportable steps, one
		// event whose declared type field is absent, then the result. Only the
		// two reportable steps may reach a reporter.
		for _, line := range []string{
			`{"type":"rate_limit_event","tokens":10}`,
			`{"type":"assistant","message":{"content":[{"type":"thinking","text":"secret reasoning"}]}}`,
			`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash"}]}}`,
			`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit"}]}}`,
			`{"type":"assistant","message":{}}`,
			`{"type":"result","is_error":false,"result":"final text"}`,
		} {
			_, _ = os.Stdout.WriteString(line + "\n")
		}
	case "ignored-after-terminal":
		_, _ = os.Stdout.WriteString(`{"type":"result","is_error":false,"result":"ok"}` + "\n" + `{"type":"rate_limit_event"}` + "\n")
	default:
		_, _ = os.Stdout.WriteString(`{"type":"assistant","message":{"content":[{"type":"thinking","text":"secret reasoning"}]}}` + "\n" + `{"type":"result","is_error":false,"result":"final text"}` + "\n")
	}
	os.Exit(0)
}

func testLLM(t *testing.T, mode, dump string) *agentcli.LLM {
	t.Helper()
	dir := t.TempDir()
	provider := agentdef.Provider{Name: "fake", Type: agentdef.ProviderTypeAgentCLI, Version: &agentdef.CLIVersion{Command: []string{"-test.run=^TestHelperProcess$", "helper-agent-cli", "--version"}, Pattern: `fake-cli (?P<version>\d+\.\d+\.\d+)`, Min: "1.0.0", Max: "1.9.9"}, Invocation: &agentdef.CLIInvocation{Prompt: "stdin", Args: []string{"-test.run=^TestHelperProcess$", "helper-agent-cli", "mode=" + mode, "dump=" + dump}, Options: map[string]agentdef.CLIInvocationOption{"model": {Flag: "--model"}}}, Stream: &agentdef.CLIStream{Format: "ndjson", FinalText: agentdef.CLIFinalText{When: map[string]string{"type": "result"}, Path: "result"}, Failure: agentdef.CLIFailure{WhenAny: []map[string]string{{"is_error": "true"}}}, Activity: &agentdef.CLIActivity{When: map[string]string{"type": "assistant"}, TypeField: "message.content.0.type", DiscardTypes: []string{"thinking"}}, TerminalTypes: []string{"result"}}}
	llm, err := agentcli.New(agentcli.Config{Command: os.Args[0], Provider: provider, Profile: agentdef.Profile{Model: "fake-model"}, Workspace: domain.Workspace{WorkingDirectory: dir, Projects: []domain.Project{{Name: "workspace", Path: dir}}}, WorkingDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	return llm
}

// delegate renders the {project, task} object that ADK serializes from the
// leaf's declared input schema. Every agent CLI call now arrives in this shape.
func delegate(project, task string) string {
	data, _ := json.Marshal(map[string]string{"project": project, "task": task})
	return string(data)
}

func collect(t *testing.T, llm *agentcli.LLM) (*model.LLMResponse, error) {
	t.Helper()
	request := &model.LLMRequest{Contents: []*genai.Content{genai.NewContentFromText(delegate("workspace", "private prompt"), genai.RoleUser)}}
	for response, err := range llm.GenerateContent(context.Background(), request, false) {
		return response, err
	}
	return nil, nil
}

func TestDirectCLIParsesFinalTextAndKeepsPromptOffArgv(t *testing.T) {
	dump := filepath.Join(t.TempDir(), "stdin")
	llm := testLLM(t, "success", dump)
	if err := llm.Validate(t.Context()); err != nil {
		t.Fatalf("validate: %v", err)
	}
	response, err := collect(t, llm)
	if err != nil || response.Content.Parts[0].Text != "final text" {
		t.Fatalf("response = %+v, error = %v", response, err)
	}
	data, err := os.ReadFile(dump)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "private prompt") {
		t.Fatal("prompt was not written to stdin")
	}
	if strings.Contains(strings.Join(os.Args, " "), "private prompt") {
		t.Fatal("prompt entered argv")
	}
}

func TestDirectCLIClassifiesNativeFailure(t *testing.T) {
	_, err := collect(t, testLLM(t, "failure", ""))
	var cliErr *agentcli.CLIError
	if !errors.As(err, &cliErr) || cliErr.Code != agentcli.CodeProcessFailed {
		t.Fatalf("error = %v", err)
	}
}

func TestEventAfterTerminalFails(t *testing.T) {
	_, err := collect(t, testLLM(t, "after-terminal", ""))
	var cliErr *agentcli.CLIError
	if !errors.As(err, &cliErr) || cliErr.Code != agentcli.CodeProcessFailed {
		t.Fatalf("error = %v", err)
	}
}

func TestPromptContainsNoSessionOrNativePayload(t *testing.T) {
	prompt := map[string]string{"user": "private prompt"}
	data, _ := json.Marshal(prompt)
	if !strings.Contains(string(data), "private prompt") {
		t.Fatal("fixture invalid")
	}
}

// systemPromptLLM builds a CLI whose descriptor declares a native
// system-prompt channel. The helper dumps both stdin and argv so a test can
// assert which content took which path.
func systemPromptLLM(t *testing.T, dumpStdin, dumpArgv string) *agentcli.LLM {
	t.Helper()
	dir := t.TempDir()
	provider := agentdef.Provider{
		Name: "fake", Type: agentdef.ProviderTypeAgentCLI,
		Version: &agentdef.CLIVersion{Command: []string{"-test.run=^TestHelperProcess$", "helper-agent-cli", "--version"}, Pattern: `fake-cli (?P<version>\d+\.\d+\.\d+)`, Min: "1.0.0"},
		Invocation: &agentdef.CLIInvocation{
			Prompt:       "stdin",
			SystemPrompt: &agentdef.CLISystemPrompt{Flag: "--append-system-prompt"},
			Args:         []string{"-test.run=^TestHelperProcess$", "helper-agent-cli", "mode=success", "dump=" + dumpStdin, "dumpargv=" + dumpArgv},
		},
		Stream: &agentdef.CLIStream{Format: "ndjson", FinalText: agentdef.CLIFinalText{When: map[string]string{"type": "result"}, Path: "result"}, Failure: agentdef.CLIFailure{WhenAny: []map[string]string{{"is_error": "true"}}}, Activity: &agentdef.CLIActivity{When: map[string]string{"type": "assistant"}, TypeField: "message.content.0.type", DiscardTypes: []string{"thinking"}}, TerminalTypes: []string{"result"}},
	}
	llm, err := agentcli.New(agentcli.Config{
		Command: os.Args[0], Provider: provider, Profile: agentdef.Profile{Model: "fake-model"},
		Workspace:  domain.Workspace{WorkingDirectory: dir, Projects: []domain.Project{{Name: "workspace", Path: dir}}},
		WorkingDir: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	return llm
}

func generate(t *testing.T, llm *agentcli.LLM, instruction, task string) {
	t.Helper()
	request := &model.LLMRequest{
		Config:   &genai.GenerateContentConfig{SystemInstruction: genai.NewContentFromText(instruction, genai.RoleUser)},
		Contents: []*genai.Content{genai.NewContentFromText(delegate("workspace", task), genai.RoleUser)},
	}
	for _, err := range llm.GenerateContent(context.Background(), request, false) {
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
	}
}

// The instruction must reach the CLI through its own channel, and the stdin
// prompt must then carry the task alone.
func TestSystemPromptChannelKeepsInstructionOffStdin(t *testing.T) {
	dir := t.TempDir()
	stdinPath, argvPath := filepath.Join(dir, "stdin"), filepath.Join(dir, "argv")
	generate(t, systemPromptLLM(t, stdinPath, argvPath), "agent instruction here", "the delegated task")

	stdin, err := os.ReadFile(stdinPath)
	if err != nil {
		t.Fatal(err)
	}
	argv, err := os.ReadFile(argvPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(stdin)) != "the delegated task" {
		t.Fatalf("stdin must carry the task alone, got %q", string(stdin))
	}
	if !strings.Contains(string(argv), "--append-system-prompt") || !strings.Contains(string(argv), "agent instruction here") {
		t.Fatalf("instruction did not reach the native channel: %s", string(argv))
	}
}

// Without a declared channel the instruction falls back to stdin, but no
// section may claim to be trusted. That claim is what Claude Code reads as a
// prompt injection.
func TestFallbackPromptCarriesInstructionWithoutTrustClaim(t *testing.T) {
	dump := filepath.Join(t.TempDir(), "stdin")
	generate(t, testLLM(t, "success", dump), "agent instruction here", "the delegated task")

	data, err := os.ReadFile(dump)
	if err != nil {
		t.Fatal(err)
	}
	prompt := string(data)
	if !strings.Contains(prompt, "agent instruction here") || !strings.Contains(prompt, "the delegated task") {
		t.Fatalf("fallback prompt lost content: %q", prompt)
	}
	for _, banned := range []string{"trusted", "TRUSTED", "<<AGENT INSTRUCTIONS", "<<WORKSPACE REGISTRY", "<<CONVERSATION TRANSCRIPT"} {
		if strings.Contains(prompt, banned) {
			t.Fatalf("prompt still asserts trust or framing (%q): %q", banned, prompt)
		}
	}
}

// A single-project workspace adds nothing the workspace flags do not already
// carry, so the registry must stay out of the prompt.
func TestSingleProjectWorkspaceIsNotSerializedIntoPrompt(t *testing.T) {
	dump := filepath.Join(t.TempDir(), "stdin")
	generate(t, testLLM(t, "success", dump), "", "the delegated task")

	data, err := os.ReadFile(dump)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "Registered projects") {
		t.Fatalf("registry leaked for a single-project workspace: %q", string(data))
	}
}

// twoProjectLLM registers a second project so a test can prove the caller's
// choice, not the process working directory, decides where the CLI runs.
func twoProjectLLM(t *testing.T, dumpCWD string) (*agentcli.LLM, string, string) {
	t.Helper()
	alpha, beta := t.TempDir(), t.TempDir()
	provider := agentdef.Provider{
		Name: "fake", Type: agentdef.ProviderTypeAgentCLI,
		Version:    &agentdef.CLIVersion{Command: []string{"-test.run=^TestHelperProcess$", "helper-agent-cli", "--version"}, Pattern: `fake-cli (?P<version>\d+\.\d+\.\d+)`, Min: "1.0.0"},
		Invocation: &agentdef.CLIInvocation{Prompt: "stdin", Args: []string{"-test.run=^TestHelperProcess$", "helper-agent-cli", "mode=success", "dumpcwd=" + dumpCWD}},
		Stream:     &agentdef.CLIStream{Format: "ndjson", FinalText: agentdef.CLIFinalText{When: map[string]string{"type": "result"}, Path: "result"}, Failure: agentdef.CLIFailure{WhenAny: []map[string]string{{"is_error": "true"}}}, Activity: &agentdef.CLIActivity{When: map[string]string{"type": "assistant"}, TypeField: "message.content.0.type", DiscardTypes: []string{"thinking"}}, TerminalTypes: []string{"result"}},
	}
	llm, err := agentcli.New(agentcli.Config{
		Command: os.Args[0], Provider: provider, Profile: agentdef.Profile{Model: "fake-model"},
		Workspace:  domain.Workspace{WorkingDirectory: alpha, Projects: []domain.Project{{Name: "alpha", Path: alpha}, {Name: "beta", Path: beta}}},
		WorkingDir: alpha,
	})
	if err != nil {
		t.Fatal(err)
	}
	return llm, alpha, beta
}

func run(llm *agentcli.LLM, project, task string) error {
	request := &model.LLMRequest{Contents: []*genai.Content{genai.NewContentFromText(delegate(project, task), genai.RoleUser)}}
	for _, err := range llm.GenerateContent(context.Background(), request, false) {
		return err
	}
	return nil
}

// The caller names the workspace, so a project other than the process working
// directory must reach the CLI. This is what the application-root requirement
// used to make impossible.
func TestSelectedProjectDecidesWorkingDirectory(t *testing.T) {
	cwdPath := filepath.Join(t.TempDir(), "cwd")
	llm, alpha, beta := twoProjectLLM(t, cwdPath)
	if err := run(llm, "beta", "the task"); err != nil {
		t.Fatalf("generate: %v", err)
	}
	data, err := os.ReadFile(cwdPath)
	if err != nil {
		t.Fatal(err)
	}
	// The helper reports its own working directory, so this asserts where the
	// process actually ran rather than which flags were built.
	got, err := filepath.EvalSymlinks(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(beta)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("CLI ran in %q, want the selected project %q (process root is %q)", got, want, alpha)
	}
}

func TestUnknownProjectIsRejectedAndNamesTheRegistry(t *testing.T) {
	llm, _, _ := twoProjectLLM(t, filepath.Join(t.TempDir(), "argv"))
	err := run(llm, "not-registered", "the task")
	var cliErr *agentcli.CLIError
	if !errors.As(err, &cliErr) || cliErr.Code != agentcli.CodeInvalidRequest {
		t.Fatalf("error = %v", err)
	}
	for _, want := range []string{"not-registered", "alpha", "beta"} {
		if !strings.Contains(cliErr.Message, want) {
			t.Fatalf("error must name %q so the caller can retry: %s", want, cliErr.Message)
		}
	}
}

// A caller that does not use the declared schema is rejected rather than
// defaulted, because a default would pick a workspace on the caller's behalf.
func TestNonDelegationRequestIsRejected(t *testing.T) {
	llm, _, _ := twoProjectLLM(t, filepath.Join(t.TempDir(), "argv"))
	request := &model.LLMRequest{Contents: []*genai.Content{genai.NewContentFromText("just some free text", genai.RoleUser)}}
	var got error
	for _, err := range llm.GenerateContent(context.Background(), request, false) {
		got = err
	}
	var cliErr *agentcli.CLIError
	if !errors.As(got, &cliErr) || cliErr.Code != agentcli.CodeInvalidRequest {
		t.Fatalf("error = %v", got)
	}
}

// The project name is routing data for the host. Only the task belongs in the
// prompt the CLI reads.
func TestProjectNameDoesNotEnterThePrompt(t *testing.T) {
	dump := filepath.Join(t.TempDir(), "stdin")
	llm := testLLM(t, "success", dump)
	if err := run(llm, "workspace", "the delegated task"); err != nil {
		t.Fatalf("generate: %v", err)
	}
	data, err := os.ReadFile(dump)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "\"project\"") || strings.Contains(string(data), "workspace") {
		t.Fatalf("routing data leaked into the prompt: %q", string(data))
	}
	if !strings.Contains(string(data), "the delegated task") {
		t.Fatalf("task missing from prompt: %q", string(data))
	}
}

// Preconditions describe the selected project, so they run for each delegation
// and fail that call rather than the process.
func TestPreconditionRunsAgainstTheSelectedProject(t *testing.T) {
	llm, _, _ := twoProjectLLM(t, filepath.Join(t.TempDir(), "argv"))
	if err := llm.Validate(t.Context()); err != nil {
		t.Fatalf("startup validation must not need a project: %v", err)
	}
}

// activityLLM builds a model whose descriptor both ignores a type and reports
// two step types, so one run exercises every activity rule at once.
func activityLLM(t *testing.T, mode string) *agentcli.LLM {
	t.Helper()
	dir := t.TempDir()
	provider := agentdef.Provider{
		Name: "fake", Type: agentdef.ProviderTypeAgentCLI,
		Version: &agentdef.CLIVersion{Command: []string{"-test.run=^TestHelperProcess$", "helper-agent-cli", "--version"}, Pattern: `fake-cli (?P<version>\d+\.\d+\.\d+)`, Min: "1.0.0"},
		Invocation: &agentdef.CLIInvocation{
			Prompt: "stdin",
			Args:   []string{"-test.run=^TestHelperProcess$", "helper-agent-cli", "mode=" + mode},
		},
		Stream: &agentdef.CLIStream{
			Format:      "ndjson",
			IgnoreTypes: []string{"rate_limit_event"},
			FinalText:   agentdef.CLIFinalText{When: map[string]string{"type": "result"}, Path: "result"},
			Failure:     agentdef.CLIFailure{WhenAny: []map[string]string{{"is_error": "true"}}},
			Activity: &agentdef.CLIActivity{
				When: map[string]string{"type": "assistant"}, TypeField: "message.content.0.type",
				ReportTypes: []string{"tool_use"}, DiscardTypes: []string{"thinking"},
			},
			TerminalTypes: []string{"result"},
		},
	}
	llm, err := agentcli.New(agentcli.Config{
		Command: os.Args[0], Provider: provider, Profile: agentdef.Profile{Model: "fake-model"},
		Workspace:  domain.Workspace{WorkingDirectory: dir, Projects: []domain.Project{{Name: "workspace", Path: dir}}},
		WorkingDir: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	return llm
}

// A durable job keeps its progress projection fresh from the descriptor's own
// activity selection. Only declared report types reach the reporter, and the
// reported value is always one the descriptor named, never CLI text.
func TestActivityReporterReceivesDeclaredStepsOnly(t *testing.T) {
	var seen []agentcli.Activity
	ctx := agentcli.WithActivityReporter(t.Context(), func(activity agentcli.Activity) {
		seen = append(seen, activity)
	})

	request := &model.LLMRequest{Contents: []*genai.Content{genai.NewContentFromText(delegate("workspace", "task"), genai.RoleUser)}}
	var text string
	for response, err := range activityLLM(t, "activity").GenerateContent(ctx, request, false) {
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		text = response.Content.Parts[0].Text
	}
	if text != "final text" {
		t.Fatalf("text = %q, want the final text", text)
	}

	if len(seen) == 0 || seen[0].Kind != agentcli.ActivityProcessStarted || seen[0].PID <= 0 {
		t.Fatalf("first activity = %+v, want the started process and its pid", seen)
	}
	steps := seen[1:]
	if len(steps) != 2 {
		t.Fatalf("steps = %+v, want exactly the two declared tool_use events", steps)
	}
	for _, step := range steps {
		if step.Kind != agentcli.ActivityStep || step.Step != "tool_use" {
			t.Fatalf("step = %+v, want a declared tool_use step", step)
		}
	}
}

// An unresolved activity type field is a classification gap, not an invalid
// run. It used to abort a healthy call and lose the result.
func TestUnresolvedActivityTypeFieldKeepsTheResult(t *testing.T) {
	// The "activity" fixture emits an assistant event with no content array,
	// so the declared message.content.0.type path cannot resolve.
	response, err := collect(t, activityLLM(t, "activity"))
	if err != nil {
		t.Fatalf("an unclassifiable activity event must not fail the run: %v", err)
	}
	if response.Content.Parts[0].Text != "final text" {
		t.Fatalf("text = %q, want the final text", response.Content.Parts[0].Text)
	}
}

// An ignored type carries nothing the adapter reads, so a trailing one must not
// be mistaken for an event after the terminal event.
func TestIgnoredTypeAfterTerminalEventIsNotAViolation(t *testing.T) {
	response, err := collect(t, activityLLM(t, "ignored-after-terminal"))
	if err != nil {
		t.Fatalf("a trailing ignored event must not fail the run: %v", err)
	}
	if response.Content.Parts[0].Text != "ok" {
		t.Fatalf("text = %q, want the final text", response.Content.Parts[0].Text)
	}
}
