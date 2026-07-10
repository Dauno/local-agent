package openaillm

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/openai/openai-go/v3/option"
)

type settings struct {
	apiKey          string
	baseURL         string
	headers         map[string]string
	model           string
	reasoningEffort string
	extraBody       map[string]any
}

// Option configures an OpenAI-compatible model adapter without coupling it to
// the application's concrete configuration representation.
type Option func(*settings) error

// WithAPIKey configures the credential passed to the OpenAI client.
func WithAPIKey(value string) Option {
	return func(cfg *settings) error {
		if strings.TrimSpace(value) == "" {
			return errors.New("OpenAI-compatible API key is required")
		}
		cfg.apiKey = value
		return nil
	}
}

// WithBaseURL configures the trusted OpenAI-compatible endpoint base URL.
func WithBaseURL(value string) Option {
	return func(cfg *settings) error {
		parsed, err := url.Parse(value)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return errors.New("OpenAI-compatible base URL must be an absolute http or https URL")
		}
		if parsed.User != nil {
			return errors.New("OpenAI-compatible base URL must not contain credentials")
		}
		if parsed.Fragment != "" {
			return errors.New("OpenAI-compatible base URL must not contain a fragment")
		}
		cfg.baseURL = value
		return nil
	}
}

// WithHeaders adds optional non-sensitive static HTTP headers.
func WithHeaders(values map[string]string) Option {
	return func(cfg *settings) error {
		headers := make(map[string]string, len(values))
		for name, value := range values {
			if !validHeaderName(name) {
				return fmt.Errorf("invalid OpenAI-compatible header name %q", name)
			}
			if strings.ContainsAny(value, "\r\n") {
				return fmt.Errorf("OpenAI-compatible header %q must not contain a newline", name)
			}
			if sensitiveHeader(name) {
				return fmt.Errorf("OpenAI-compatible header %q must not contain credentials", name)
			}
			headers[name] = value
		}
		cfg.headers = headers
		return nil
	}
}

func sensitiveHeader(name string) bool {
	switch strings.ToLower(name) {
	case "authorization", "proxy-authorization", "cookie", "set-cookie", "x-api-key", "api-key":
		return true
	default:
		return false
	}
}

func validHeaderName(value string) bool {
	if value == "" {
		return false
	}
	for index := 0; index < len(value); index++ {
		char := value[index]
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') {
			continue
		}
		switch char {
		case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
			continue
		default:
			return false
		}
	}
	return true
}

// WithModel configures the exact model identifier sent to the provider.
func WithModel(value string) Option {
	return func(cfg *settings) error {
		if strings.TrimSpace(value) == "" {
			return errors.New("OpenAI-compatible model name is required")
		}
		cfg.model = value
		return nil
	}
}

// WithReasoningEffort configures the provider-compatible reasoning effort.
func WithReasoningEffort(value string) Option {
	return func(cfg *settings) error {
		if strings.TrimSpace(value) == "" {
			return errors.New("reasoning effort must not be empty")
		}
		cfg.reasoningEffort = value
		return nil
	}
}

// WithExtraBody configures trusted extra JSON fields. Extra fields may override
// typed request fields, except stream, which is reserved by this non-streaming
// adapter.
func WithExtraBody(values map[string]any) Option {
	return func(cfg *settings) error {
		if _, present := values["stream"]; present {
			return errors.New("extra request body must not override reserved field stream")
		}
		encoded, err := json.Marshal(values)
		if err != nil {
			return fmt.Errorf("extra request body must contain JSON-compatible values: %w", err)
		}
		var cloned map[string]any
		if err := json.Unmarshal(encoded, &cloned); err != nil {
			return fmt.Errorf("clone extra request body: %w", err)
		}
		cfg.extraBody = cloned
		return nil
	}
}

func (cfg settings) clientOptions() []option.RequestOption {
	options := []option.RequestOption{
		option.WithAPIKey(cfg.apiKey),
		option.WithBaseURL(cfg.baseURL),
	}
	headerNames := make([]string, 0, len(cfg.headers))
	for name := range cfg.headers {
		headerNames = append(headerNames, name)
	}
	sort.Strings(headerNames)
	for _, name := range headerNames {
		options = append(options, option.WithHeader(name, cfg.headers[name]))
	}
	return options
}

func (cfg settings) validate() error {
	if cfg.apiKey == "" {
		return errors.New("OpenAI-compatible API key is required")
	}
	if cfg.baseURL == "" {
		return errors.New("OpenAI-compatible base URL is required")
	}
	if cfg.model == "" {
		return errors.New("OpenAI-compatible model name is required")
	}
	return nil
}
