package app

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"iter"
	"net/http"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"

	slackapi "github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/Dauno/slack-local-agent/internal/adapter/acpclient"
	"github.com/Dauno/slack-local-agent/internal/adapter/adkartifact"
	"github.com/Dauno/slack-local-agent/internal/adapter/modelcalllimiter"
	"github.com/Dauno/slack-local-agent/internal/adapter/openaillm"
	"github.com/Dauno/slack-local-agent/internal/adapter/openaistt"
	"github.com/Dauno/slack-local-agent/internal/adapter/tokencounter"
	"github.com/Dauno/slack-local-agent/internal/agentdef"
	"github.com/Dauno/slack-local-agent/internal/config"
	"github.com/Dauno/slack-local-agent/internal/domain"
	"github.com/Dauno/slack-local-agent/internal/port"
	"github.com/Dauno/slack-local-agent/internal/usecase/doctor"
)

type liveChecker struct{}

func (liveChecker) CheckSlackBot(ctx context.Context, botToken string) error {
	response, err := slackapi.New(botToken).AuthTestContext(ctx)
	if err != nil {
		return fmt.Errorf("slack auth.test failed: %w", err)
	}
	if response == nil || response.UserID == "" {
		return errors.New("slack auth.test returned no bot user ID")
	}
	return nil
}

func (liveChecker) CheckSlackApp(ctx context.Context, botToken, appToken string) error {
	api := slackapi.New(botToken, slackapi.OptionAppLevelToken(appToken))
	_, websocketURL, err := socketmode.New(api).OpenContext(ctx)
	if err != nil {
		return fmt.Errorf("slack apps.connections.open failed: %w", err)
	}
	if strings.TrimSpace(websocketURL) == "" {
		return errors.New("slack apps.connections.open returned no WebSocket URL")
	}
	return nil
}

func (liveChecker) CheckSlackContext(ctx context.Context, botToken string) error {
	api := slackapi.New(botToken)
	auth, err := api.AuthTestContext(ctx)
	if err != nil {
		return fmt.Errorf("slack auth.test for context check failed: %w", err)
	}
	if auth == nil || auth.UserID == "" {
		return errors.New("slack auth.test for context check returned no bot user ID")
	}
	if _, err := api.GetUserInfoContext(ctx, auth.UserID); err != nil {
		return fmt.Errorf("slack users.info failed: %w", err)
	}
	return nil
}

func (liveChecker) CheckSlackCanvas(ctx context.Context, botToken string) error {
	return checkSlackBotScope(ctx, botToken, "canvases:write", "Canvas")
}

func (liveChecker) CheckSlackExports(ctx context.Context, botToken string) error {
	return checkSlackBotScope(ctx, botToken, "files:write", "generated file")
}

func checkSlackBotScope(ctx context.Context, botToken, requiredScope, subject string) error {
	var grantedScopes string
	api := slackapi.New(botToken, slackapi.OptionOnResponseHeaders(func(path string, headers http.Header) {
		if path == "auth.test" {
			grantedScopes = headers.Get("X-OAuth-Scopes")
		}
	}))
	if _, err := api.AuthTestContext(ctx); err != nil {
		return fmt.Errorf("slack auth.test for %s check failed: %w", subject, err)
	}
	if hasSlackScope(grantedScopes, requiredScope) {
		return nil
	}
	return fmt.Errorf("slack bot token is missing %s", requiredScope)
}

func hasSlackScope(grantedScopes, required string) bool {
	for scope := range strings.SplitSeq(grantedScopes, ",") {
		if strings.TrimSpace(scope) == required {
			return true
		}
	}
	return false
}

func (liveChecker) CheckResolvedModel(ctx context.Context, resolved *agentdef.ResolvedModel, apiKey string) error {
	llm, err := newModelFromResolved(resolved, apiKey)
	if err != nil {
		return err
	}
	request := &model.LLMRequest{
		Contents: []*genai.Content{genai.NewContentFromText("Reply with OK.", genai.RoleUser)},
	}
	for response, generateErr := range llm.GenerateContent(ctx, request, false) {
		if generateErr != nil {
			return generateErr
		}
		if response == nil || response.Content == nil {
			return errors.New("model endpoint returned no assistant content")
		}
		return nil
	}
	return errors.New("model endpoint returned no response")
}

func (liveChecker) CheckAttachmentAnalyzer(ctx context.Context, resolved *agentdef.ResolvedModel, apiKey string) error {
	llm, err := newModelFromResolved(resolved, apiKey)
	if err != nil {
		return err
	}
	tracker := &toolCallTrackingModel{delegate: llm}
	processor := adkartifact.NewProcessor(artifact.InMemoryService(), tracker, "Load the image artifact named in the current request and describe it.", defaultAttachmentTimeout, modelcalllimiter.New(1))
	_, err = processor.Process(ctx, port.AttachmentRequest{
		ProcessingID: "doctor-attachment-check",
		Attachment: port.LoadedAttachment{
			ID: "doctor-image", Name: "doctor.png", MIMEType: "image/png", Data: diagnosticPNG(),
		},
	})
	if err != nil {
		return fmt.Errorf("attachment_analyzer load_artifacts smoke test failed: %w", err)
	}
	if !tracker.loadArtifactsCalled.Load() {
		return errors.New("attachment_analyzer did not call load_artifacts")
	}
	return nil
}

func (liveChecker) CheckAudioTranscription(ctx context.Context, resolved *agentdef.ResolvedModel, apiKey string) error {
	if resolved == nil {
		return errors.New("audio transcription profile is not resolved")
	}
	transcriber, err := openaistt.New(openaistt.Config{
		BaseURL: resolved.BaseURL,
		APIKey:  apiKey,
		Model:   resolved.Model,
		Headers: resolved.Headers,
	})
	if err != nil {
		return err
	}
	if _, err := transcriber.Transcribe(ctx, port.AudioTranscriptionRequest{
		FileName: "doctor.wav",
		MIMEType: "audio/wav",
		Data:     diagnosticWAV(),
	}); err != nil {
		return fmt.Errorf("audio transcription endpoint check failed: %w", err)
	}
	return nil
}

func (liveChecker) CheckKnowledgeEmbedding(ctx context.Context, cfg config.KnowledgeEmbeddingConfig, apiKey string) error {
	provider, err := openaillm.NewEmbeddingProvider(openaillm.EmbeddingProviderConfig{
		APIKey:     apiKey,
		BaseURL:    cfg.BaseURL,
		Model:      cfg.Model,
		Dimensions: cfg.Dimensions,
		Timeout:    time.Duration(cfg.TimeoutSeconds) * time.Second,
		MaxBatch:   1,
		Limiter:    modelcalllimiter.New(1),
	})
	if err != nil {
		return err
	}
	vectors, err := provider.Embed(ctx, []string{"OK"})
	if err != nil {
		return fmt.Errorf("embedding endpoint check failed: %w", err)
	}
	if len(vectors) != 1 {
		return fmt.Errorf("embedding endpoint returned %d vectors for one input", len(vectors))
	}
	if err := domain.ValidateEmbeddingOutput(vectors[0], cfg.Dimensions); err != nil {
		return fmt.Errorf("embedding endpoint output is invalid: %w", err)
	}
	return nil
}

func diagnosticWAV() []byte {
	const (
		sampleRate = 16000
		seconds    = 1
		channels   = 1
		bits       = 16
	)
	dataBytes := sampleRate * seconds * channels * bits / 8
	data := make([]byte, dataBytes)
	var wav bytes.Buffer
	wav.WriteString("RIFF")
	_ = binary.Write(&wav, binary.LittleEndian, uint32(36+dataBytes))
	wav.WriteString("WAVEfmt ")
	_ = binary.Write(&wav, binary.LittleEndian, uint32(16))
	_ = binary.Write(&wav, binary.LittleEndian, uint16(1))
	_ = binary.Write(&wav, binary.LittleEndian, uint16(channels))
	_ = binary.Write(&wav, binary.LittleEndian, uint32(sampleRate))
	_ = binary.Write(&wav, binary.LittleEndian, uint32(sampleRate*channels*bits/8))
	_ = binary.Write(&wav, binary.LittleEndian, uint16(channels*bits/8))
	_ = binary.Write(&wav, binary.LittleEndian, uint16(bits))
	wav.WriteString("data")
	_ = binary.Write(&wav, binary.LittleEndian, uint32(dataBytes))
	_, _ = wav.Write(data)
	return wav.Bytes()
}

func diagnosticPNG() []byte {
	imageData := image.NewRGBA(image.Rect(0, 0, 16, 16))
	colors := []color.RGBA{
		{R: 0x22, G: 0x66, B: 0xaa, A: 0xff},
		{R: 0xaa, G: 0x66, B: 0x22, A: 0xff},
		{R: 0x66, G: 0xaa, B: 0x22, A: 0xff},
		{R: 0xaa, G: 0x22, B: 0x66, A: 0xff},
	}
	for y := range 16 {
		for x := range 16 {
			imageData.Set(x, y, colors[(x/8+2*(y/8))%len(colors)])
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, imageData); err != nil {
		return nil
	}
	return encoded.Bytes()
}

type toolCallTrackingModel struct {
	delegate            model.LLM
	loadArtifactsCalled atomic.Bool
}

func (m *toolCallTrackingModel) Name() string { return m.delegate.Name() }

func (m *toolCallTrackingModel) GenerateContent(ctx context.Context, request *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		for response, err := range m.delegate.GenerateContent(ctx, request, stream) {
			if response != nil && response.Content != nil {
				for _, part := range response.Content.Parts {
					if part != nil && part.FunctionCall != nil && part.FunctionCall.Name == "load_artifacts" {
						m.loadArtifactsCalled.Store(true)
					}
				}
			}
			if !yield(response, err) {
				return
			}
		}
	}
}

func newModelFromResolved(resolved *agentdef.ResolvedModel, apiKey string) (*openaillm.OpenAICompatibleLLM, error) {
	opts := []openaillm.Option{
		openaillm.WithAPIKey(apiKey),
		openaillm.WithBaseURL(resolved.BaseURL),
		openaillm.WithModel(resolved.Model),
	}
	if len(resolved.Headers) > 0 {
		opts = append(opts, openaillm.WithHeaders(resolved.Headers))
	}
	if resolved.ReasoningEffort != "" {
		opts = append(opts, openaillm.WithReasoningEffort(resolved.ReasoningEffort))
	}
	if len(resolved.ExtraBody) > 0 {
		opts = append(opts, openaillm.WithExtraBody(resolved.ExtraBody))
	}
	llm, err := openaillm.New(opts...)
	if err != nil {
		return nil, err
	}
	counter, err := composeRootTokenCounter(resolved)
	if err != nil {
		return nil, err
	}
	budget, err := domain.NewRequestBudget(resolved.ContextWindowTokens, domain.RequestBudgetPolicy{MaxRequestPercent: 60})
	if err != nil {
		return nil, err
	}
	if err := llm.ConfigureRequestGuard(counter, budget, resolved.Provider.Name+"/"+resolved.Model); err != nil {
		return nil, err
	}
	if err := llm.ConfigureDefaultMaxOutputTokens(resolved.MaxOutputTokens); err != nil {
		return nil, err
	}
	return llm, nil
}

// counterChecker implements doctor.CounterChecker with the same factory the
// startup path uses, so doctor offline and runtime composition can never
// disagree about availability.
type counterChecker struct{}

func (counterChecker) CheckCounter(strategy, id string) error {
	_, err := tokencounter.New(strategy, id)
	return err
}

// cliProviderChecker implements doctor.CLIProviderChecker for agent_cli
// providers through the same construction and handshake path used at startup.
type cliProviderChecker struct{}

func (cliProviderChecker) CheckProvider(ctx context.Context, resolved *agentdef.ResolvedModel, cfg config.Config, projectRoot string, describe bool) (doctor.CLIProviderCheck, error) {
	paths, err := cfg.ResolvePaths(projectRoot)
	if err != nil {
		return doctor.CLIProviderCheck{}, err
	}
	cliModel, err := buildAgentCLIModel(ctx, resolved, cfg, paths, nil, nil)
	if err != nil {
		return doctor.CLIProviderCheck{}, err
	}
	description, err := handshakeAgentCLI(ctx, cliModel, describe)
	if err != nil {
		return doctor.CLIProviderCheck{}, err
	}
	if describe {
		return doctor.CLIProviderCheck{
			Detail:       fmt.Sprintf("agent CLI %s version %s; profile validated", description.Name, description.CLIVersion),
			ProviderName: description.Name,
		}, nil
	}
	return doctor.CLIProviderCheck{Detail: "profile validated"}, nil
}

type acpProviderChecker struct{}

func (acpProviderChecker) CheckProvider(ctx context.Context, resolved *agentdef.ResolvedModel, projectRoots map[string]string) (string, error) {
	client := acpclient.New(resolved.Command, resolved.Args)
	description, err := client.Describe(ctx)
	if err != nil {
		return "", err
	}
	options := make([]domain.ACPConfigOption, 0, len(resolved.ConfigOptions))
	for _, option := range resolved.ConfigOptions {
		options = append(options, domain.ACPConfigOption{ID: option.ID, Value: option.Value})
	}
	if len(projectRoots) == 0 {
		return "", errors.New("ACP provider has no registered project to probe")
	}
	for name, path := range projectRoots {
		canonical, err := canonicalProjectPath(path)
		if err != nil {
			return "", fmt.Errorf("project %q: %w", name, err)
		}
		if err := client.Probe(ctx, canonical, options); err != nil {
			return "", fmt.Errorf("project %q: %w", name, err)
		}
	}
	return fmt.Sprintf("%s %s uses ACP v%s; single-project profile verified", description.AgentInfo.Name, description.AgentInfo.Version, description.ProtocolVersion), nil
}

// CheckAuthentication reports saved-login status for a known provider identity
// without making a model call. The command is application-owned and selected
// from the trusted descriptor; providers cannot supply authentication argv.
// Native output can contain account identifiers, so both streams are drained
// and discarded.
func (cliProviderChecker) CheckAuthentication(ctx context.Context, _ *agentdef.ResolvedModel, providerName string) (string, error) {
	var (
		executable string
		args       []string
		success    string
	)
	switch providerName {
	case "opencode":
		executable, args = "opencode", []string{"auth", "list"}
		success = "opencode auth list succeeded; saved credentials are available"
	case "codex":
		executable, args = "codex", []string{"login", "status"}
		success = "codex login status succeeded; saved credentials are available"
	default:
		return "", fmt.Errorf("authentication status for provider %q is not supported by this release", providerName)
	}
	resolved, err := exec.LookPath(executable)
	if err != nil {
		return "", fmt.Errorf("%s executable not found: %w", executable, err)
	}
	command := exec.CommandContext(ctx, resolved, args...)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("%s %s cancelled: %w", executable, strings.Join(args, " "), ctxErr)
		}
		return "", fmt.Errorf("%s %s failed: %w", executable, strings.Join(args, " "), err)
	}
	return success, nil
}
