package acpclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

const (
	defaultMaxFrameBytes          = 8 * 1024 * 1024
	defaultMaxInlineResultBytes   = 64 * 1024
	defaultMaxResultArtifactBytes = 16 * 1024 * 1024
	defaultStderrTailBytes        = 128 * 1024
	processKillGrace              = 5 * time.Second
	promptDrainQuiet              = 250 * time.Millisecond
	jsonRPCVersion                = "2.0"
	defaultProbeTimeout           = 2 * time.Minute
	defaultRunTimeout             = 30 * time.Minute
	recoveryPrompt                = "Inspect the existing repository and remote state for this interrupted task. Do not repeat completed mutations. Complete only missing steps. Return a concise factual result with files, commit, branch, remote, pull request, verification, and unresolved ambiguity."
)

type Bounds struct {
	MaxFrameBytes          int
	MaxInlineResultBytes   int
	MaxResultArtifactBytes int
	StderrTailBytes        int
}

type Client struct {
	executable  string
	args        []string
	bounds      Bounds
	coordinator port.OpenCodeCoordinator
}

var _ port.ExternalAgentRuntime = (*Client)(nil)

func New(executable string, args []string) *Client {
	return NewWithBounds(executable, args, Bounds{})
}

func NewWithBounds(executable string, args []string, bounds Bounds) *Client {
	if bounds.MaxFrameBytes <= 0 {
		bounds.MaxFrameBytes = defaultMaxFrameBytes
	}
	if bounds.MaxInlineResultBytes <= 0 {
		bounds.MaxInlineResultBytes = defaultMaxInlineResultBytes
	}
	if bounds.MaxResultArtifactBytes <= 0 {
		bounds.MaxResultArtifactBytes = defaultMaxResultArtifactBytes
	}
	if bounds.StderrTailBytes <= 0 {
		bounds.StderrTailBytes = defaultStderrTailBytes
	}
	client := &Client{executable: executable, args: append([]string(nil), args...), bounds: bounds}
	return client
}

func NewWithCoordinator(executable string, args []string, coordinator port.OpenCodeCoordinator) *Client {
	client := New(executable, args)
	client.coordinator = coordinator
	return client
}

func NewWithCoordinatorAndBounds(executable string, args []string, coordinator port.OpenCodeCoordinator, bounds Bounds) *Client {
	client := NewWithBounds(executable, args, bounds)
	client.coordinator = coordinator
	return client
}

func (c *Client) Describe(ctx context.Context) (domain.ACPInitResult, error) {
	ctx, cancel := context.WithTimeout(ctx, defaultProbeTimeout)
	defer cancel()
	proc, err := c.start(ctx, "")
	if err != nil {
		return domain.ACPInitResult{}, err
	}
	defer c.terminate(proc)
	return c.initialize(proc)
}

func (c *Client) Probe(ctx context.Context, primaryPath string, configOptions []domain.ACPConfigOption) error {
	ctx, cancel := context.WithTimeout(ctx, defaultProbeTimeout)
	defer cancel()
	if err := validateWorkspacePath(primaryPath); err != nil {
		return err
	}
	proc, err := c.start(ctx, primaryPath)
	if err != nil {
		return err
	}
	defer c.terminate(proc)

	init, err := c.initialize(proc)
	if err != nil {
		return err
	}
	sessionID, _, err := c.newSession(proc, primaryPath)
	if err != nil {
		return fmt.Errorf("session/new: %w", err)
	}
	if err := c.applyConfig(proc, sessionID, configOptions); err != nil {
		return err
	}
	if init.SessionCapabilities.Close {
		if err := c.closeSession(proc, sessionID); err != nil {
			return fmt.Errorf("session/close: %w", err)
		}
	}
	return nil
}

func (c *Client) Run(ctx context.Context, req domain.AcpInvocationRequest) (domain.AcpInvocationResult, error) {
	if c.coordinator != nil {
		release, acquired := c.coordinator.TryInvocation()
		if !acquired {
			return domain.AcpInvocationResult{}, errors.New("OpenCode maintenance is in progress")
		}
		defer release()
	}
	if !utf8.ValidString(req.Task) || containsUnsafeControl(req.Task) || utf8.RuneCountInString(req.Task) > domain.MaxExternalAgentTaskRunes {
		return domain.AcpInvocationResult{}, &domain.ACPError{Code: domain.ACPErrorInvalidInput, Err: errors.New("ACP task is invalid or exceeds the configured character budget")}
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = defaultRunTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := validateWorkspacePath(req.PrimaryPath); err != nil {
		return domain.AcpInvocationResult{}, err
	}
	emitter := &progressEmitter{onProgress: req.OnProgress}
	proc, err := c.start(ctx, req.PrimaryPath)
	if err != nil {
		return domain.AcpInvocationResult{}, fmt.Errorf("acp client start: %w", err)
	}
	defer c.terminate(proc)
	emitter.emit(domain.ACPProgressEvent{Kind: domain.ACPEventProcessStarted, PID: processPID(proc)})

	init, err := c.initialize(proc)
	if err != nil {
		emitter.emitProgressFailure(err)
		return domain.AcpInvocationResult{}, fmt.Errorf("acp initialize: %w", err)
	}
	emitter.emit(domain.ACPProgressEvent{Kind: domain.ACPEventInitializeResponse})
	sessionID, initialConfig, err := c.newSession(proc, req.PrimaryPath)
	if err != nil {
		emitter.emitProgressFailure(err)
		return domain.AcpInvocationResult{}, fmt.Errorf("acp session/new: %w", err)
	}
	emitter.emit(domain.ACPProgressEvent{Kind: domain.ACPEventSessionNew})
	if req.OnSessionCreated != nil {
		if err := req.OnSessionCreated(sessionID); err != nil {
			return domain.AcpInvocationResult{}, err
		}
	}
	if len(req.ConfigOptions) > 0 && len(initialConfig.Options) == 0 {
		return domain.AcpInvocationResult{}, errors.New("ACP session did not advertise configuration options")
	}
	if err := c.applyConfig(proc, sessionID, req.ConfigOptions); err != nil {
		emitter.emitProgressFailure(err)
		return domain.AcpInvocationResult{}, err
	}
	emitter.emit(domain.ACPProgressEvent{Kind: domain.ACPEventTransportActivity})

	prompt := buildPrompt(req.GlobalInstruction, req.AgentInstruction, req.Task)
	emitter.emit(domain.ACPProgressEvent{Kind: domain.ACPEventPromptSent})
	result, err := c.prompt(proc, sessionID, prompt, req.PermissionOptionKind, req.ConfigOptions, req.JobID, req.OnSideEffectsPossible, req.BeforePermission, emitter)
	if err != nil {
		// Host cancellation or timeout is not a process failure: the terminal
		// prompt response (e.g. cancelled) must not be regressed to failed.
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			emitter.emitProgressFailure(err)
		}
		return domain.AcpInvocationResult{}, err
	}
	if init.SessionCapabilities.Close {
		if err := c.closeSession(proc, sessionID); err != nil {
			return domain.AcpInvocationResult{}, fmt.Errorf("acp session/close: %w", err)
		}
		emitter.emit(domain.ACPProgressEvent{Kind: domain.ACPEventTransportActivity})
	}
	return result, nil
}

// progressEmitter forwards content-free progress events to the host-owned
// callback. A nil callback is valid for probes and reconciliation.
type progressEmitter struct {
	onProgress func(domain.ACPProgressEvent)
}

func (e *progressEmitter) emit(event domain.ACPProgressEvent) {
	if e == nil || e.onProgress == nil {
		return
	}
	e.onProgress(event)
}

func (e *progressEmitter) emitProgressFailure(err error) {
	if e == nil || e.onProgress == nil {
		return
	}
	e.emit(domain.ACPProgressEvent{Kind: domain.ACPEventProcessFailed, ErrorClass: acpFailureClass(err)})
}

// acpFailureClass maps a transport error to a bounded host-owned class.
func acpFailureClass(err error) string {
	var acpErr *domain.ACPError
	if errors.As(err, &acpErr) && acpErr.Code != "" {
		return string(acpErr.Code)
	}
	return "acp_prompt_failed"
}

func processPID(proc *process) int {
	if proc == nil || proc.cmd == nil || proc.cmd.Process == nil {
		return 0
	}
	return proc.cmd.Process.Pid
}

// Reconcile negotiates recovery before attempting any session operation. It
// intentionally never sends job.Task: an ambiguous task must not be replayed.
func (c *Client) Reconcile(ctx context.Context, job domain.ExternalAgentJob) (domain.AcpInvocationResult, error) {
	if strings.TrimSpace(job.ACPSessionID) == "" {
		return domain.AcpInvocationResult{}, &domain.ACPError{Code: domain.ACPErrorSessionRecoveryUnsupported, Err: errors.New("session recovery is unsupported because no ACP session ID was persisted")}
	}
	return c.reconcile(ctx, domain.AcpInvocationRequest{JobID: job.ID, Task: ""}, job.ACPSessionID)
}

// ReconcileInvocation is used by the composed job dispatcher after resolving
// trusted project paths. It is separate from Reconcile so durable state never
// stores host paths.
func (c *Client) ReconcileInvocation(ctx context.Context, req domain.AcpInvocationRequest, sessionID string) (domain.AcpInvocationResult, error) {
	return c.reconcile(ctx, req, sessionID)
}

func (c *Client) reconcile(ctx context.Context, req domain.AcpInvocationRequest, sessionID string) (domain.AcpInvocationResult, error) {
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = defaultRunTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	proc, err := c.start(ctx, req.PrimaryPath)
	if err != nil {
		return domain.AcpInvocationResult{}, fmt.Errorf("acp recovery start: %w", err)
	}
	defer c.terminate(proc)
	init, err := c.initialize(proc)
	if err != nil {
		return domain.AcpInvocationResult{}, fmt.Errorf("acp recovery initialize: %w", err)
	}
	if !init.SessionCapabilities.LoadSession && !init.SessionCapabilities.Resume {
		return domain.AcpInvocationResult{}, &domain.ACPError{Code: domain.ACPErrorSessionRecoveryUnsupported, Err: errors.New("session recovery is unsupported by the ACP agent")}
	}
	if err := validateWorkspacePath(req.PrimaryPath); err != nil {
		return domain.AcpInvocationResult{}, err
	}
	method := "session/resume"
	if init.SessionCapabilities.LoadSession {
		method = "session/load"
	}
	params := map[string]any{"sessionId": sessionID, "cwd": req.PrimaryPath, "mcpServers": []any{}}
	loaded, err := c.request(proc, method, params, nil)
	if err != nil {
		return domain.AcpInvocationResult{}, fmt.Errorf("ACP %s: %w", method, err)
	}
	var response sessionResult
	if len(loaded) > 0 && string(loaded) != "null" {
		if err := json.Unmarshal(loaded, &response); err != nil {
			return domain.AcpInvocationResult{}, errors.New("ACP session recovery result is malformed")
		}
	}
	if response.SessionID != "" && response.SessionID != sessionID {
		return domain.AcpInvocationResult{}, errors.New("ACP session recovery returned a different session ID")
	}
	if len(req.ConfigOptions) > 0 && len(response.ConfigOptions) > 0 {
		if err := c.applyConfig(proc, sessionID, req.ConfigOptions); err != nil {
			return domain.AcpInvocationResult{}, err
		}
	}
	permission := req.PermissionOptionKind
	if permission == "" {
		permission = domain.ACPPermissionRejectOnce
	}
	if permission != domain.ACPPermissionRejectOnce && permission != domain.ACPPermissionAllowOnce {
		return domain.AcpInvocationResult{}, errors.New("ACP recovery permission policy is invalid")
	}
	result, err := c.prompt(proc, sessionID, recoveryPrompt, permission, req.ConfigOptions, req.JobID, nil, nil, nil)
	if err != nil {
		return domain.AcpInvocationResult{}, err
	}
	if init.SessionCapabilities.Close {
		if err := c.closeSession(proc, sessionID); err != nil {
			return domain.AcpInvocationResult{}, fmt.Errorf("acp recovery session/close: %w", err)
		}
	}
	return result, nil
}

func validateWorkspacePath(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("ACP workspace path must be absolute")
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("resolve ACP workspace path: %w", err)
	}
	if filepath.Clean(canonical) != filepath.Clean(path) {
		return errors.New("ACP workspace path must be canonical")
	}
	return nil
}

func buildPrompt(globalInstruction, agentInstruction, task string) string {
	return "<<GLOBAL INSTRUCTION (trusted)>>\n" + globalInstruction + "\n\n<<AGENT INSTRUCTION (trusted)>>\n" + agentInstruction + "\n\n<<TASK>>\n" + task
}

type wireMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *wireError      `json:"error,omitempty"`
}

type wireError struct {
	Code int `json:"code"`
}

type process struct {
	ctx         context.Context
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	messages    chan wireMessage
	fatal       chan error
	done        chan error
	writeMu     sync.Mutex
	idMu        sync.Mutex
	nextID      int64
	stderrMu    sync.Mutex
	stderr      *tailBuffer
	stderrBytes int64
}

func (c *Client) start(ctx context.Context, dir string) (*process, error) {
	if strings.TrimSpace(c.executable) == "" {
		return nil, errors.New("ACP executable is empty")
	}
	if dir != "" && !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("ACP working directory must be absolute")
	}
	cmd := exec.Command(c.executable, c.args...)
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(), "OPENCODE_DISABLE_AUTOUPDATE=true")
	configureProcess(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start process: %w", err)
	}

	proc := &process{
		ctx:      ctx,
		cmd:      cmd,
		stdin:    stdin,
		messages: make(chan wireMessage, 16),
		fatal:    make(chan error, 2),
		done:     make(chan error, 1),
		nextID:   1,
		stderr:   newTailBuffer(c.bounds.StderrTailBytes),
	}
	go c.readStdout(proc, stdout)
	go c.drainStderr(proc, stderr)
	go func() { proc.done <- cmd.Wait() }()
	return proc, nil
}

func (c *Client) readStdout(proc *process, stdout io.Reader) {
	reader := bufio.NewReaderSize(stdout, min(c.bounds.MaxFrameBytes+1, 64*1024))
	for {
		frame, err := readFrame(reader, c.bounds.MaxFrameBytes)
		if err != nil {
			if errors.Is(err, io.EOF) {
				proc.reportFatal(&domain.ACPError{Code: domain.ACPErrorProcessExit, Err: errors.New("ACP stdout closed unexpectedly")})
			} else {
				proc.reportFatal(err)
			}
			return
		}
		message, err := decodeFrame(frame)
		if err != nil {
			proc.reportFatal(err)
			return
		}
		select {
		case proc.messages <- message:
		case <-proc.ctx.Done():
			return
		}
	}
}

func (c *Client) drainStderr(proc *process, stderr io.Reader) {
	buffer := make([]byte, 4096)
	for {
		n, err := stderr.Read(buffer)
		if n > 0 {
			proc.stderrMu.Lock()
			proc.stderrBytes += int64(n)
			proc.stderr.Append(buffer[:n])
			proc.stderrMu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func readFrame(reader *bufio.Reader, maxBytes int) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, &domain.ACPError{Code: domain.ACPErrorFrameTooLarge, Err: errors.New("invalid frame bound")}
	}
	frame := make([]byte, 0, min(maxBytes+1, 64*1024))
	for {
		part, err := reader.ReadSlice('\n')
		if len(part) > maxBytes+1 || len(frame)+len(part) > maxBytes+1 {
			return nil, &domain.ACPError{Code: domain.ACPErrorFrameTooLarge, Err: errors.New("ACP frame exceeds configured limit")}
		}
		frame = append(frame, part...)
		if len(frame) > maxBytes+1 {
			return nil, &domain.ACPError{Code: domain.ACPErrorFrameTooLarge, Err: errors.New("ACP frame exceeds configured limit")}
		}
		if err == nil {
			if len(frame) == 0 || frame[len(frame)-1] != '\n' {
				return nil, &domain.ACPError{Code: domain.ACPErrorFrameTooLarge, Err: errors.New("ACP frame is unterminated")}
			}
			frame = bytes.TrimSuffix(frame, []byte{'\n'})
			if len(frame) > 0 && frame[len(frame)-1] == '\r' {
				frame = frame[:len(frame)-1]
			}
			if len(frame) > maxBytes {
				return nil, &domain.ACPError{Code: domain.ACPErrorFrameTooLarge, Err: errors.New("ACP frame exceeds configured limit")}
			}
			return frame, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			if len(frame) > 0 {
				return nil, &domain.ACPError{Code: domain.ACPErrorFrameTooLarge, Err: errors.New("ACP frame is unterminated")}
			}
			return nil, io.EOF
		}
		return nil, &domain.ACPError{Code: domain.ACPErrorMalformedFrame, Err: errors.New("ACP frame read failed")}
	}
}

func decodeFrame(frame []byte) (wireMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(frame))
	var message wireMessage
	if err := decoder.Decode(&message); err != nil {
		return wireMessage{}, &domain.ACPError{Code: domain.ACPErrorMalformedFrame, Err: errors.New("ACP frame is not valid JSON")}
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return wireMessage{}, &domain.ACPError{Code: domain.ACPErrorMalformedFrame, Err: errors.New("ACP frame contains trailing data")}
	}
	if message.JSONRPC == "" {
		return wireMessage{}, &domain.ACPError{Code: domain.ACPErrorProtocolViolation, Err: errors.New("ACP frame has no JSON-RPC version")}
	}
	return message, nil
}

type tailBuffer struct {
	data []byte
	max  int
}

func newTailBuffer(max int) *tailBuffer { return &tailBuffer{max: max} }

func (b *tailBuffer) Append(data []byte) {
	if b == nil || b.max <= 0 {
		return
	}
	if len(data) >= b.max {
		b.data = append(b.data[:0], data[len(data)-b.max:]...)
		return
	}
	if len(b.data)+len(data) > b.max {
		b.data = append([]byte(nil), b.data[len(b.data)+len(data)-b.max:]...)
	}
	b.data = append(b.data, data...)
}

func (p *process) reportFatal(err error) {
	select {
	case p.fatal <- err:
	default:
	}
}

func (c *Client) terminate(proc *process) {
	if proc == nil {
		return
	}
	_ = proc.stdin.Close()
	if proc.ctx.Err() != nil {
		_ = killProcessGroup(proc.cmd)
		<-proc.done
		return
	}
	timer := time.NewTimer(processKillGrace)
	defer timer.Stop()
	select {
	case <-proc.done:
	case <-timer.C:
		_ = killProcessGroup(proc.cmd)
		<-proc.done
	}
}

func (p *process) requestID() int64 {
	p.idMu.Lock()
	defer p.idMu.Unlock()
	id := p.nextID
	p.nextID++
	return id
}

func (p *process) write(value any) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	payload, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal JSON-RPC message: %w", err)
	}
	payload = append(payload, '\n')
	if _, err := p.stdin.Write(payload); err != nil {
		return fmt.Errorf("write JSON-RPC message: %w", err)
	}
	return nil
}

type inboundHandler func(*process, wireMessage) error

func (c *Client) request(proc *process, method string, params any, handler inboundHandler) (json.RawMessage, error) {
	id := proc.requestID()
	if err := proc.write(map[string]any{"jsonrpc": jsonRPCVersion, "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	for {
		select {
		case <-proc.ctx.Done():
			return nil, proc.ctx.Err()
		case err := <-proc.fatal:
			return nil, err
		case message := <-proc.messages:
			if message.JSONRPC != jsonRPCVersion {
				return nil, errors.New("ACP emitted unsupported JSON-RPC version")
			}
			if message.Method != "" {
				if handler != nil {
					if err := handler(proc, message); err != nil {
						return nil, err
					}
					continue
				}
				if len(message.ID) > 0 {
					_ = c.respondError(proc, message.ID, -32601)
					return nil, fmt.Errorf("ACP called unsupported client method %q", message.Method)
				}
				continue
			}
			responseID, err := parseNumericID(message.ID)
			if err != nil || responseID != id {
				return nil, errors.New("ACP response ID does not match request")
			}
			if message.Error != nil {
				return nil, fmt.Errorf("ACP JSON-RPC error code %d", message.Error.Code)
			}
			return message.Result, nil
		}
	}
}

func parseNumericID(raw json.RawMessage) (int64, error) {
	if len(raw) == 0 {
		return 0, errors.New("missing JSON-RPC ID")
	}
	return strconv.ParseInt(string(raw), 10, 64)
}

func (c *Client) respondResult(proc *process, id json.RawMessage, result any) error {
	return proc.write(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  any             `json:"result"`
	}{JSONRPC: jsonRPCVersion, ID: id, Result: result})
}

func (c *Client) respondError(proc *process, id json.RawMessage, code int) error {
	return proc.write(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   map[string]any  `json:"error"`
	}{JSONRPC: jsonRPCVersion, ID: id, Error: map[string]any{"code": code, "message": "unsupported client request"}})
}

func (c *Client) notify(proc *process, method string, params any) error {
	return proc.write(map[string]any{"jsonrpc": jsonRPCVersion, "method": method, "params": params})
}

type initializeResult struct {
	ProtocolVersion json.RawMessage            `json:"protocolVersion"`
	AgentInfo       *domain.ACPAgentInfo       `json:"agentInfo"`
	Capabilities    map[string]json.RawMessage `json:"agentCapabilities"`
}

func (c *Client) initialize(proc *process) (domain.ACPInitResult, error) {
	params := map[string]any{
		"protocolVersion": 1,
		"clientCapabilities": map[string]any{
			"fs":       map[string]bool{"readTextFile": false, "writeTextFile": false},
			"terminal": false,
			"session":  map[string]any{"configOptions": map[string]any{"boolean": map[string]any{}}},
		},
		"clientInfo": map[string]string{"name": domain.ACPClientIdentity, "version": domain.ACPClientVersion},
	}
	result, err := c.request(proc, "initialize", params, nil)
	if err != nil {
		return domain.ACPInitResult{}, err
	}
	var response initializeResult
	if err := json.Unmarshal(result, &response); err != nil {
		return domain.ACPInitResult{}, errors.New("ACP initialize result is malformed")
	}
	version, err := protocolVersion(response.ProtocolVersion)
	if err != nil || version != domain.ACPProtocolVersion {
		return domain.ACPInitResult{}, errors.New("ACP agent does not support protocol version 1")
	}
	if response.AgentInfo == nil || !boundedIdentity(response.AgentInfo.Name) || !boundedIdentity(response.AgentInfo.Version) {
		return domain.ACPInitResult{}, errors.New("ACP agent identity is missing or invalid")
	}

	sessionCaps := capabilityObject(response.Capabilities["sessionCapabilities"])
	loadSession := capabilityEnabled(response.Capabilities["loadSession"]) || capabilityEnabled(sessionCaps["loadSession"])
	return domain.ACPInitResult{
		ProtocolVersion: version,
		AgentInfo:       *response.AgentInfo,
		SessionCapabilities: domain.ACPSessionCapabilities{
			Close:       capabilityEnabled(sessionCaps["close"]),
			LoadSession: loadSession,
			Resume:      capabilityEnabled(sessionCaps["resume"]),
		},
	}, nil
}

func protocolVersion(raw json.RawMessage) (string, error) {
	var number int
	if err := json.Unmarshal(raw, &number); err == nil {
		return strconv.Itoa(number), nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return "", err
	}
	return text, nil
}

func boundedIdentity(value string) bool {
	return strings.TrimSpace(value) != "" && len(value) <= 256 && !strings.ContainsAny(value, "\r\n\x00")
}

// boundedStopReason maps an unknown provider stop reason to the bounded
// allowlist; unrecognized reasons emit as empty (failed phase, no reason).
func boundedStopReason(reason string) string {
	switch reason {
	case domain.ACPStopReasonEndTurn, domain.ACPStopReasonCancelled, domain.ACPStopReasonMaxTokens, domain.ACPStopReasonRefusal:
		return reason
	default:
		return ""
	}
}

func capabilityObject(raw json.RawMessage) map[string]json.RawMessage {
	var result map[string]json.RawMessage
	_ = json.Unmarshal(raw, &result)
	return result
}

func capabilityEnabled(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" || string(raw) == "false" {
		return false
	}
	return true
}

type sessionConfigOption struct {
	ID           string `json:"id"`
	CurrentValue any    `json:"currentValue"`
}

type sessionResult struct {
	SessionID     string                `json:"sessionId"`
	ConfigOptions []sessionConfigOption `json:"configOptions"`
}

func (c *Client) newSession(proc *process, primaryPath string) (string, domain.ACPConfigState, error) {
	params := map[string]any{"cwd": primaryPath, "mcpServers": []any{}}
	result, err := c.request(proc, "session/new", params, nil)
	if err != nil {
		return "", domain.ACPConfigState{}, err
	}
	var response sessionResult
	if err := json.Unmarshal(result, &response); err != nil || !boundedIdentity(response.SessionID) {
		return "", domain.ACPConfigState{}, errors.New("ACP session/new result is malformed")
	}
	state, err := configState(response.ConfigOptions)
	if err != nil {
		return "", domain.ACPConfigState{}, err
	}
	return response.SessionID, state, nil
}

func configState(options []sessionConfigOption) (domain.ACPConfigState, error) {
	state := domain.ACPConfigState{Options: make([]domain.ACPConfigOption, 0, len(options))}
	seen := make(map[string]struct{}, len(options))
	for _, option := range options {
		if !boundedIdentity(option.ID) {
			return domain.ACPConfigState{}, errors.New("ACP config state contains invalid option ID")
		}
		if _, duplicate := seen[option.ID]; duplicate {
			return domain.ACPConfigState{}, errors.New("ACP config state contains duplicate option IDs")
		}
		seen[option.ID] = struct{}{}
		switch option.CurrentValue.(type) {
		case string, bool:
		default:
			return domain.ACPConfigState{}, errors.New("ACP config state contains unsupported value type")
		}
		state.Options = append(state.Options, domain.ACPConfigOption{ID: option.ID, Value: option.CurrentValue})
	}
	return state, nil
}

func (c *Client) applyConfig(proc *process, sessionID string, options []domain.ACPConfigOption) error {
	for index, option := range options {
		params := map[string]any{"sessionId": sessionID, "configId": option.ID, "value": option.Value}
		if _, ok := option.Value.(bool); ok {
			params["type"] = "boolean"
		}
		result, err := c.request(proc, "session/set_config_option", params, nil)
		if err != nil {
			return fmt.Errorf("acp session/set_config_option %q: %w", option.ID, err)
		}
		var response struct {
			ConfigOptions []sessionConfigOption `json:"configOptions"`
		}
		if err := json.Unmarshal(result, &response); err != nil {
			return errors.New("ACP config response is malformed")
		}
		state, err := configState(response.ConfigOptions)
		if err != nil {
			return err
		}
		if err := verifyConfigState(options[:index+1], state); err != nil {
			return fmt.Errorf("ACP config state verification failed: %w", err)
		}
	}
	return nil
}

func verifyConfigState(expected []domain.ACPConfigOption, state domain.ACPConfigState) error {
	actual := make(map[string]any, len(state.Options))
	for _, option := range state.Options {
		actual[option.ID] = option.Value
	}
	for _, option := range expected {
		value, exists := actual[option.ID]
		if !exists || !jsonValuesEqual(option.Value, value) {
			return fmt.Errorf("selected config option %q was not retained", option.ID)
		}
	}
	return nil
}

func jsonValuesEqual(left, right any) bool {
	l, _ := json.Marshal(left)
	r, _ := json.Marshal(right)
	return string(l) == string(r)
}

type promptCollector struct {
	client           *Client
	sessionID        string
	permissionKind   string
	expectedConfig   []domain.ACPConfigOption
	ownerID          string
	onSideEffects    func() error
	beforePermission func() error
	progress         *progressEmitter
	sideEffectsSeen  bool
	messageID        string
	hasMessageID     bool
	lastInputTokens  int
	lastOutputTokens int
	text             strings.Builder
}

func (c *Client) prompt(proc *process, sessionID, text, permissionKind string, expectedConfig []domain.ACPConfigOption, ownerID string, onSideEffects func() error, beforePermission func() error, emitter *progressEmitter) (domain.AcpInvocationResult, error) {
	if ownerID == "" {
		ownerID = "invocation-" + sessionID
	}
	collector := &promptCollector{client: c, sessionID: sessionID, permissionKind: permissionKind, expectedConfig: expectedConfig, ownerID: ownerID, onSideEffects: onSideEffects, beforePermission: beforePermission, progress: emitter}
	result, err := c.request(proc, "session/prompt", map[string]any{
		"sessionId": sessionID,
		"prompt":    []any{map[string]string{"type": "text", "text": text}},
	}, collector.handle)
	if err != nil {
		if proc.ctx.Err() != nil {
			_ = c.notify(proc, "session/cancel", map[string]string{"sessionId": sessionID})
			return domain.AcpInvocationResult{}, proc.ctx.Err()
		}
		return domain.AcpInvocationResult{}, fmt.Errorf("ACP session/prompt: %w", err)
	}
	var response struct {
		StopReason string `json:"stopReason"`
	}
	if err := json.Unmarshal(result, &response); err != nil {
		return domain.AcpInvocationResult{}, errors.New("ACP prompt result is malformed")
	}
	if response.StopReason == domain.ACPStopReasonCancelled && proc.ctx.Err() != nil {
		emitter.emit(domain.ACPProgressEvent{Kind: domain.ACPEventPromptResponse, StopReason: domain.ACPStopReasonCancelled})
		return domain.AcpInvocationResult{}, proc.ctx.Err()
	}
	if response.StopReason != domain.ACPStopReasonEndTurn {
		// The stop reason is bounded before emission so unknown provider
		// reasons can never enter the projection or logs.
		emitter.emit(domain.ACPProgressEvent{Kind: domain.ACPEventPromptResponse, StopReason: boundedStopReason(response.StopReason)})
		return domain.AcpInvocationResult{}, fmt.Errorf("ACP run stopped with reason %q", response.StopReason)
	}
	// Drain trailing updates before emitting the terminal response so a late
	// update can never regress the projection from completed to an earlier
	// phase.
	if err := c.drainPromptUpdates(proc, collector); err != nil {
		return domain.AcpInvocationResult{}, err
	}
	emitter.emit(domain.ACPProgressEvent{Kind: domain.ACPEventPromptResponse, StopReason: domain.ACPStopReasonEndTurn})
	return collector.result(proc.ctx)
}

func (c *Client) drainPromptUpdates(proc *process, collector *promptCollector) error {
	timer := time.NewTimer(promptDrainQuiet)
	defer timer.Stop()
	for {
		select {
		case <-proc.ctx.Done():
			return proc.ctx.Err()
		case err := <-proc.fatal:
			return err
		case message := <-proc.messages:
			if message.JSONRPC != jsonRPCVersion || message.Method == "" {
				return errors.New("ACP emitted an invalid message after prompt completion")
			}
			if err := collector.handle(proc, message); err != nil {
				return err
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(promptDrainQuiet)
		case <-timer.C:
			return nil
		}
	}
}

func (c *promptCollector) handle(proc *process, message wireMessage) error {
	switch message.Method {
	case "session/update":
		return c.handleUpdate(message.Params)
	case "session/request_permission":
		if len(message.ID) == 0 {
			return errors.New("ACP permission request is missing an ID")
		}
		return c.handlePermission(proc, message.ID, message.Params)
	default:
		if len(message.ID) > 0 {
			_ = c.client.respondError(proc, message.ID, -32601)
			return fmt.Errorf("ACP called unsupported client method %q", message.Method)
		}
		return nil
	}
}

func (c *promptCollector) handleUpdate(raw json.RawMessage) error {
	var notification struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			Kind          string                `json:"sessionUpdate"`
			MessageID     string                `json:"messageId"`
			Content       json.RawMessage       `json:"content"`
			ConfigOptions []sessionConfigOption `json:"configOptions"`
			Usage         json.RawMessage       `json:"usage"`
			ToolCallID    string                `json:"toolCallId"`
			ToolKind      string                `json:"kind"`
			ToolStatus    string                `json:"status"`
		} `json:"update"`
	}
	if err := json.Unmarshal(raw, &notification); err != nil || notification.SessionID != c.sessionID {
		return errors.New("ACP session/update is malformed or belongs to another session")
	}
	switch notification.Update.Kind {
	case "tool_call", "tool_call_update":
		if !c.sideEffectsSeen && c.onSideEffects != nil {
			// Tool activity can already have mutated external state before permission; classify it first.
			if err := c.onSideEffects(); err != nil {
				return err
			}
			c.sideEffectsSeen = true
		}
		kind := domain.ACPEventToolCall
		if notification.Update.Kind == "tool_call_update" {
			kind = domain.ACPEventToolCallUpdate
		}
		c.progress.emit(domain.ACPProgressEvent{Kind: kind, Tool: parseToolProgress(
			notification.Update.ToolCallID,
			notification.Update.ToolKind,
			notification.Update.ToolStatus,
			kind == domain.ACPEventToolCall,
		)})
	case "agent_message_chunk":
		var content struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(notification.Update.Content, &content); err != nil || content.Type != "text" {
			return &domain.ACPError{Code: domain.ACPErrorMalformedFrame, Err: errors.New("ACP agent message chunk is malformed")}
		}
		if !utf8.ValidString(content.Text) || containsUnsafeControl(content.Text) {
			return &domain.ACPError{Code: domain.ACPErrorMalformedFrame, Err: errors.New("ACP agent message chunk contains invalid text")}
		}
		if notification.Update.MessageID != "" && c.hasMessageID && notification.Update.MessageID != c.messageID {
			c.text.Reset()
		}
		if notification.Update.MessageID != "" {
			c.messageID = notification.Update.MessageID
			c.hasMessageID = true
		}
		if c.text.Len()+len(content.Text) > c.client.bounds.MaxResultArtifactBytes {
			return &domain.ACPError{Code: domain.ACPErrorResultTooLarge, Err: errors.New("ACP final result exceeds configured artifact limit")}
		}
		c.text.WriteString(content.Text)
		c.progress.emit(domain.ACPProgressEvent{Kind: domain.ACPEventMessageChunk})
	case "agent_thought_chunk":
		// Thought content is discarded immediately and never retained.
		c.progress.emit(domain.ACPProgressEvent{Kind: domain.ACPEventThoughtChunk})
	case "usage_update":
		// Non-increasing usage still refreshes session activity; only
		// increasing bounded counters count as meaningful progress.
		c.progress.emit(domain.ACPProgressEvent{Kind: domain.ACPEventUsageUpdate, UsageIncreased: usageIncreased(c, notification.Update.Usage)})
	case "plan":
		c.progress.emit(domain.ACPProgressEvent{Kind: domain.ACPEventPlan})
	case "config_option_update":
		state, err := configState(notification.Update.ConfigOptions)
		if err != nil {
			return err
		}
		if err := verifyConfigState(c.expectedConfig, state); err != nil {
			return &domain.ACPError{Code: domain.ACPErrorConfigDrift, Err: err}
		}
		c.progress.emit(domain.ACPProgressEvent{Kind: domain.ACPEventConfigOptionUpdate})
	default:
		c.progress.emit(domain.ACPProgressEvent{Kind: domain.ACPEventUnknownNotification})
	}
	return nil
}

// usageIncreased reports whether a valid usage frame strictly increased one
// bounded counter compared with the last seen values. Malformed frames are
// treated as not-increasing (a protocol-safe ignored extension).
func usageIncreased(c *promptCollector, raw json.RawMessage) bool {
	var usage struct {
		InputTokens  int `json:"inputTokens"`
		OutputTokens int `json:"outputTokens"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &usage) != nil || usage.InputTokens < 0 || usage.OutputTokens < 0 {
		return false
	}
	increase := usage.InputTokens > c.lastInputTokens || usage.OutputTokens > c.lastOutputTokens
	c.lastInputTokens, c.lastOutputTokens = usage.InputTokens, usage.OutputTokens
	return increase
}

// parseToolProgress builds bounded tool identity from an ACP v1 wire status.
// ACP v1 statuses (pending, in_progress, completed, failed) map onto the
// bounded internal set; malformed payloads return nil, and the frame still
// counts as session activity.
func parseToolProgress(callID, kind, status string, initial bool) *domain.ACPToolProgress {
	if initial && status == "" {
		status = "pending"
	}
	toolStatus, ok := domain.ACPToolStatusFromWire(status)
	if !ok || callID == "" || len(callID) > 256 {
		return nil
	}
	for _, r := range callID {
		if r < ' ' || r == '\x7f' {
			return nil
		}
	}
	toolKind := domain.ACPToolKind("")
	if initial || kind != "" {
		toolKind = domain.ACPToolKindFromWire(kind)
	}
	return &domain.ACPToolProgress{CallID: callID, Kind: toolKind, Status: toolStatus}
}

func (c *promptCollector) result(ctx context.Context) (domain.AcpInvocationResult, error) {
	finalText := c.text.String()
	if strings.TrimSpace(finalText) == "" {
		return domain.AcpInvocationResult{}, &domain.ACPError{Code: domain.ACPErrorCompletedWithoutFinalText, Err: errors.New("ACP run completed without assistant text")}
	}
	result := domain.AcpInvocationResult{Text: finalText, Inline: true, ResultBytes: int64(len(finalText))}
	return result, nil
}

func containsUnsafeControl(value string) bool {
	return strings.ContainsFunc(value, func(r rune) bool {
		return unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t'
	})
}

func (c *promptCollector) handlePermission(proc *process, id json.RawMessage, raw json.RawMessage) error {
	var request struct {
		SessionID string `json:"sessionId"`
		Options   []struct {
			ID   string `json:"optionId"`
			Kind string `json:"kind"`
		} `json:"options"`
	}
	if err := json.Unmarshal(raw, &request); err != nil || request.SessionID != c.sessionID {
		return errors.New("ACP permission request is malformed or belongs to another session")
	}
	c.progress.emit(domain.ACPProgressEvent{Kind: domain.ACPEventPermissionRequested, PermissionPending: true})
	if proc.ctx.Err() != nil {
		return c.client.respondResult(proc, id, map[string]any{"outcome": map[string]string{"outcome": "cancelled"}})
	}
	selected := ""
	for _, option := range request.Options {
		if option.Kind != c.permissionKind {
			continue
		}
		if !boundedIdentity(option.ID) || selected != "" {
			return errors.New("ACP permission options are ambiguous or malformed")
		}
		selected = option.ID
	}
	if selected == "" {
		return &domain.ACPError{Code: domain.ACPErrorPermissionUnavailable, Err: fmt.Errorf("ACP permission request does not offer %s", c.permissionKind)}
	}
	if c.permissionKind == domain.ACPPermissionAllowOnce && c.beforePermission != nil {
		if err := c.beforePermission(); err != nil {
			return err
		}
	}
	if err := c.client.respondResult(proc, id, map[string]any{"outcome": map[string]string{"outcome": "selected", "optionId": selected}}); err != nil {
		return err
	}
	c.progress.emit(domain.ACPProgressEvent{Kind: domain.ACPEventPermissionResponded, PermissionPending: false})
	return nil
}

func (c *Client) closeSession(proc *process, sessionID string) error {
	_, err := c.request(proc, "session/close", map[string]string{"sessionId": sessionID}, nil)
	return err
}
