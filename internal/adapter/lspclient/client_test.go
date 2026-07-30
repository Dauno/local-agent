package lspclient

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
)

type sourceReader struct{ digest string }

func (r sourceReader) ReadRange(context.Context, domain.SourceRangeRequest) (domain.SourceRange, error) {
	return domain.SourceRange{Location: domain.CodeLocation{FileSHA256: r.digest}, Content: "package sample\nfunc Hello() {}\n", EOF: true}, nil
}

type pathSourceReader struct{}

func (pathSourceReader) ReadRange(_ context.Context, req domain.SourceRangeRequest) (domain.SourceRange, error) {
	content := "package sample\nfunc Hello() {}\n"
	return domain.SourceRange{Location: domain.CodeLocation{Path: req.Path, FileSHA256: req.Path + "-digest"}, Content: content, EOF: true}, nil
}

type recordingResultStore struct {
	put port.PutResultRequest
}

func (s *recordingResultStore) Put(_ context.Context, req port.PutResultRequest) (domain.RecoverableResult, error) {
	s.put = req
	return domain.RecoverableResult{Ref: "result-ref"}, nil
}

func (*recordingResultStore) ReadChunk(context.Context, domain.ResultChunkRequest) (domain.ResultChunk, error) {
	return domain.ResultChunk{}, nil
}

func (*recordingResultStore) Stat(context.Context, port.StatResultRequest) (domain.RecoverableResult, error) {
	return domain.RecoverableResult{}, nil
}

func (*recordingResultStore) DeleteExpired(context.Context, time.Time, int) (int, error) {
	return 0, nil
}

func TestClientNegotiatesAndReturnsDigestBoundSymbols(t *testing.T) {
	root := t.TempDir()
	binary, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	binaryDigest := fmt.Sprintf("%x", sha256.Sum256(binary))
	client, err := New(Config{
		Servers: []Server{{ID: "fake", Path: os.Args[0], SHA256: binaryDigest, Args: []string{"-test.run=TestLSPHelperProcess", "--", "lsp-helper"}, Languages: []string{"go"}}},
		Routes:  map[string][]string{"go": {"fake"}}, ProjectRoots: map[string]string{"workspace": root},
		Readers: map[string]port.CodeReader{"workspace": sourceReader{digest: "digest"}}, MaxProcesses: 1,
		InitTimeout: time.Second, RequestTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Symbols(context.Background(), domain.SymbolRequest{Project: "workspace", Path: "main.go", MaxResults: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Symbols) != 1 || result.Symbols[0].Name != "Hello" || result.Symbols[0].Location.FileSHA256 != "digest" || result.Symbols[0].Location.Path != "main.go" {
		t.Fatalf("symbols = %#v", result.Symbols)
	}
	if _, err := client.Definition(context.Background(), domain.LocationRequest{Project: "workspace", Path: "main.go", Line: 2, Column: 6}); err == nil {
		t.Fatal("definition should fail when capability is not advertised")
	}
}

func TestClientExternalizesTruncatedSymbolResults(t *testing.T) {
	root := t.TempDir()
	binary, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	resultStore := &recordingResultStore{}
	client, err := New(Config{
		Servers: []Server{{ID: "fake", Path: os.Args[0], SHA256: fmt.Sprintf("%x", sha256.Sum256(binary)), Args: []string{"-test.run=TestLSPHelperProcess", "--", "lsp-helper"}, Languages: []string{"go"}}},
		Routes:  map[string][]string{"go": {"fake"}}, ProjectRoots: map[string]string{"workspace": root},
		Readers: map[string]port.CodeReader{"workspace": sourceReader{digest: "digest"}}, ResultStore: resultStore, MaxProcesses: 1,
		InitTimeout: time.Second, RequestTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Symbols(t.Context(), domain.SymbolRequest{Project: "workspace", Path: "many.go", MaxResults: 2, Actor: "U123", ConversationKey: "conversation"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Truncated || len(result.Symbols) != 2 || result.TotalCount != 201 || result.ResultRef != "result-ref" {
		t.Fatalf("symbol result = %#v", result)
	}
	if resultStore.put.Actor != "U123" || resultStore.put.ConversationKey != "conversation" || resultStore.put.Kind != "lsp_symbols" || resultStore.put.Content == "" {
		t.Fatalf("stored result = %#v", resultStore.put)
	}
}

func TestDecodeLocationsReadsAndBindsCrossFileSnapshot(t *testing.T) {
	root := t.TempDir()
	client := &Client{config: Config{ProjectRoots: map[string]string{"workspace": root}, Readers: map[string]port.CodeReader{"workspace": pathSourceReader{}}}}
	raw, err := json.Marshal([]map[string]any{{
		"uri":   fileURI(filepath.Join(root, "other.go")),
		"range": map[string]any{"start": map[string]any{"line": 1, "character": 0}, "end": map[string]any{"line": 1, "character": 4}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	locations, err := client.decodeLocations(t.Context(), raw, sourceSnapshot{project: "workspace", path: "main.go", digest: "main-digest", content: "package sample\n"}, "U123", "conversation")
	if err != nil {
		t.Fatal(err)
	}
	if len(locations) != 1 || locations[0].Path != "other.go" || locations[0].FileSHA256 != "other.go-digest" {
		t.Fatalf("locations = %#v", locations)
	}
}

func TestLSPHelperProcess(t *testing.T) {
	if len(os.Args) == 0 || os.Args[len(os.Args)-1] != "lsp-helper" {
		return
	}
	reader := bufio.NewReader(os.Stdin)
	for {
		body, err := readFrame(reader)
		if err != nil {
			os.Exit(0)
		}
		var message rpcMessage
		if json.Unmarshal(body, &message) != nil {
			os.Exit(2)
		}
		switch message.Method {
		case "initialize":
			_ = writeRPC(os.Stdout, map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"capabilities": map[string]any{"documentSymbolProvider": true}}})
		case "textDocument/documentSymbol":
			uri := fileURI(filepath.Join(filepath.Dir(filepath.Join("/", "unused")), "unused"))
			var params struct {
				TextDocument struct {
					URI string `json:"uri"`
				} `json:"textDocument"`
			}
			_ = json.Unmarshal(extractParams(body), &params)
			if params.TextDocument.URI != "" {
				uri = params.TextDocument.URI
			}
			symbols := []any{map[string]any{
				"name": "Hello", "kind": 12, "location": map[string]any{"uri": uri, "range": map[string]any{
					"start": map[string]any{"line": 1, "character": 0}, "end": map[string]any{"line": 1, "character": 15},
				}},
			}}
			if strings.Contains(uri, "many.go") {
				symbols = make([]any, 201)
				for index := range symbols {
					symbols[index] = map[string]any{"name": fmt.Sprintf("Symbol%d", index), "kind": 12, "location": map[string]any{"uri": uri, "range": map[string]any{
						"start": map[string]any{"line": 1, "character": 0}, "end": map[string]any{"line": 1, "character": 4},
					}}}
				}
			}
			_ = writeRPC(os.Stdout, map[string]any{"jsonrpc": "2.0", "id": 2, "result": symbols})
		case "shutdown":
			_ = writeRPC(os.Stdout, map[string]any{"jsonrpc": "2.0", "id": 3, "result": nil})
		case "exit":
			os.Exit(0)
		}
	}
}

func extractParams(body []byte) json.RawMessage {
	var request struct {
		Params json.RawMessage `json:"params"`
	}
	_ = json.Unmarshal(body, &request)
	return request.Params
}
