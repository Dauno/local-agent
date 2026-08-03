package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Dauno/slack-local-agent/internal/agentdef"
)

func TestCheckAuthenticationRejectsUnknownShimIdentity(t *testing.T) {
	checker := cliProviderChecker{}
	for _, name := range []string{"", "unknown-cli", "opencode;rm -rf /"} {
		_, err := checker.CheckAuthentication(context.Background(), nil, name)
		if err == nil {
			t.Fatalf("shim identity %q must be rejected", name)
		}
		if !strings.Contains(err.Error(), "not supported") {
			t.Fatalf("unexpected error for %q: %v", name, err)
		}
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
