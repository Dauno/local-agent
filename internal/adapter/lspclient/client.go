package lspclient

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

const maxFrameBytes = 4 << 20

type Server struct {
	ID        string
	Path      string
	SHA256    string
	Args      []string
	Languages []string
}

type Config struct {
	Servers        []Server
	Routes         map[string][]string
	ProjectRoots   map[string]string
	Readers        map[string]port.CodeReader
	ResultStore    port.RecoverableResultStore
	MaxProcesses   int
	InitTimeout    time.Duration
	RequestTimeout time.Duration
}

type Client struct {
	config  Config
	sem     chan struct{}
	metrics port.MetricRecorder
}

func New(config Config) (*Client, error) {
	if config.MaxProcesses <= 0 || config.InitTimeout <= 0 || config.RequestTimeout <= 0 {
		return nil, errors.New("LSP runtime requires positive process and timeout limits")
	}
	return &Client{config: config, sem: make(chan struct{}, config.MaxProcesses)}, nil
}

func (c *Client) WithMetrics(recorder port.MetricRecorder) *Client {
	if c != nil {
		c.metrics = recorder
	}
	return c
}

func (c *Client) Symbols(ctx context.Context, req domain.SymbolRequest) (domain.SymbolResult, error) {
	raw, source, err := c.query(ctx, req.Project, req.Path, req.Actor, req.ConversationKey, "textDocument/documentSymbol", map[string]any{
		"textDocument": map[string]any{"uri": c.documentURI(req.Project, req.Path)},
	})
	if err != nil {
		return domain.SymbolResult{}, err
	}
	locations, err := c.decodeSymbols(raw, source)
	if err != nil {
		return domain.SymbolResult{}, err
	}
	limit := clampResults(req.MaxResults)
	result := domain.SymbolResult{TotalCount: len(locations)}
	if len(locations) > limit {
		result.ResultRef, err = c.storeOverflow(ctx, req.Actor, req.ConversationKey, "lsp_symbols", locations)
		if err != nil {
			return domain.SymbolResult{}, err
		}
		locations, result.Truncated = locations[:limit], true
	}
	result.Symbols = locations
	return result, nil
}

func (c *Client) Definition(ctx context.Context, req domain.LocationRequest) (domain.LocationResult, error) {
	return c.locations(ctx, req, "textDocument/definition", map[string]any{})
}

func (c *Client) References(ctx context.Context, req domain.LocationRequest) (domain.LocationResult, error) {
	return c.locations(ctx, req, "textDocument/references", map[string]any{"context": map[string]any{"includeDeclaration": true}})
}

func (c *Client) locations(ctx context.Context, req domain.LocationRequest, method string, extra map[string]any) (domain.LocationResult, error) {
	params := map[string]any{
		"textDocument": map[string]any{"uri": c.documentURI(req.Project, req.Path)},
		"position":     map[string]any{"line": max(req.Line-1, 0), "character": max(req.Column-1, 0)},
	}
	maps.Copy(params, extra)
	raw, source, err := c.query(ctx, req.Project, req.Path, req.Actor, req.ConversationKey, method, params)
	if err != nil {
		return domain.LocationResult{}, err
	}
	locations, err := c.decodeLocations(ctx, raw, source, req.Actor, req.ConversationKey)
	if err != nil {
		return domain.LocationResult{}, err
	}
	limit := clampResults(req.MaxResults)
	result := domain.LocationResult{TotalCount: len(locations)}
	if len(locations) > limit {
		result.ResultRef, err = c.storeOverflow(ctx, req.Actor, req.ConversationKey, "lsp_locations", locations)
		if err != nil {
			return domain.LocationResult{}, err
		}
		locations, result.Truncated = locations[:limit], true
	}
	result.Locations = locations
	return result, nil
}

type sourceSnapshot struct {
	project string
	path    string
	digest  string
	content string
}

func (c *Client) query(ctx context.Context, project, path, actor, conversation, method string, params map[string]any) (result json.RawMessage, snapshot sourceSnapshot, err error) {
	reader := c.config.Readers[project]
	root := c.config.ProjectRoots[project]
	if reader == nil || root == "" {
		return nil, sourceSnapshot{}, errors.New("project is unavailable")
	}
	source, err := reader.ReadRange(ctx, domain.SourceRangeRequest{Project: project, Path: path, StartLine: 1, MaxLines: 10_000, Actor: actor, ConversationKey: conversation})
	if err != nil || source.Truncated || !source.EOF {
		return nil, sourceSnapshot{}, errors.New("source is unavailable for LSP inspection")
	}
	language := languageForPath(path)
	servers := c.serversFor(language)
	if len(servers) == 0 {
		if c.metrics != nil {
			c.metrics.SetGauge(domain.MetricLSPServerState, 0, port.MetricLabels{"language": language})
		}
		return nil, sourceSnapshot{}, errors.New("language server is unavailable")
	}
	if c.metrics != nil {
		c.metrics.SetGauge(domain.MetricLSPServerState, 1, port.MetricLabels{"language": language})
	}
	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		return nil, sourceSnapshot{}, ctx.Err()
	}
	snapshot = sourceSnapshot{project: project, path: path, digest: source.Location.FileSHA256, content: source.Content}
	var lastErr error
	for index, server := range servers {
		started := time.Now()
		raw, queryErr := c.run(ctx, server, root, path, language, source.Content, method, params)
		if c.metrics != nil {
			labels := port.MetricLabels{"language": language, "lsp_server_id": server.ID}
			c.metrics.AddCounter(domain.MetricLSPRequestTotal, 1, labels)
			c.metrics.Observe(domain.MetricLSPRequestDuration, time.Since(started).Seconds(), labels)
		}
		if queryErr == nil {
			return raw, snapshot, nil
		}
		if c.metrics != nil && index+1 < len(servers) {
			c.metrics.AddCounter(domain.MetricLSPFallbackTotal, 1, port.MetricLabels{"language": language})
		}
		lastErr = queryErr
	}
	return nil, sourceSnapshot{}, fmt.Errorf("language server is unavailable: %w", lastErr)
}

func (c *Client) run(ctx context.Context, server Server, root, path, language, content, method string, params map[string]any) (json.RawMessage, error) {
	if err := verifyBinary(server.Path, server.SHA256); err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithTimeout(ctx, c.config.InitTimeout+c.config.RequestTimeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, server.Path, server.Args...)
	cmd.Env = allowedEnvironment()
	cmd.Dir = root
	configureProcess(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	finished := make(chan struct{})
	processWaited := false
	defer func() {
		if !processWaited {
			killProcessGroup(cmd)
			_ = stdin.Close()
			_ = cmd.Wait()
		}
		close(finished)
	}()
	go func() {
		select {
		case <-runCtx.Done():
			killProcessGroup(cmd)
		case <-finished:
		}
	}()
	go func() { _, _ = io.Copy(io.Discard, stderr) }()
	reader := bufio.NewReader(stdout)
	rootURI := fileURI(root)
	initialize := map[string]any{"processId": nil, "rootUri": rootURI, "capabilities": map[string]any{}, "workspaceFolders": []any{map[string]any{"uri": rootURI, "name": filepath.Base(root)}}}
	if err := writeRequest(stdin, 1, "initialize", initialize); err != nil {
		return nil, err
	}
	initializeResult, err := readResponse(reader, stdin, 1)
	if err != nil {
		return nil, err
	}
	if !supportsMethod(initializeResult, method) {
		return nil, fmt.Errorf("language server does not advertise %s", method)
	}
	if err := writeNotification(stdin, "initialized", map[string]any{}); err != nil {
		return nil, err
	}
	if err := writeNotification(stdin, "textDocument/didOpen", map[string]any{"textDocument": map[string]any{
		"uri": fileURI(filepath.Join(root, filepath.FromSlash(path))), "languageId": language, "version": 1, "text": content,
	}}); err != nil {
		return nil, err
	}
	if err := writeRequest(stdin, 2, method, params); err != nil {
		return nil, err
	}
	result, err := readResponse(reader, stdin, 2)
	_ = writeRequest(stdin, 3, "shutdown", nil)
	_, _ = readResponse(reader, stdin, 3)
	_ = writeNotification(stdin, "exit", nil)
	_ = stdin.Close()
	waitErr := cmd.Wait()
	processWaited = true
	if err == nil && waitErr != nil && runCtx.Err() != nil {
		err = runCtx.Err()
	}
	return result, err
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func readResponse(reader *bufio.Reader, writer io.Writer, wanted int) (json.RawMessage, error) {
	for {
		body, err := readFrame(reader)
		if err != nil {
			return nil, err
		}
		var message rpcMessage
		if err := json.Unmarshal(body, &message); err != nil || message.JSONRPC != "2.0" {
			return nil, errors.New("invalid LSP JSON-RPC message")
		}
		if message.Method != "" && len(message.ID) > 0 {
			_ = writeRPC(writer, map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(message.ID), "error": map[string]any{"code": -32601, "message": "method not allowed"}})
			continue
		}
		id, _ := strconv.Atoi(string(message.ID))
		if id != wanted {
			continue
		}
		if message.Error != nil {
			return nil, fmt.Errorf("LSP request failed with code %d", message.Error.Code)
		}
		return message.Result, nil
	}
}

func readFrame(reader *bufio.Reader) ([]byte, error) {
	length := -1
	for {
		lineBytes, err := reader.ReadSlice('\n')
		if err != nil {
			return nil, err
		}
		if len(lineBytes) > 8192 {
			return nil, errors.New("LSP header exceeds limit")
		}
		line := strings.TrimSpace(string(lineBytes))
		if line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			length, err = strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return nil, errors.New("invalid LSP content length")
			}
		}
	}
	if length < 0 || length > maxFrameBytes {
		return nil, errors.New("invalid LSP frame size")
	}
	body := make([]byte, length)
	_, err := io.ReadFull(reader, body)
	return body, err
}

func writeRequest(writer io.Writer, id int, method string, params any) error {
	return writeRPC(writer, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
}

func writeNotification(writer io.Writer, method string, params any) error {
	return writeRPC(writer, map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func writeRPC(writer io.Writer, value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(writer, "Content-Length: %d\r\n\r\n%s", len(body), body)
	return err
}

func (c *Client) serversFor(language string) []Server {
	byID := make(map[string]Server, len(c.config.Servers))
	for _, server := range c.config.Servers {
		byID[server.ID] = server
	}
	var result []Server
	for _, id := range c.config.Routes[language] {
		if server, ok := byID[id]; ok {
			result = append(result, server)
		}
	}
	if len(result) > 0 {
		return result
	}
	for _, server := range c.config.Servers {
		for _, current := range server.Languages {
			if current == language {
				result = append(result, server)
			}
		}
	}
	return result
}

func (c *Client) documentURI(project, path string) string {
	return fileURI(filepath.Join(c.config.ProjectRoots[project], filepath.FromSlash(path)))
}

func (c *Client) decodeSymbols(raw json.RawMessage, source sourceSnapshot) ([]domain.CodeSymbol, error) {
	var symbols []struct {
		Name     string   `json:"name"`
		Kind     int      `json:"kind"`
		Range    lspRange `json:"range"`
		Location *struct {
			URI   string   `json:"uri"`
			Range lspRange `json:"range"`
		} `json:"location"`
	}
	if string(raw) == "null" {
		return nil, nil
	}
	if err := json.Unmarshal(raw, &symbols); err != nil {
		return nil, errors.New("invalid LSP symbol result")
	}
	result := make([]domain.CodeSymbol, 0, len(symbols))
	for _, symbol := range symbols {
		location := symbol.Range
		path := source.path
		if symbol.Location != nil {
			location = symbol.Location.Range
			resolved, err := c.relativeLocation(source.project, symbol.Location.URI)
			if err != nil {
				continue
			}
			path = resolved
		}
		if path != source.path {
			continue
		}
		result = append(result, domain.CodeSymbol{Name: symbol.Name, Kind: strconv.Itoa(symbol.Kind), Location: toCodeLocation(source, path, location)})
	}
	return result, nil
}

type lspRange struct {
	Start struct{ Line, Character int } `json:"start"`
	End   struct{ Line, Character int } `json:"end"`
}

func (c *Client) decodeLocations(ctx context.Context, raw json.RawMessage, source sourceSnapshot, actor, conversation string) ([]domain.CodeLocation, error) {
	if string(raw) == "null" {
		return nil, nil
	}
	var entries []struct {
		URI         string   `json:"uri"`
		Range       lspRange `json:"range"`
		TargetURI   string   `json:"targetUri"`
		TargetRange lspRange `json:"targetRange"`
	}
	if len(raw) > 0 && raw[0] == '{' {
		raw = append(append([]byte{'['}, raw...), ']')
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, errors.New("invalid LSP location result")
	}
	result := make([]domain.CodeLocation, 0, len(entries))
	snapshots := map[string]sourceSnapshot{source.path: source}
	for _, entry := range entries {
		uri, currentRange := entry.URI, entry.Range
		if entry.TargetURI != "" {
			uri, currentRange = entry.TargetURI, entry.TargetRange
		}
		path, err := c.relativeLocation(source.project, uri)
		if err != nil {
			continue
		}
		target, ok := snapshots[path]
		if !ok {
			loaded, readErr := c.readSnapshot(ctx, source.project, path, actor, conversation)
			if readErr != nil {
				continue
			}
			target = loaded
			snapshots[path] = loaded
		}
		result = append(result, toCodeLocation(target, path, currentRange))
	}
	return result, nil
}

func (c *Client) readSnapshot(ctx context.Context, project, path, actor, conversation string) (sourceSnapshot, error) {
	reader := c.config.Readers[project]
	if reader == nil {
		return sourceSnapshot{}, errors.New("project is unavailable")
	}
	result, err := reader.ReadRange(ctx, domain.SourceRangeRequest{Project: project, Path: path, StartLine: 1, MaxLines: 10_000, Actor: actor, ConversationKey: conversation})
	if err != nil || result.Truncated || !result.EOF {
		return sourceSnapshot{}, errors.New("source is unavailable for LSP inspection")
	}
	return sourceSnapshot{project: project, path: path, digest: result.Location.FileSHA256, content: result.Content}, nil
}

func (c *Client) storeOverflow(ctx context.Context, actor, conversation, kind string, value any) (string, error) {
	if c.config.ResultStore == nil {
		return "", errors.New("recoverable result store is unavailable")
	}
	encoded, err := domain.CanonicalJSON(value)
	if err != nil {
		return "", errors.New("encode recoverable LSP result")
	}
	stored, err := c.config.ResultStore.Put(ctx, port.PutResultRequest{Actor: actor, ConversationKey: conversation, Kind: kind, Content: string(encoded)})
	if err != nil {
		return "", errors.New("persist recoverable LSP result")
	}
	return stored.Ref, nil
}

func (c *Client) relativeLocation(project, uri string) (string, error) {
	parsed, err := url.Parse(uri)
	if err != nil || parsed.Scheme != "file" {
		return "", errors.New("unsupported LSP location")
	}
	root, err := filepath.Abs(c.config.ProjectRoots[project])
	if err != nil {
		return "", err
	}
	path, err := filepath.Abs(filepath.FromSlash(parsed.Path))
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("LSP location escapes project")
	}
	return filepath.ToSlash(relative), nil
}

func toCodeLocation(source sourceSnapshot, path string, value lspRange) domain.CodeLocation {
	return domain.CodeLocation{
		Project: source.project, Path: path, StartByte: byteOffsetUTF16(source.content, value.Start.Line, value.Start.Character),
		EndByte: byteOffsetUTF16(source.content, value.End.Line, value.End.Character), StartLine: value.Start.Line + 1,
		EndLine: value.End.Line + 1, Language: languageForPath(path), FileSHA256: source.digest,
	}
}

func byteOffsetUTF16(content string, line, character int) int64 {
	if line < 0 || character < 0 {
		return 0
	}
	offset, currentLine := 0, 0
	for offset < len(content) && currentLine < line {
		if content[offset] == '\n' {
			currentLine++
		}
		offset++
	}
	units := 0
	for offset < len(content) && content[offset] != '\n' {
		r, size := utf8.DecodeRuneInString(content[offset:])
		width := utf16.RuneLen(r)
		if units+width > character {
			break
		}
		units += width
		offset += size
	}
	return int64(offset)
}

func supportsMethod(initializeResult json.RawMessage, method string) bool {
	var result struct {
		Capabilities map[string]json.RawMessage `json:"capabilities"`
	}
	if json.Unmarshal(initializeResult, &result) != nil {
		return false
	}
	key := map[string]string{"textDocument/documentSymbol": "documentSymbolProvider", "textDocument/definition": "definitionProvider", "textDocument/references": "referencesProvider"}[method]
	value := result.Capabilities[key]
	return len(value) > 0 && string(value) != "false" && string(value) != "null"
}

func fileURI(path string) string {
	absolute, _ := filepath.Abs(path)
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(absolute)}).String()
}

func languageForPath(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return "go"
	case ".js", ".jsx":
		return "javascript"
	case ".ts", ".tsx":
		return "typescript"
	case ".rs":
		return "rust"
	default:
		return strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
	}
}

func clampResults(value int) int {
	if value <= 0 || value > 200 {
		return 200
	}
	return value
}

func verifyBinary(path, expected string) error {
	if expected == "" {
		return errors.New("language server digest is required")
	}
	file, err := os.Open(path)
	if err != nil {
		return errors.New("language server binary is unavailable")
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil || !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), expected) {
		return errors.New("language server binary digest changed")
	}
	return nil
}

func allowedEnvironment() []string {
	keys := []string{"PATH", "HOME", "USER", "TMPDIR", "LANG", "LC_ALL", "GOROOT", "GOPATH", "GOMODCACHE", "GOCACHE", "GOENV", "GOFLAGS", "GOPROXY", "GONOSUMDB", "GOPRIVATE"}
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		if value := os.Getenv(key); value != "" {
			result = append(result, key+"="+value)
		}
	}
	return result
}

var _ port.CodeIntelligence = (*Client)(nil)
