package agentcli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"

	"github.com/Dauno/slack-local-agent/internal/agentdef"
	"github.com/Dauno/slack-local-agent/internal/domain"
)

type semanticVersion struct{ major, minor, patch int }

func parseVersion(value string) (semanticVersion, bool) {
	var version semanticVersion
	if _, err := fmt.Sscanf(strings.TrimSpace(value), "%d.%d.%d", &version.major, &version.minor, &version.patch); err != nil {
		return semanticVersion{}, false
	}
	if version.major < 0 || version.minor < 0 || version.patch < 0 {
		return semanticVersion{}, false
	}
	return version, true
}

func compareVersion(left, right semanticVersion) int {
	if left.major != right.major {
		return left.major - right.major
	}
	if left.minor != right.minor {
		return left.minor - right.minor
	}
	return left.patch - right.patch
}

func (l *LLM) probeVersion(ctx context.Context) (string, error) {
	version := l.provider.Version
	output, err := l.capture(ctx, l.command, version.Command)
	if err != nil {
		return "", err
	}
	pattern, err := regexp.Compile(version.Pattern)
	if err != nil {
		return "", &CLIError{Code: CodeProcessFailed, Message: "descriptor version.pattern is invalid", Cause: err}
	}
	match := pattern.FindStringSubmatch(output)
	index := pattern.SubexpIndex("version")
	if match == nil || index < 0 || index >= len(match) {
		return "", &CLIError{Code: CodeProcessFailed, Message: "descriptor version.pattern did not resolve version"}
	}
	installed, ok := parseVersion(match[index])
	if !ok {
		return "", &CLIError{Code: CodeProcessFailed, Message: "descriptor version.pattern captured an invalid version"}
	}
	minimum, _ := parseVersion(version.Min)
	maximum, hasMaximum := parseVersion(version.Max)
	if compareVersion(installed, minimum) < 0 || (hasMaximum && compareVersion(installed, maximum) > 0) {
		rangeText := ">=" + version.Min
		if version.Max != "" {
			rangeText += " and <=" + version.Max
		}
		return "", &CLIError{Code: CodeUnsupported, Message: fmt.Sprintf("installed CLI version %s is outside accepted range %s", match[index], rangeText)}
	}
	return match[index], nil
}

// capture runs a probe in the process working directory. Only project-neutral
// probes belong here; anything that depends on the selected workspace must use
// captureIn.
func (l *LLM) capture(ctx context.Context, command string, args []string) (string, error) {
	return l.captureIn(ctx, l.workingDir, command, args)
}

func (l *LLM) captureIn(ctx context.Context, workingDir, command string, args []string) (string, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = workingDir
	cmd.Env = os.Environ()
	configureProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = defaultWaitDelay
	stdout := &limitedCapture{limit: l.maxStdoutBytes}
	stderr := &limitedCapture{limit: l.maxStderrBytes}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", &CLIError{Code: CodeTimeout, Message: "agent CLI probe cancelled", Retryable: errors.Is(ctxErr, context.DeadlineExceeded), Cause: ctxErr}
	}
	if stdout.truncated {
		return "", &CLIError{Code: CodeProcessFailed, Message: fmt.Sprintf("agent CLI probe stdout exceeded %d bytes", l.maxStdoutBytes)}
	}
	if err != nil {
		return "", classifyProcessError(command, err, stderr.summary())
	}
	return string(stdout.data), nil
}

func (l *LLM) exchange(ctx context.Context, request request) (string, error) {
	// Preconditions are checked here, not at startup, because they describe the
	// selected project. The Git-worktree check that Codex needs cannot be
	// answered before the caller names a workspace.
	if err := l.checkPreconditions(ctx, request.workingDir); err != nil {
		return "", err
	}
	systemPrompt, prompt := l.renderRequest(request)
	args, err := l.buildArgs(systemPrompt, request.workingDir)
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, l.command, args...)
	cmd.Dir, cmd.Env = request.workingDir, os.Environ()
	configureProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = defaultWaitDelay
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", fmt.Errorf("open agent CLI stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("open agent CLI stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", fmt.Errorf("open agent CLI stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return "", classifyStartError(l.command, err)
	}
	report := activityReporterFrom(ctx)
	if report != nil && cmd.Process != nil {
		report(Activity{Kind: ActivityProcessStarted, PID: cmd.Process.Pid})
	}
	go func() {
		_, _ = io.WriteString(stdin, prompt)
		_ = stdin.Close()
	}()
	stderrCh := make(chan diagnosticSummary, 1)
	go func() { stderrCh <- readDiagnostic(stderr, l.maxStderrBytes) }()
	text, readErr := l.readStdout(stdout, report)
	if readErr != nil {
		_ = killProcessGroup(cmd)
		_, _ = io.Copy(io.Discard, stdout)
	}
	diagnostic := <-stderrCh
	waitErr := cmd.Wait()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", &CLIError{Code: CodeTimeout, Message: "agent CLI call cancelled", Retryable: errors.Is(ctxErr, context.DeadlineExceeded), Cause: ctxErr}
	}
	if readErr != nil {
		return "", l.annotateProtocol(readErr, diagnostic)
	}
	if waitErr != nil {
		return "", classifyProcessError(l.command, waitErr, diagnostic)
	}
	return text, nil
}

func (l *LLM) buildArgs(systemPrompt, workingDir string) ([]string, error) {
	invocation := l.provider.Invocation
	values := map[string]string{"model": l.profile.Model, "agent": l.profile.Agent, "approval": l.profile.Approval, "variant": l.profile.Variant, "workdir": workingDir}
	var prefix, suffix []string
	for _, name := range sortedOptionNames(invocation.Options) {
		option := invocation.Options[name]
		value := values[name]
		if len(option.Values) > 0 {
			mapping, ok := option.Values[value]
			if !ok {
				return nil, &CLIError{Code: CodeInvalidRequest, Message: "descriptor invocation.options." + name + " has no mapping for profile value"}
			}
			for key, mapped := range mapping.Substitutions {
				values[key] = mapped
			}
			if option.Position == "prefix" {
				prefix = append(prefix, mapping.Args...)
			} else {
				suffix = append(suffix, mapping.Args...)
			}
			continue
		}
		if value == "" {
			continue
		}
		var args []string
		if option.Flag != "" {
			args = []string{option.Flag, value}
		} else {
			args = substituteArgs(option.Template, map[string]string{"value": value})
		}
		if option.Position == "prefix" {
			prefix = append(prefix, args...)
		} else {
			suffix = append(suffix, args...)
		}
	}
	args := append([]string{}, prefix...)
	if workspace := invocation.Workspace; workspace != nil && workspace.CWDFlag != "" {
		args = append(args, workspace.CWDFlag, workingDir)
	}
	args = append(args, substituteArgs(invocation.ArgsPrefix, values)...)
	if workspace := invocation.Workspace; workspace != nil && workspace.AddDirFlag != "" && conditionMatches(workspace.AddDirWhen, values) {
		seen := map[string]bool{}
		for _, project := range l.workspace.Projects {
			if project.Path != workingDir && !seen[project.Path] {
				args = append(args, workspace.AddDirFlag, project.Path)
				seen[project.Path] = true
			}
		}
	}
	args = append(args, substituteArgs(invocation.Args, values)...)
	args = append(args, suffix...)
	// The system prompt is host-owned text, never user text, so it is the one
	// content value allowed on argv. The transcript always stays on stdin.
	if system := invocation.SystemPrompt; system != nil && systemPrompt != "" {
		args = append(args, system.Flag, systemPrompt)
	}
	return args, nil
}

// renderRequest splits host-owned content from user-controlled content.
//
// When the descriptor declares a native system-prompt channel, the agent
// instruction and the project registry travel through it and stdin carries only
// the transcript. Without a channel the host content is prepended to the stdin
// prompt, because the CLI offers nowhere else to put it.
func (l *LLM) renderRequest(request request) (systemPrompt, prompt string) {
	host := buildHostContext(request.systemInstruction, l.workspace)
	if l.provider.Invocation.SystemPrompt != nil {
		return host, buildPrompt("", request.messages)
	}
	return "", buildPrompt(host, request.messages)
}

func sortedOptionNames(options map[string]agentdef.CLIInvocationOption) []string {
	names := make([]string, 0, len(options))
	for name := range options {
		names = append(names, name)
	}
	sort.Strings(names)
	// Profile values change substitutions before templates use them.
	for index, name := range names {
		if len(options[name].Values) > 0 {
			copy(names[1:index+1], names[:index])
			names[0] = name
			break
		}
	}
	return names
}

func (l *LLM) readStdout(reader io.Reader, report ActivityReporter) (string, error) {
	stream := l.provider.Stream
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64<<10), l.maxLineBytes)
	total := 0
	terminal := false
	failed := false
	final := ""
	for scanner.Scan() {
		line := scanner.Bytes()
		total += len(line) + 1
		if total > l.maxStdoutBytes {
			return "", &ProtocolViolation{Reason: fmt.Sprintf("stdout exceeded %d bytes", l.maxStdoutBytes)}
		}
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var event any
		if err := json.Unmarshal(line, &event); err != nil {
			return "", &ProtocolViolation{Reason: "malformed NDJSON line"}
		}
		// An ignored type carries nothing this adapter reads. It is skipped
		// before every other rule, so a trailing bookkeeping event cannot be
		// mistaken for an event after the terminal one.
		if eventType, found := stringAt(event, "type"); found && contains(stream.IgnoreTypes, eventType) {
			continue
		}
		if terminal {
			return "", &ProtocolViolation{Reason: "event after terminal event"}
		}
		if matches(event, stream.Failure.WhenAny) {
			failed = true
		}
		if stream.Activity != nil && matchesOne(event, stream.Activity.When) {
			// Activity is observational. An unresolved type field means this
			// event cannot be classified, never that the run is invalid, so
			// the result is still read.
			kind, found := stringAt(event, stream.Activity.TypeField)
			switch {
			case !found:
				if l.logger != nil {
					l.logger.Debug("agent CLI activity type field did not resolve",
						"type_field", stream.Activity.TypeField)
				}
			case contains(stream.Activity.DiscardTypes, kind):
				continue
			case contains(stream.Activity.ReportTypes, kind):
				if l.logger != nil {
					l.logger.Debug("agent CLI native activity", "kind", kind, "status", "completed")
				}
				if report != nil {
					report(Activity{Kind: ActivityStep, Step: kind})
				}
			}
		}
		if matchesOne(event, stream.FinalText.When) {
			text, found := stringAt(event, stream.FinalText.Path)
			if !found {
				return "", &ProtocolViolation{Reason: "descriptor stream.final_text.path did not resolve"}
			}
			if strings.TrimSpace(text) != "" {
				final = text
			}
		}
		typeName, _ := stringAt(event, "type")
		if contains(stream.TerminalTypes, typeName) {
			terminal = true
		}
	}
	if err := scanner.Err(); err != nil {
		return "", &ProtocolViolation{Reason: fmt.Sprintf("agent CLI emitted a line longer than %d bytes", l.maxLineBytes)}
	}
	if failed {
		return "", &CLIError{Code: CodeProcessFailed, Message: "agent CLI reported a failed turn"}
	}
	if !terminal || strings.TrimSpace(final) == "" {
		return "", &CLIError{Code: CodeNoResponse, Message: "agent CLI exited without final text"}
	}
	return strings.TrimSpace(final), nil
}

func matches(event any, conditions []map[string]string) bool {
	for _, condition := range conditions {
		if matchesOne(event, condition) {
			return true
		}
	}
	return false
}
func matchesOne(event any, condition map[string]string) bool {
	for path, want := range condition {
		got, ok := valueAt(event, path)
		if !ok || fmt.Sprint(got) != want {
			return false
		}
	}
	return true
}
func conditionMatches(condition, values map[string]string) bool {
	for key, want := range condition {
		if values[key] != want {
			return false
		}
	}
	return true
}
func stringAt(event any, path string) (string, bool) {
	value, ok := valueAt(event, path)
	if !ok {
		return "", false
	}
	text, ok := value.(string)
	return text, ok
}
func valueAt(value any, path string) (any, bool) {
	for _, part := range strings.Split(path, ".") {
		switch current := value.(type) {
		case map[string]any:
			var ok bool
			value, ok = current[part]
			if !ok {
				return nil, false
			}
		case []any:
			var index int
			if _, err := fmt.Sscanf(part, "%d", &index); err != nil || index < 0 || index >= len(current) {
				return nil, false
			}
			value = current[index]
		default:
			return nil, false
		}
	}
	return value, true
}
func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
func substituteArgs(args []string, values map[string]string) []string {
	result := make([]string, len(args))
	for i, arg := range args {
		result[i] = arg
		for key, value := range values {
			result[i] = strings.ReplaceAll(result[i], "{{"+key+"}}", value)
		}
	}
	return result
}

// buildHostContext renders the content local-agent owns: the agent instruction
// and, when it names more than the working directory, the project registry.
//
// No section claims to be trusted. Such a claim is unverifiable wherever this
// text lands, and inside a stdin prompt it actively harms: the text arrives as
// user input, so a passage asserting its own authority has the exact shape of a
// prompt injection. Claude Code flags that shape and appends a warning to the
// answer the root agent receives.
func buildHostContext(instruction string, workspace domain.Workspace) string {
	var builder strings.Builder
	if instruction = strings.TrimSpace(instruction); instruction != "" {
		builder.WriteString(instruction)
	}
	// The working directory already reaches the CLI through the workspace
	// flags. The registry only adds information when it names other projects.
	if len(workspace.Projects) > 1 {
		registry, _ := json.Marshal(workspace)
		if builder.Len() > 0 {
			builder.WriteString("\n\n")
		}
		builder.WriteString("Registered projects: ")
		builder.Write(registry)
	}
	return builder.String()
}

// buildPrompt renders the stdin prompt. hostContext is empty when the
// descriptor declared a native system-prompt channel, because the content went
// there instead.
func buildPrompt(hostContext string, messages []domain.Message) string {
	var builder strings.Builder
	if hostContext = strings.TrimSpace(hostContext); hostContext != "" {
		builder.WriteString(hostContext)
		builder.WriteString("\n\n")
	}
	// A single message is the delegated-leaf case, which is one task and no
	// conversation. Role labels and a closing pointer would only add framing
	// for the CLI to distrust.
	if len(messages) == 1 {
		builder.WriteString(strings.TrimSpace(messages[0].Content))
		return builder.String()
	}
	for _, message := range messages {
		builder.WriteString("[")
		builder.WriteString(string(message.Role))
		builder.WriteString("]\n")
		builder.WriteString(message.Content)
		builder.WriteString("\n\n")
	}
	builder.WriteString("The final message above is the current request. Respond to it.")
	return builder.String()
}

type diagnosticSummary struct {
	bytes, limit int64
	truncated    bool
}
type limitedCapture struct {
	limit     int
	data      []byte
	total     int64
	truncated bool
}

func (c *limitedCapture) Write(data []byte) (int, error) {
	c.total += int64(len(data))
	remaining := c.limit - len(c.data)
	if remaining > 0 {
		if remaining > len(data) {
			remaining = len(data)
		}
		c.data = append(c.data, data[:remaining]...)
	}
	c.truncated = c.total > int64(c.limit)
	return len(data), nil
}
func (c *limitedCapture) summary() diagnosticSummary {
	return diagnosticSummary{bytes: c.total, limit: int64(c.limit), truncated: c.truncated}
}
func readDiagnostic(reader io.Reader, limit int) diagnosticSummary {
	capture := &limitedCapture{limit: limit}
	_, _ = io.Copy(capture, reader)
	return capture.summary()
}
func classifyStartError(command string, err error) error {
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		return &CLIError{Code: CodeExecutableMissing, Message: fmt.Sprintf("agent CLI executable %q not found or not runnable", command)}
	}
	return &CLIError{Code: CodeProcessFailed, Message: fmt.Sprintf("start agent CLI %q failed", command), Cause: err}
}
func classifyProcessError(command string, err error, diagnostic diagnosticSummary) error {
	return &CLIError{Code: CodeProcessFailed, Message: fmt.Sprintf("agent CLI %q failed%s", command, diagnosticSuffix("stderr", diagnostic)), Cause: err}
}
func (l *LLM) annotateProtocol(err error, diagnostic diagnosticSummary) error {
	var violation *ProtocolViolation
	if errors.As(err, &violation) {
		return &CLIError{Code: CodeProcessFailed, Message: l.sanitizeText(violation.Reason) + diagnosticSuffix("stderr", diagnostic)}
	}
	return err
}
func (l *LLM) sanitizeText(text string) string {
	text = strings.Map(func(value rune) rune {
		if value < 0x20 && value != '\t' {
			return ' '
		}
		return value
	}, text)
	if l.sanitize != nil {
		text = l.sanitize(text)
	}
	return strings.TrimSpace(text)
}
func diagnosticSuffix(label string, diagnostic diagnosticSummary) string {
	if diagnostic.bytes == 0 {
		return ""
	}
	if diagnostic.truncated {
		return fmt.Sprintf(" (%s omitted; more than %d bytes)", label, diagnostic.limit)
	}
	return fmt.Sprintf(" (%s omitted; %d bytes)", label, diagnostic.bytes)
}
