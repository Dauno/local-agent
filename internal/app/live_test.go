package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/agentdef"
)

// The auth command comes from the descriptor, so a provider that declares none
// must say so plainly instead of running a guessed command.
func TestCheckAuthenticationRequiresADeclaredCommand(t *testing.T) {
	checker := cliProviderChecker{}
	cases := map[string]*agentdef.ResolvedModel{
		"no resolved model": nil,
		"no auth block":     {Provider: agentdef.Provider{Name: "codex", Executable: "codex"}},
		"empty auth command": {Provider: agentdef.Provider{
			Name: "codex", Executable: "codex", Auth: &agentdef.CLIAuth{},
		}},
		"no executable": {Provider: agentdef.Provider{
			Name: "codex", Auth: &agentdef.CLIAuth{Command: []string{"login", "status"}},
		}},
	}
	for name, resolved := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := checker.CheckAuthentication(context.Background(), resolved, "codex"); err == nil {
				t.Fatal("a provider with no declared auth command must be rejected")
			}
		})
	}
}

// The executable is resolved through PATH and executed directly, never through
// a shell, so a provider name that looks like a shell command cannot run one.
func TestCheckAuthenticationNeverUsesAShell(t *testing.T) {
	resolved := &agentdef.ResolvedModel{Provider: agentdef.Provider{
		Name: "codex", Executable: "codex;rm -rf /",
		Auth: &agentdef.CLIAuth{Command: []string{"login", "status"}},
	}}
	_, err := cliProviderChecker{}.CheckAuthentication(context.Background(), resolved, "codex")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("error = %v, want a PATH lookup failure rather than execution", err)
	}
}

func TestLiveAudioCheckUsesDedicatedTranscriptionEndpoint(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		paths = append(paths, request.Method+" "+request.URL.Path)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"text":"probe"}`))
	}))
	t.Cleanup(server.Close)

	err := (liveChecker{}).CheckAudioTranscription(t.Context(), &agentdef.ResolvedModel{
		Provider:  agentdef.Provider{Type: agentdef.ProviderTypeOpenAICompatible},
		BaseURL:   server.URL + "/v1",
		APIKeyEnv: "STT_API_KEY",
		Model:     "stt-model",
	}, "probe-key")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != "POST /v1/audio/transcriptions" {
		t.Fatalf("live probe paths = %v", paths)
	}
}
