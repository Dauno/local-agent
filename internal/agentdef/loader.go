package agentdef

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

var environmentNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func Load(dir string) (*Definitions, error) {
	agentsDir := filepath.Join(dir, "agents")
	providersDir := filepath.Join(dir, "providers")

	agentsExists, err := dirExists(agentsDir)
	if err != nil {
		return nil, fmt.Errorf("check agents directory: %w", err)
	}
	providersExists, err := dirExists(providersDir)
	if err != nil {
		return nil, fmt.Errorf("check providers directory: %w", err)
	}
	if !agentsExists && !providersExists {
		return nil, nil
	}
	if !agentsExists || !providersExists {
		return nil, errors.New("agents and providers directories must either both exist or both be absent")
	}

	providers, err := loadProviders(providersDir)
	if err != nil {
		return nil, err
	}
	agents, err := loadAgents(agentsDir)
	if err != nil {
		return nil, err
	}

	defs := &Definitions{Providers: providers, Agents: agents}
	if err := validateDefinitions(defs); err != nil {
		return nil, err
	}
	return defs, nil
}

func LoadFromDirs(agentsDir, providersDir string) (*Definitions, error) {
	providers, err := loadProviders(providersDir)
	if err != nil {
		return nil, err
	}
	agents, err := loadAgents(agentsDir)
	if err != nil {
		return nil, err
	}
	defs := &Definitions{Providers: providers, Agents: agents}
	if err := validateDefinitions(defs); err != nil {
		return nil, err
	}
	return defs, nil
}

func dirExists(path string) (bool, error) {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return info.IsDir(), nil
}

func loadProviders(dir string) (map[string]Provider, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read providers directory: %w", err)
	}
	providers := make(map[string]Provider)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read provider file %q: %w", entry.Name(), err)
		}
		var p Provider
		if err := decodeStrictYAML(data, &p); err != nil {
			return nil, fmt.Errorf("parse provider file %q: %w", entry.Name(), err)
		}
		if _, exists := providers[p.Name]; exists {
			return nil, fmt.Errorf("duplicate provider name %q in %q", p.Name, entry.Name())
		}
		providers[p.Name] = p
	}
	return providers, nil
}

func loadAgents(dir string) (map[string]AgentDef, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read agents directory: %w", err)
	}
	agents := make(map[string]AgentDef)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read agent file %q: %w", entry.Name(), err)
		}
		var a AgentDef
		if err := decodeStrictYAML(data, &a); err != nil {
			return nil, fmt.Errorf("parse agent file %q: %w", entry.Name(), err)
		}
		if _, exists := agents[a.Name]; exists {
			return nil, fmt.Errorf("duplicate agent name %q in %q", a.Name, entry.Name())
		}
		agents[a.Name] = a
	}
	return agents, nil
}

func decodeStrictYAML(data []byte, target any) error {
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("expected one YAML document")
		}
		return err
	}
	return nil
}

func validateDefinitions(defs *Definitions) error {
	var errs []string

	if len(defs.Providers) == 0 {
		errs = append(errs, "at least one provider definition is required")
	}
	for _, p := range defs.Providers {
		errs = append(errs, validateProvider(p)...)
	}
	if len(defs.Agents) == 0 {
		errs = append(errs, "at least one agent definition is required")
	}
	for _, a := range defs.Agents {
		errs = append(errs, validateAgent(a, defs.Providers)...)
	}

	if len(errs) > 0 {
		return fmt.Errorf("invalid agent definitions: %s", strings.Join(errs, "; "))
	}
	return nil
}

func validateProvider(p Provider) []string {
	var errs []string
	prefix := fmt.Sprintf("provider %q", p.Name)

	if strings.TrimSpace(p.Name) == "" {
		errs = append(errs, "provider name must not be empty")
	}
	if p.Type != "openai_compatible" {
		errs = append(errs, fmt.Sprintf("%s: type must be openai_compatible", prefix))
	}
	if err := validateBaseURL(p.BaseURL); err != nil {
		errs = append(errs, fmt.Sprintf("%s: %s", prefix, err))
	}
	if !environmentNamePattern.MatchString(p.APIKeyEnv) {
		errs = append(errs, fmt.Sprintf("%s: api_key_env must be a valid environment variable name", prefix))
	}
	for name, value := range p.Headers {
		if !validHeaderName(name) {
			errs = append(errs, fmt.Sprintf("%s: header %q is not a valid HTTP header name", prefix, name))
		}
		if sensitiveHeader(name) {
			errs = append(errs, fmt.Sprintf("%s: header %q must not contain credentials", prefix, name))
		}
		if strings.ContainsAny(value, "\r\n") {
			errs = append(errs, fmt.Sprintf("%s: header %q must not contain a newline", prefix, name))
		}
	}
	if len(p.Profiles) == 0 {
		errs = append(errs, fmt.Sprintf("%s: at least one profile is required", prefix))
	}

	seenProfiles := make(map[string]struct{})
	for name, profile := range p.Profiles {
		if strings.TrimSpace(name) == "" {
			errs = append(errs, fmt.Sprintf("%s: profile name must not be empty", prefix))
			continue
		}
		if _, exists := seenProfiles[name]; exists {
			errs = append(errs, fmt.Sprintf("%s: duplicate profile %q", prefix, name))
		}
		seenProfiles[name] = struct{}{}
		errs = append(errs, validateProfile(prefix, name, profile)...)
	}
	return errs
}

func validateProfile(providerPrefix, name string, profile Profile) []string {
	var errs []string
	prefix := fmt.Sprintf("%s profile %q", providerPrefix, name)

	if strings.TrimSpace(profile.Model) == "" {
		errs = append(errs, fmt.Sprintf("%s: model must not be empty", prefix))
	}
	if _, err := json.Marshal(profile.ExtraBody); err != nil {
		errs = append(errs, fmt.Sprintf("%s: extra_body must contain JSON-compatible values: %v", prefix, err))
	}
	if _, present := profile.ExtraBody["stream"]; present {
		errs = append(errs, fmt.Sprintf("%s: extra_body.stream is reserved", prefix))
	}

	if profile.GenerateContentConfig != nil && profile.GenerateContentConfig.MaxOutputTokens < 0 {
		errs = append(errs, fmt.Sprintf("%s: generate_content_config.max_output_tokens must not be negative", prefix))
	}

	return errs
}

func validateAgent(a AgentDef, providers map[string]Provider) []string {
	var errs []string
	prefix := fmt.Sprintf("agent %q", a.Name)

	if strings.TrimSpace(a.Name) == "" {
		errs = append(errs, "agent name must not be empty")
	}
	if a.AgentClass != "LlmAgent" {
		errs = append(errs, fmt.Sprintf("%s: agent_class must be LlmAgent", prefix))
	}
	if strings.TrimSpace(a.Instruction) == "" {
		errs = append(errs, fmt.Sprintf("%s: instruction must not be empty", prefix))
	}

	if a.Model == "" {
		errs = append(errs, fmt.Sprintf("%s: model reference must not be empty", prefix))
	} else {
		providerName, profileName, ok := splitModelReference(a.Model)
		if !ok {
			errs = append(errs, fmt.Sprintf("%s: model must be provider/profile format", prefix))
		} else {
			if p, exists := providers[providerName]; !exists {
				errs = append(errs, fmt.Sprintf("%s: unknown provider %q", prefix, providerName))
			} else if _, exists := p.Profiles[profileName]; !exists {
				errs = append(errs, fmt.Sprintf("%s: unknown profile %q in provider %q", prefix, profileName, providerName))
			}
		}
	}

	switch a.IncludeContents {
	case "", "default", "none":
	default:
		errs = append(errs, fmt.Sprintf("%s: include_contents must be default or none", prefix))
	}
	if a.Mode != "" && a.Mode != "chat" {
		errs = append(errs, fmt.Sprintf("%s: mode must be chat", prefix))
	}
	if a.ToolScope != "" && a.ToolScope != "invocation_scoped" {
		errs = append(errs, fmt.Sprintf("%s: tool_scope must be invocation_scoped", prefix))
	}

	return errs
}

func splitModelReference(modelRef string) (providerName, profileName string, ok bool) {
	if strings.Count(modelRef, "/") != 1 {
		return "", "", false
	}
	parts := strings.SplitN(modelRef, "/", 2)
	if strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func validateBaseURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("base_url must be an absolute http or https URL")
	}
	if parsed.User != nil {
		return fmt.Errorf("base_url must not contain credentials")
	}
	if parsed.Fragment != "" {
		return fmt.Errorf("base_url must not contain a fragment")
	}
	return nil
}

func validHeaderName(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		char := value[i]
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

func sensitiveHeader(name string) bool {
	switch strings.ToLower(name) {
	case "authorization", "proxy-authorization", "cookie", "set-cookie", "x-api-key", "api-key":
		return true
	default:
		return false
	}
}
