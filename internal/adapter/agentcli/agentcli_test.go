package agentcli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/Dauno/slack-local-agent/internal/adapter/agentcli"
	"github.com/Dauno/slack-local-agent/internal/cliprotocol"
	"github.com/Dauno/slack-local-agent/internal/domain"
)

// TestHelperProcess is re-executed by the adapter as a fake cli-v1 shim. When
// run as part of the normal suite (no "--" separator) it is a no-op.
func TestHelperProcess(t *testing.T) {
	args := helperArgs()
	if args == nil {
		return
	}
	os.Exit(runFakeShim(args))
}

func helperArgs() []string {
	for index, arg := range os.Args {
		if arg == "--" {
			return os.Args[index+1:]
		}
	}
	return nil
}

func runFakeShim(args []string) int {
	params := map[string]string{}
	for _, arg := range args {
		key, value, ok := strings.Cut(arg, "=")
		if ok {
			params[key] = value
		}
	}
	stdinData, _ := io.ReadAll(os.Stdin)
	if dump := params["dump"]; dump != "" {
		payload, _ := json.Marshal(map[string]any{"argv": os.Args, "stdin": string(stdinData)})
		_ = os.WriteFile(dump, payload, 0o644)
	}
	req, err := cliprotocol.DecodeRequest(bytes.TrimSpace(stdinData))
	id := "unknown"
	if err == nil {
		id = req.ID
	}
	emit := func(resp cliprotocol.Response) {
		line, _ := cliprotocol.EncodeLine(resp)
		_, _ = os.Stdout.Write(line)
	}
	switch params["mode"] {
	case "describe":
		emit(cliprotocol.NewDescription(id, "fake", "v0.0.1", "1.17.20", []string{"text"}))
	case "validated":
		emit(cliprotocol.NewValidated(id))
	case "result", "":
		emit(cliprotocol.NewResult(id, "final text from shim"))
	case "activity_result":
		emit(cliprotocol.NewActivity(id, "tool", "bash", "completed"))
		emit(cliprotocol.NewResult(id, "done after activity"))
	case "invalid_activity":
		emit(cliprotocol.NewActivity(id, "tool", "bash", "running"))
		emit(cliprotocol.NewResult(id, "must not be accepted"))
	case "stderr_result":
		_, _ = os.Stderr.WriteString("diagnostic noise\n")
		emit(cliprotocol.NewResult(id, "ok with stderr"))
	case "error":
		emit(cliprotocol.NewError(id, cliprotocol.CodeProcessFailed, "opencode exited early", false))
	case "diagnostic_error":
		_, _ = os.Stderr.WriteString("SECRET-FILE-CONTENT-IN-STDERR\n")
		emit(cliprotocol.NewError(id, cliprotocol.CodeProcessFailed, "SECRET-FILE-CONTENT-IN-MESSAGE", false))
	case "malformed":
		_, _ = os.Stdout.WriteString("{not valid json\n")
	case "continued_writer":
		_, _ = os.Stdout.WriteString("{not valid json\n")
		for {
			if _, err := os.Stdout.WriteString(strings.Repeat("x", 64<<10)); err != nil {
				return 0
			}
		}
	case "two_terminals":
		emit(cliprotocol.NewResult(id, "first"))
		emit(cliprotocol.NewResult(id, "second"))
	case "no_terminal":
		return 0
	case "no_terminal_nonzero":
		return 3
	case "oversized":
		_, _ = os.Stdout.WriteString("{\"x\":\"" + strings.Repeat("A", 8192) + "\"}\n")
	case "wrong_id":
		resp := cliprotocol.NewResult("some-other-id", "text")
		emit(resp)
	case "hang":
		time.Sleep(30 * time.Second)
	default:
		return 9
	}
	return 0
}

// --- test helpers ---

type testOptions struct {
	mode         string
	dump         string
	limits       domain.ContextLimits
	maxLineBytes int
	profileModel string
}

func newTestLLM(t *testing.T, opts testOptions) *agentcli.LLM {
	t.Helper()
	dir := t.TempDir()
	model := opts.profileModel
	if model == "" {
		model = "anthropic/model-name"
	}
	args := []string{"-test.run=^TestHelperProcess$", "--", "mode=" + opts.mode}
	if opts.dump != "" {
		args = append(args, "dump="+opts.dump)
	}
	llm, err := agentcli.New(agentcli.Config{
		Command: os.Args[0],
		Args:    args,
		Profile: cliprotocol.Profile{Model: model, Agent: "build", Approval: cliprotocol.ApprovalAuto},
		Workspace: cliprotocol.Workspace{
			WorkingDirectory: dir,
			Projects:         []cliprotocol.Project{{Name: "workspace", Path: dir}},
		},
		ContextLimits: opts.limits,
		WorkingDir:    dir,
		MaxLineBytes:  opts.maxLineBytes,
	})
	if err != nil {
		t.Fatalf("construct agentcli LLM: %v", err)
	}
	return llm
}

func userRequest(texts ...string) *model.LLMRequest {
	contents := make([]*genai.Content, 0, len(texts))
	for index, text := range texts {
		var role genai.Role = genai.RoleUser
		if index%2 == 1 {
			role = genai.RoleModel
		}
		contents = append(contents, genai.NewContentFromText(text, role))
	}
	return &model.LLMRequest{Contents: contents}
}

func collect(t *testing.T, llm *agentcli.LLM, ctx context.Context, request *model.LLMRequest) (*model.LLMResponse, error) {
	t.Helper()
	var (
		last *model.LLMResponse
		err  error
	)
	for resp, genErr := range llm.GenerateContent(ctx, request, false) {
		if genErr != nil {
			err = genErr
			break
		}
		last = resp
	}
	return last, err
}

// --- tests ---

func TestGenerateContentSuccess(t *testing.T) {
	llm := newTestLLM(t, testOptions{mode: "result"})
	resp, err := collect(t, llm, context.Background(), userRequest("hello"))
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if resp == nil || resp.Content == nil || len(resp.Content.Parts) != 1 {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if resp.Content.Parts[0].Text != "final text from shim" {
		t.Fatalf("text = %q", resp.Content.Parts[0].Text)
	}
	if resp.FinishReason != genai.FinishReasonStop {
		t.Fatalf("finish reason = %q", resp.FinishReason)
	}
	if resp.Content.Role != genai.RoleModel {
		t.Fatalf("role = %q", resp.Content.Role)
	}
}

func TestActivityBeforeResultIsIgnored(t *testing.T) {
	llm := newTestLLM(t, testOptions{mode: "activity_result"})
	resp, err := collect(t, llm, context.Background(), userRequest("hi"))
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if resp.Content.Parts[0].Text != "done after activity" {
		t.Fatalf("text = %q", resp.Content.Parts[0].Text)
	}
}

func TestInvalidActivityIsProtocolError(t *testing.T) {
	llm := newTestLLM(t, testOptions{mode: "invalid_activity"})
	_, err := collect(t, llm, context.Background(), userRequest("hi"))
	var shimErr *agentcli.ShimError
	if err == nil || !asShim(err, &shimErr) || shimErr.Code != cliprotocol.CodeProtocolError {
		t.Fatalf("expected protocol_error for invalid activity, got %v", err)
	}
}

func TestStderrIsCapturedNotFatal(t *testing.T) {
	llm := newTestLLM(t, testOptions{mode: "stderr_result"})
	resp, err := collect(t, llm, context.Background(), userRequest("hi"))
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if resp.Content.Parts[0].Text != "ok with stderr" {
		t.Fatalf("text = %q", resp.Content.Parts[0].Text)
	}
}

func TestTerminalErrorSurfaced(t *testing.T) {
	llm := newTestLLM(t, testOptions{mode: "error"})
	_, err := collect(t, llm, context.Background(), userRequest("hi"))
	var shimErr *agentcli.ShimError
	if err == nil || !asShim(err, &shimErr) {
		t.Fatalf("expected ShimError, got %v", err)
	}
	if shimErr.Code != cliprotocol.CodeProcessFailed {
		t.Fatalf("code = %q", shimErr.Code)
	}
}

func TestChildDiagnosticsAreNotExposed(t *testing.T) {
	llm := newTestLLM(t, testOptions{mode: "diagnostic_error"})
	_, err := collect(t, llm, context.Background(), userRequest("hi"))
	if err == nil {
		t.Fatal("expected shim error")
	}
	if strings.Contains(err.Error(), "SECRET-FILE-CONTENT") {
		t.Fatalf("child-controlled diagnostic leaked: %v", err)
	}
	if !strings.Contains(err.Error(), "omitted") {
		t.Fatalf("safe diagnostic metadata missing: %v", err)
	}
}

func TestMalformedOutputIsProtocolError(t *testing.T) {
	llm := newTestLLM(t, testOptions{mode: "malformed"})
	_, err := collect(t, llm, context.Background(), userRequest("hi"))
	var shimErr *agentcli.ShimError
	if err == nil || !asShim(err, &shimErr) || shimErr.Code != cliprotocol.CodeProtocolError {
		t.Fatalf("expected protocol_error, got %v", err)
	}
}

func TestProtocolFailureTerminatesContinuedWriter(t *testing.T) {
	llm := newTestLLM(t, testOptions{mode: "continued_writer"})
	done := make(chan error, 1)
	go func() {
		_, err := collect(t, llm, context.Background(), userRequest("hi"))
		done <- err
	}()
	select {
	case err := <-done:
		var shimErr *agentcli.ShimError
		if err == nil || !asShim(err, &shimErr) || shimErr.Code != cliprotocol.CodeProtocolError {
			t.Fatalf("expected protocol_error, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("protocol failure deadlocked while child continued writing")
	}
}

func TestMultipleTerminalsRejected(t *testing.T) {
	llm := newTestLLM(t, testOptions{mode: "two_terminals"})
	_, err := collect(t, llm, context.Background(), userRequest("hi"))
	var shimErr *agentcli.ShimError
	if err == nil || !asShim(err, &shimErr) || shimErr.Code != cliprotocol.CodeProtocolError {
		t.Fatalf("expected protocol_error for duplicate terminal, got %v", err)
	}
}

func TestNoTerminalCleanExit(t *testing.T) {
	llm := newTestLLM(t, testOptions{mode: "no_terminal"})
	_, err := collect(t, llm, context.Background(), userRequest("hi"))
	var shimErr *agentcli.ShimError
	if err == nil || !asShim(err, &shimErr) || shimErr.Code != cliprotocol.CodeNoResponse {
		t.Fatalf("expected no_response, got %v", err)
	}
}

func TestNoTerminalNonZeroExit(t *testing.T) {
	llm := newTestLLM(t, testOptions{mode: "no_terminal_nonzero"})
	_, err := collect(t, llm, context.Background(), userRequest("hi"))
	var shimErr *agentcli.ShimError
	if err == nil || !asShim(err, &shimErr) || shimErr.Code != cliprotocol.CodeProcessFailed {
		t.Fatalf("expected process_failed, got %v", err)
	}
}

func TestOversizedLineRejected(t *testing.T) {
	llm := newTestLLM(t, testOptions{mode: "oversized", maxLineBytes: 1024})
	_, err := collect(t, llm, context.Background(), userRequest("hi"))
	var shimErr *agentcli.ShimError
	if err == nil || !asShim(err, &shimErr) || shimErr.Code != cliprotocol.CodeProtocolError {
		t.Fatalf("expected protocol_error for oversized line, got %v", err)
	}
}

func TestMismatchedIDRejected(t *testing.T) {
	llm := newTestLLM(t, testOptions{mode: "wrong_id"})
	_, err := collect(t, llm, context.Background(), userRequest("hi"))
	var shimErr *agentcli.ShimError
	if err == nil || !asShim(err, &shimErr) || shimErr.Code != cliprotocol.CodeProtocolError {
		t.Fatalf("expected protocol_error for mismatched id, got %v", err)
	}
}

func TestStreamingRejected(t *testing.T) {
	llm := newTestLLM(t, testOptions{mode: "result"})
	var err error
	for _, genErr := range llm.GenerateContent(context.Background(), userRequest("hi"), true) {
		err = genErr
		break
	}
	if err != agentcli.ErrStreamingUnsupported {
		t.Fatalf("expected streaming rejection, got %v", err)
	}
}

func TestToolsRejectedBeforeLaunch(t *testing.T) {
	llm := newTestLLM(t, testOptions{mode: "result"})
	request := userRequest("hi")
	request.Config = &genai.GenerateContentConfig{
		Tools: []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{{Name: "do_thing"}}}},
	}
	_, err := collect(t, llm, context.Background(), request)
	if err != agentcli.ErrToolsUnsupported {
		t.Fatalf("expected tools rejection, got %v", err)
	}
}

func TestRequestToolsRejectedBeforeLaunch(t *testing.T) {
	dump := filepath.Join(t.TempDir(), "dump.json")
	llm := newTestLLM(t, testOptions{mode: "result", dump: dump})
	request := userRequest("hi")
	request.Tools = map[string]any{"sandbox": struct{}{}}
	_, err := collect(t, llm, context.Background(), request)
	if !errors.Is(err, agentcli.ErrToolsUnsupported) {
		t.Fatalf("expected request.Tools rejection, got %v", err)
	}
	if _, statErr := os.Stat(dump); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("shim launched before request.Tools rejection: %v", statErr)
	}
}

func TestEveryUnsupportedGenerateConfigFieldRejectedBeforeLaunch(t *testing.T) {
	configType := reflect.TypeOf(genai.GenerateContentConfig{})
	for index := 0; index < configType.NumField(); index++ {
		field := configType.Field(index)
		switch field.Name {
		case "SystemInstruction", "ResponseMIMEType", "Tools", "ToolConfig":
			continue
		}
		t.Run(field.Name, func(t *testing.T) {
			dump := filepath.Join(t.TempDir(), "dump.json")
			llm := newTestLLM(t, testOptions{mode: "result", dump: dump})
			cfg := &genai.GenerateContentConfig{}
			setNonZero(reflect.ValueOf(cfg).Elem().FieldByName(field.Name))
			request := userRequest("hi")
			request.Config = cfg
			_, err := collect(t, llm, context.Background(), request)
			if !errors.Is(err, agentcli.ErrUnsupportedConfig) {
				t.Fatalf("expected unsupported config rejection for %s, got %v", field.Name, err)
			}
			if _, statErr := os.Stat(dump); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("shim launched before %s rejection: %v", field.Name, statErr)
			}
		})
	}
}

func TestTextPlainAndSystemInstructionRemainSupported(t *testing.T) {
	llm := newTestLLM(t, testOptions{mode: "result"})
	request := userRequest("hi")
	request.Config = &genai.GenerateContentConfig{
		SystemInstruction: genai.NewContentFromText("system", genai.RoleUser),
		ResponseMIMEType:  "text/plain",
	}
	if _, err := collect(t, llm, context.Background(), request); err != nil {
		t.Fatalf("documented text defaults should remain supported: %v", err)
	}
}

func TestFunctionHistoryRejected(t *testing.T) {
	llm := newTestLLM(t, testOptions{mode: "result"})
	request := &model.LLMRequest{Contents: []*genai.Content{
		{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "1", Name: "x"}}}},
		genai.NewContentFromText("continue", genai.RoleUser),
	}}
	_, err := collect(t, llm, context.Background(), request)
	if err == nil {
		t.Fatalf("expected function history rejection")
	}
}

func TestNonUserFinalTurnRejected(t *testing.T) {
	llm := newTestLLM(t, testOptions{mode: "result"})
	request := &model.LLMRequest{Contents: []*genai.Content{
		genai.NewContentFromText("assistant only", genai.RoleModel),
	}}
	_, err := collect(t, llm, context.Background(), request)
	if err != agentcli.ErrNoUserTurn {
		t.Fatalf("expected ErrNoUserTurn, got %v", err)
	}
}

func TestUserTextOnlyInStdinNeverArgv(t *testing.T) {
	dump := filepath.Join(t.TempDir(), "dump.json")
	llm := newTestLLM(t, testOptions{mode: "result", dump: dump})
	secret := "SENSITIVE-USER-PROMPT-9f3a"
	if _, err := collect(t, llm, context.Background(), userRequest(secret)); err != nil {
		t.Fatalf("generate: %v", err)
	}
	captured := readDump(t, dump)
	argvJSON, _ := json.Marshal(captured["argv"])
	if strings.Contains(string(argvJSON), secret) {
		t.Fatalf("user text leaked into argv: %s", argvJSON)
	}
	if !strings.Contains(captured["stdin"].(string), secret) {
		t.Fatalf("user text missing from stdin payload")
	}
}

func TestRequestCarriesProfileInstructionAndProjects(t *testing.T) {
	dump := filepath.Join(t.TempDir(), "dump.json")
	llm := newTestLLM(t, testOptions{mode: "result", dump: dump})
	request := userRequest("hello")
	request.Config = &genai.GenerateContentConfig{
		SystemInstruction: genai.NewContentFromText("You are Dev Agent.", genai.RoleUser),
	}
	if _, err := collect(t, llm, context.Background(), request); err != nil {
		t.Fatalf("generate: %v", err)
	}
	captured := readDump(t, dump)
	var req cliprotocol.Request
	if err := json.Unmarshal([]byte(captured["stdin"].(string)), &req); err != nil {
		t.Fatalf("decode captured request: %v", err)
	}
	if req.SystemInstruction != "You are Dev Agent." {
		t.Fatalf("system instruction = %q", req.SystemInstruction)
	}
	if req.Profile == nil || req.Profile.Model != "anthropic/model-name" || req.Profile.Agent != "build" {
		t.Fatalf("profile not forwarded: %+v", req.Profile)
	}
	if req.Workspace == nil || len(req.Workspace.Projects) != 1 || req.Workspace.Projects[0].Name != "workspace" {
		t.Fatalf("workspace not forwarded: %+v", req.Workspace)
	}
}

func TestLongSessionBoundedBeforeLaunch(t *testing.T) {
	dump := filepath.Join(t.TempDir(), "dump.json")
	llm := newTestLLM(t, testOptions{
		mode:   "result",
		dump:   dump,
		limits: domain.ContextLimits{MaxMessages: 3, MaxChars: 40},
	})
	texts := []string{}
	for i := 0; i < 20; i++ {
		texts = append(texts, strings.Repeat("x", 30))
	}
	// Ensure the final turn is a user message.
	if len(texts)%2 == 0 {
		texts = append(texts, strings.Repeat("y", 30))
	}
	if _, err := collect(t, llm, context.Background(), userRequest(texts...)); err != nil {
		t.Fatalf("generate: %v", err)
	}
	captured := readDump(t, dump)
	var req cliprotocol.Request
	if err := json.Unmarshal([]byte(captured["stdin"].(string)), &req); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if len(req.Messages) > 3 {
		t.Fatalf("expected <= 3 bounded messages, got %d", len(req.Messages))
	}
	total := 0
	for _, message := range req.Messages {
		total += len([]rune(message.Text))
	}
	if total > 40 {
		t.Fatalf("expected <= 40 code points, got %d", total)
	}
	if req.Messages[len(req.Messages)-1].Role != cliprotocol.RoleUser {
		t.Fatalf("final bounded message must be user")
	}
}

func TestCancellationTerminatesShim(t *testing.T) {
	llm := newTestLLM(t, testOptions{mode: "hang"})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := collect(t, llm, ctx, userRequest("hi"))
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("expected cancellation error")
	}
	if elapsed > 10*time.Second {
		t.Fatalf("cancellation did not terminate shim promptly: %v", elapsed)
	}
	var shimErr *agentcli.ShimError
	if !asShim(err, &shimErr) || shimErr.Code != cliprotocol.CodeTimeout {
		t.Fatalf("expected timeout ShimError, got %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline cause was not preserved: %v", err)
	}
}

func TestDescribeExchange(t *testing.T) {
	llm := newTestLLM(t, testOptions{mode: "describe"})
	resp, err := llm.Describe(context.Background())
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if resp.Type != cliprotocol.TypeDescription || resp.CLIVersion != "1.17.20" {
		t.Fatalf("unexpected describe response: %+v", resp)
	}
}

func TestValidateExchange(t *testing.T) {
	llm := newTestLLM(t, testOptions{mode: "validated"})
	if err := llm.Validate(context.Background()); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func TestResolveCommandSelf(t *testing.T) {
	resolved, err := agentcli.ResolveCommand(agentcli.SelfCommand)
	if err != nil {
		t.Fatalf("resolve self: %v", err)
	}
	if !filepath.IsAbs(resolved) {
		t.Fatalf("expected absolute path, got %q", resolved)
	}
	if _, err := agentcli.ResolveCommand("definitely-not-a-real-binary-xyz"); err == nil {
		t.Fatalf("expected lookup failure for missing binary")
	}
}

// --- utilities ---

func readDump(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read dump: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("decode dump: %v", err)
	}
	return out
}

func asShim(err error, target **agentcli.ShimError) bool {
	for err != nil {
		if shim, ok := err.(*agentcli.ShimError); ok {
			*target = shim
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}

func setNonZero(value reflect.Value) {
	switch value.Kind() {
	case reflect.Pointer:
		value.Set(reflect.New(value.Type().Elem()))
	case reflect.Interface:
		value.Set(reflect.ValueOf(map[string]any{"unsupported": true}))
	case reflect.Map:
		value.Set(reflect.MakeMap(value.Type()))
		value.SetMapIndex(reflect.Zero(value.Type().Key()), reflect.Zero(value.Type().Elem()))
	case reflect.Slice:
		value.Set(reflect.MakeSlice(value.Type(), 1, 1))
	case reflect.String:
		value.SetString("unsupported")
	case reflect.Bool:
		value.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		value.SetInt(1)
	default:
		panic("unsupported GenerateContentConfig field kind: " + value.Kind().String())
	}
}
