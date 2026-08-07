package agentdef

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

var environmentNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

const maxContextWindowTokens = 10_000_000

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

	return LoadFromDirs(agentsDir, providersDir)
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
	if err := ValidateDefinitions(defs); err != nil {
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
	return loadYAMLDir[Provider](dir, "provider", func(provider Provider) string {
		return provider.Name
	}, validateProviderFieldPresence)
}

func loadAgents(dir string) (map[string]AgentDef, error) {
	return loadYAMLDir[AgentDef](dir, "agent", func(agent AgentDef) string {
		return agent.Name
	}, nil)
}

func loadYAMLDir[T any](dir, kind string, nameOf func(T) string, postDecode func([]byte, T) error) (map[string]T, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %ss directory: %w", kind, err)
	}
	values := make(map[string]T)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s file %q: %w", kind, entry.Name(), err)
		}
		var value T
		if err := decodeStrictYAML(data, &value); err != nil {
			return nil, fmt.Errorf("parse %s file %q: %w", kind, entry.Name(), err)
		}
		if postDecode != nil {
			if err := postDecode(data, value); err != nil {
				return nil, fmt.Errorf("parse %s file %q: %w", kind, entry.Name(), err)
			}
		}
		name := nameOf(value)
		if _, exists := values[name]; exists {
			return nil, fmt.Errorf("duplicate %s name %q in %q", kind, name, entry.Name())
		}
		values[name] = value
	}
	return values, nil
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

// validateProviderFieldPresence rejects type-specific fields even when YAML
// decodes them to an indistinguishable empty Go value.
func validateProviderFieldPresence(data []byte, provider Provider) error {
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return err
	}
	root := yamlDocumentRoot(&document)
	if root == nil || root.Kind != yaml.MappingNode {
		return nil
	}

	var providerForbidden, profileForbidden []string
	switch provider.Type {
	case ProviderTypeAgentCLI:
		providerForbidden = []string{"base_url", "api_key_env", "headers", "command", "args"}
		profileForbidden = []string{"reasoning_effort", "extra_body", "generate_content_config", "config_options", "permission_option_kind"}
	case ProviderTypeOpenAICompatible:
		providerForbidden = []string{"shim", "command", "args"}
		profileForbidden = []string{"agent", "approval", "variant", "config_options", "permission_option_kind"}
	case ProviderTypeACP:
		providerForbidden = []string{"base_url", "api_key_env", "headers", "shim"}
		profileForbidden = []string{"model", "agent", "approval", "variant", "reasoning_effort", "extra_body", "generate_content_config"}
	default:
		return nil
	}

	prefix := fmt.Sprintf("provider %q", provider.Name)
	var errs []string
	for _, field := range providerForbidden {
		if mappingHasKey(root, field) {
			if provider.Type == ProviderTypeOpenAICompatible {
				errs = append(errs, fmt.Sprintf("%s: %s is only valid for %s providers", prefix, field, ProviderTypeAgentCLI))
			} else {
				errs = append(errs, fmt.Sprintf("%s: %s is invalid for %s providers", prefix, field, provider.Type))
			}
		}
	}
	profiles := mappingValue(root, "profiles")
	if profiles != nil && profiles.Kind == yaml.MappingNode {
		for index := 0; index+1 < len(profiles.Content); index += 2 {
			profileName := profiles.Content[index].Value
			profileNode := dereferenceAlias(profiles.Content[index+1])
			if profileNode == nil || profileNode.Kind != yaml.MappingNode {
				continue
			}
			for _, field := range profileForbidden {
				if mappingHasKey(profileNode, field) {
					if provider.Type == ProviderTypeOpenAICompatible {
						errs = append(errs, fmt.Sprintf("%s profile %q: %s is only valid for %s profiles", prefix, profileName, field, ProviderTypeAgentCLI))
					} else {
						errs = append(errs, fmt.Sprintf("%s profile %q: %s is invalid for %s profiles", prefix, profileName, field, provider.Type))
					}
				}
			}
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func yamlDocumentRoot(document *yaml.Node) *yaml.Node {
	if document == nil || len(document.Content) == 0 {
		return nil
	}
	return dereferenceAlias(document.Content[0])
}

func dereferenceAlias(node *yaml.Node) *yaml.Node {
	for node != nil && node.Kind == yaml.AliasNode {
		node = node.Alias
	}
	return node
}

func mappingHasKey(mapping *yaml.Node, key string) bool {
	return mappingValue(mapping, key) != nil
}

func mappingValue(mapping *yaml.Node, key string) *yaml.Node {
	mapping = dereferenceAlias(mapping)
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			return dereferenceAlias(mapping.Content[index+1])
		}
	}
	return nil
}

func ValidateDefinitions(defs *Definitions) error {
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
		if !IsReservedAgentName(a.Name) {
			if err := a.ValidateName(); err != nil {
				errs = append(errs, fmt.Sprintf("agent %q: %v", a.Name, err))
			}
		}
		errs = append(errs, validateAgent(a, defs.Providers)...)
	}
	for _, name := range eligibleAgentNames(defs) {
		if IsReservedAgentName(name) {
			continue
		}
		errs = append(errs, ValidateAgentEligibility(defs.Agents[name], defs.Providers)...)
	}
	errs = append(errs, validateAgentTools(defs)...)
	errs = append(errs, validateWorkflowTools(defs)...)

	if len(errs) > 0 {
		return fmt.Errorf("invalid agent definitions: %s", strings.Join(errs, "; "))
	}
	return nil
}

func ValidateCandidateAgent(current *Definitions, candidate AgentDef) error {
	if err := candidate.ValidateName(); err != nil {
		return err
	}
	if err := candidate.ValidateSize(); err != nil {
		return err
	}
	if current == nil {
		return errors.New("current agent definitions must not be nil")
	}
	if candidate.AgentClass != "LlmAgent" && candidate.AgentClass != "AcpAgent" {
		return fmt.Errorf("agent %q: agent_class must be LlmAgent or AcpAgent", candidate.Name)
	}
	if candidate.Role != "" {
		return fmt.Errorf("agent %q: role must be empty", candidate.Name)
	}
	if eligibilityErrs := ValidateAgentEligibility(candidate, current.Providers); len(eligibilityErrs) > 0 {
		return fmt.Errorf("%s", eligibilityErrs[0])
	}
	if _, exists := current.Agents[candidate.Name]; exists {
		return fmt.Errorf("agent name %q already exists", candidate.Name)
	}
	if candidate.AgentClass != "AcpAgent" {
		providerName, profileName, ok := splitModelReference(candidate.Model)
		if !ok {
			return fmt.Errorf("agent %q: model must be provider/profile format", candidate.Name)
		}
		provider, exists := current.Providers[providerName]
		if !exists {
			return fmt.Errorf("agent %q: unknown provider %q", candidate.Name, providerName)
		}
		if _, exists := provider.Profiles[profileName]; !exists {
			return fmt.Errorf("agent %q: unknown profile %q in provider %q", candidate.Name, profileName, providerName)
		}
	}

	snapshot := &Definitions{
		Providers: maps.Clone(current.Providers),
		Agents:    maps.Clone(current.Agents),
	}
	if snapshot.Agents == nil {
		snapshot.Agents = make(map[string]AgentDef)
	}
	snapshot.Agents[candidate.Name] = candidate
	return ValidateDefinitions(snapshot)
}

func ValidateAgentEligibility(agent AgentDef, providers map[string]Provider) []string {
	prefix := fmt.Sprintf("agent tool %q", agent.Name)
	var errs []string
	if strings.TrimSpace(agent.Description) == "" {
		errs = append(errs, fmt.Sprintf("%s: description must not be empty", prefix))
	}
	if agent.DurableSession {
		errs = append(errs, fmt.Sprintf("%s: durable_session and role are not supported", prefix))
	}
	if len(agent.AgentTools) > 0 {
		errs = append(errs, fmt.Sprintf("%s: nested agent_tools are not supported", prefix))
	}
	if agent.AgentClass == "AcpAgent" && agent.Confirmation != "required" {
		errs = append(errs, fmt.Sprintf("%s: confirmation must be required", prefix))
	}

	if provider, ok := providerForAgent(agent, providers); ok {
		switch provider.Type {
		case ProviderTypeAgentCLI:
			if agent.ToolScope != "" {
				errs = append(errs, fmt.Sprintf("%s: tool_scope is not supported for %s agent tools", prefix, ProviderTypeAgentCLI))
			}
		case ProviderTypeOpenAICompatible:
			if agent.ToolScope != "invocation_scoped" {
				errs = append(errs, fmt.Sprintf("%s: %s agent tools must declare tool_scope: invocation_scoped", prefix, ProviderTypeOpenAICompatible))
			}
		}
	}
	return errs
}

func eligibleAgentNames(defs *Definitions) []string {
	if defs == nil {
		return nil
	}
	names := make([]string, 0, len(defs.Agents))
	for name, agent := range defs.Agents {
		if name == "root_agent" || agent.Role != "" {
			continue
		}
		if agent.AgentClass != "LlmAgent" && agent.AgentClass != "AcpAgent" {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// EligibleAgentNames returns the deterministically ordered agent definitions
// that can be auto-discovered as root agent tools.
func EligibleAgentNames(defs *Definitions) []string {
	return eligibleAgentNames(defs)
}

func validateProvider(p Provider) []string {
	var errs []string
	prefix := fmt.Sprintf("provider %q", p.Name)

	if strings.TrimSpace(p.Name) == "" {
		errs = append(errs, "provider name must not be empty")
	}
	switch p.Type {
	case ProviderTypeOpenAICompatible:
		errs = append(errs, validateOpenAICompatibleProvider(prefix, p)...)
	case ProviderTypeAgentCLI:
		errs = append(errs, validateAgentCLIProvider(prefix, p)...)
	case ProviderTypeACP:
		errs = append(errs, validateACPProvider(prefix, p)...)
	default:
		errs = append(errs, fmt.Sprintf("%s: type must be %s, %s, or %s", prefix, ProviderTypeOpenAICompatible, ProviderTypeAgentCLI, ProviderTypeACP))
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
		errs = append(errs, validateProfile(prefix, p.Type, name, profile)...)
	}
	return errs
}

func validateOpenAICompatibleProvider(prefix string, p Provider) []string {
	var errs []string
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
	if p.Shim != nil {
		errs = append(errs, fmt.Sprintf("%s: shim is only valid for %s providers", prefix, ProviderTypeAgentCLI))
	}
	return errs
}

func validateAgentCLIProvider(prefix string, p Provider) []string {
	errs := forbidHTTPFields(prefix, p, ProviderTypeAgentCLI)
	if p.Shim == nil {
		errs = append(errs, fmt.Sprintf("%s: shim is required for %s providers", prefix, ProviderTypeAgentCLI))
		return errs
	}
	errs = append(errs, validateCommandArgs(prefix, "shim", p.Shim.Command, p.Shim.Args)...)
	return errs
}

func validateACPProvider(prefix string, p Provider) []string {
	errs := forbidHTTPFields(prefix, p, ProviderTypeACP)
	if p.Shim != nil {
		errs = append(errs, fmt.Sprintf("%s: shim is invalid for %s providers", prefix, ProviderTypeACP))
	}
	errs = append(errs, validateCommandArgs(prefix, "", p.Command, p.Args)...)
	for profileName, profile := range p.Profiles {
		profilePrefix := fmt.Sprintf("%s profile %q", prefix, profileName)
		if len(profile.ConfigOptions) == 0 {
			errs = append(errs, fmt.Sprintf("%s: at least one config option is required", profilePrefix))
		}
		seenOptions := make(map[string]struct{}, len(profile.ConfigOptions))
		for i, opt := range profile.ConfigOptions {
			id := strings.TrimSpace(opt.ID)
			if id == "" {
				errs = append(errs, fmt.Sprintf("%s: config_options[%d].id must not be empty", profilePrefix, i))
			} else if len(id) > 256 {
				errs = append(errs, fmt.Sprintf("%s: config_options[%d].id exceeds 256 bytes", profilePrefix, i))
			} else if _, duplicate := seenOptions[id]; duplicate {
				errs = append(errs, fmt.Sprintf("%s: duplicate config option id %q", profilePrefix, id))
			} else {
				seenOptions[id] = struct{}{}
			}
			switch value := opt.Value.(type) {
			case string:
				if strings.TrimSpace(value) == "" {
					errs = append(errs, fmt.Sprintf("%s: config_options[%d].value must not be empty", profilePrefix, i))
				} else if len(value) > 4096 {
					errs = append(errs, fmt.Sprintf("%s: config_options[%d].value exceeds 4096 bytes", profilePrefix, i))
				}
			case bool:
			default:
				errs = append(errs, fmt.Sprintf("%s: config_options[%d].value must be a string or boolean", profilePrefix, i))
			}
		}
		switch profile.PermissionOptionKind {
		case "", "reject_once", "allow_once":
		default:
			errs = append(errs, fmt.Sprintf("%s: permission_option_kind must be reject_once or allow_once", profilePrefix))
		}
	}
	return errs
}

func forbidHTTPFields(prefix string, p Provider, providerType string) []string {
	var errs []string
	if p.BaseURL != "" {
		errs = append(errs, fmt.Sprintf("%s: base_url is invalid for %s providers", prefix, providerType))
	}
	if p.APIKeyEnv != "" {
		errs = append(errs, fmt.Sprintf("%s: api_key_env is invalid for %s providers", prefix, providerType))
	}
	if len(p.Headers) > 0 {
		errs = append(errs, fmt.Sprintf("%s: headers are invalid for %s providers", prefix, providerType))
	}
	return errs
}

func validateCommandArgs(prefix, label, command string, args []string) []string {
	commandLabel := "command"
	argsLabel := "args"
	if label != "" {
		commandLabel = label + ".command"
		argsLabel = label + ".args"
	}

	var errs []string
	if strings.TrimSpace(command) == "" {
		if label == "" {
			errs = append(errs, fmt.Sprintf("%s: command is required for %s providers", prefix, ProviderTypeACP))
		} else {
			errs = append(errs, fmt.Sprintf("%s: %s must not be empty", prefix, commandLabel))
		}
	}
	if strings.ContainsAny(command, "\r\n\x00") {
		errs = append(errs, fmt.Sprintf("%s: %s must be a single line", prefix, commandLabel))
	}
	for index, arg := range args {
		if strings.TrimSpace(arg) == "" {
			errs = append(errs, fmt.Sprintf("%s: %s[%d] must not be empty", prefix, argsLabel, index))
		}
		if strings.ContainsAny(arg, "\r\n\x00") {
			errs = append(errs, fmt.Sprintf("%s: %s[%d] must be a single line", prefix, argsLabel, index))
		}
	}
	return errs
}

func validateProfile(providerPrefix, providerType, name string, profile Profile) []string {
	var errs []string
	prefix := fmt.Sprintf("%s profile %q", providerPrefix, name)

	if providerType != ProviderTypeACP && strings.TrimSpace(profile.Model) == "" {
		errs = append(errs, fmt.Sprintf("%s: model must not be empty", prefix))
	}

	switch providerType {
	case ProviderTypeACP, ProviderTypeAgentCLI:
		if profile.ReasoningEffort != "" {
			errs = append(errs, fmt.Sprintf("%s: reasoning_effort is invalid for %s profiles", prefix, providerType))
		}
		if len(profile.ExtraBody) > 0 {
			errs = append(errs, fmt.Sprintf("%s: extra_body is invalid for %s profiles", prefix, providerType))
		}
		if profile.GenerateContentConfig != nil {
			errs = append(errs, fmt.Sprintf("%s: generate_content_config is invalid for %s profiles", prefix, providerType))
		}
		switch providerType {
		case ProviderTypeACP:
			if profile.Agent != "" {
				errs = append(errs, fmt.Sprintf("%s: agent is invalid for %s profiles", prefix, ProviderTypeACP))
			}
			if profile.Approval != "" {
				errs = append(errs, fmt.Sprintf("%s: approval is invalid for %s profiles", prefix, ProviderTypeACP))
			}
			if profile.Variant != "" {
				errs = append(errs, fmt.Sprintf("%s: variant is invalid for %s profiles", prefix, ProviderTypeACP))
			}
		case ProviderTypeAgentCLI:
			switch profile.Approval {
			case "", ApprovalReject, ApprovalAuto:
			default:
				errs = append(errs, fmt.Sprintf("%s: approval must be %s or %s", prefix, ApprovalReject, ApprovalAuto))
			}
		}
	default:
		if profile.Agent != "" {
			errs = append(errs, fmt.Sprintf("%s: agent is only valid for %s profiles", prefix, ProviderTypeAgentCLI))
		}
		if profile.Approval != "" {
			errs = append(errs, fmt.Sprintf("%s: approval is only valid for %s profiles", prefix, ProviderTypeAgentCLI))
		}
		if profile.Variant != "" {
			errs = append(errs, fmt.Sprintf("%s: variant is only valid for %s profiles", prefix, ProviderTypeAgentCLI))
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
		if profile.ContextWindowTokens != nil || profile.MaxOutputTokens != nil || profile.TokenCounter != nil {
			errs = append(errs, validateTokenBudgets(prefix, profile.ContextWindowTokens, profile.MaxOutputTokens, profile.TokenCounter, false)...)
		}
	}

	return errs
}

func validateTokenBudgets(prefix string, contextWindowTokens, maxOutputTokens *int, tokenCounter *TokenCounterDef, counterRequired bool) []string {
	if contextWindowTokens == nil && maxOutputTokens == nil && tokenCounter == nil {
		return nil
	}

	var errs []string
	if contextWindowTokens == nil || *contextWindowTokens <= 0 {
		errs = append(errs, fmt.Sprintf("%s: context_window_tokens must be positive", prefix))
	} else if *contextWindowTokens > maxContextWindowTokens {
		errs = append(errs, fmt.Sprintf("%s: context_window_tokens exceeds safe maximum of %d", prefix, maxContextWindowTokens))
	}
	if maxOutputTokens != nil && *maxOutputTokens < 0 {
		errs = append(errs, fmt.Sprintf("%s: max_output_tokens must not be negative", prefix))
	}
	if maxOutputTokens != nil && contextWindowTokens != nil && *maxOutputTokens > 0 && *contextWindowTokens > 0 && *maxOutputTokens >= *contextWindowTokens {
		errs = append(errs, fmt.Sprintf("%s: max_output_tokens must be less than context_window_tokens", prefix))
	}
	if tokenCounter == nil {
		return errs
	}
	if tokenCounter.Strategy == "" {
		if counterRequired {
			errs = append(errs, fmt.Sprintf("%s: token_counter.strategy is required for composition", prefix))
		} else {
			errs = append(errs, fmt.Sprintf("%s: token_counter.strategy must not be empty", prefix))
		}
		return errs
	}
	switch tokenCounter.Strategy {
	case "official", "endpoint", "estimator":
		if strings.TrimSpace(tokenCounter.ID) == "" {
			errs = append(errs, fmt.Sprintf("%s: token_counter.id is required for strategy %q", prefix, tokenCounter.Strategy))
		}
	case "byte_bound":
		if strings.TrimSpace(tokenCounter.ID) != "" {
			errs = append(errs, fmt.Sprintf("%s: token_counter.id must be empty for strategy %q", prefix, tokenCounter.Strategy))
		}
	default:
		errs = append(errs, fmt.Sprintf("%s: token_counter.strategy must be one of official, endpoint, estimator, or byte_bound", prefix))
	}
	return errs
}

func validateAgent(a AgentDef, providers map[string]Provider) []string {
	var errs []string
	prefix := fmt.Sprintf("agent %q", a.Name)

	if strings.TrimSpace(a.Name) == "" {
		errs = append(errs, "agent name must not be empty")
	}
	if a.AgentClass != "LlmAgent" && a.AgentClass != "AcpAgent" {
		errs = append(errs, fmt.Sprintf("%s: agent_class must be LlmAgent or AcpAgent", prefix))
	}
	if strings.TrimSpace(a.Instruction) == "" {
		errs = append(errs, fmt.Sprintf("%s: instruction must not be empty", prefix))
	}

	if a.AgentClass == "AcpAgent" {
		errs = append(errs, validateAcpAgent(prefix, a, providers)...)
		// Skip model validation for AcpAgent since it uses runtime instead.
		return errs
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
			} else if p.Type == ProviderTypeACP {
				errs = append(errs, fmt.Sprintf("%s: ACP providers require agent_class: AcpAgent", prefix))
			}
		}
	}
	if a.Runtime != "" {
		errs = append(errs, fmt.Sprintf("%s: runtime is only valid for AcpAgent", prefix))
	}
	if a.Confirmation != "" {
		errs = append(errs, fmt.Sprintf("%s: confirmation is only valid for AcpAgent", prefix))
	}
	if a.ExecutionMode != "" {
		errs = append(errs, fmt.Sprintf("%s: execution_mode is only valid for AcpAgent", prefix))
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

	if a.Name == "root_agent" {
		if strings.TrimSpace(a.GlobalInstruction) == "" {
			errs = append(errs, fmt.Sprintf("%s: global_instruction must not be empty", prefix))
		}
		if a.DelegatedGlobalInstruction != "" && strings.TrimSpace(a.DelegatedGlobalInstruction) == "" {
			errs = append(errs, fmt.Sprintf("%s: delegated_global_instruction must not be empty", prefix))
		}
	} else if a.GlobalInstruction != "" {
		errs = append(errs, fmt.Sprintf("%s: global_instruction is only allowed on root_agent", prefix))
	}
	if a.Name != "root_agent" && a.DelegatedGlobalInstruction != "" {
		errs = append(errs, fmt.Sprintf("%s: delegated_global_instruction is only allowed on root_agent", prefix))
	}

	return errs
}

func validateAgentTools(defs *Definitions) []string {
	var errs []string
	for _, owner := range defs.Agents {
		if len(owner.AgentTools) == 0 {
			continue
		}
		prefix := fmt.Sprintf("agent %q", owner.Name)
		if owner.Name != "root_agent" {
			errs = append(errs, fmt.Sprintf("%s: agent_tools is only allowed on root_agent", prefix))
		}
		if provider, ok := providerForAgent(owner, defs.Providers); ok && provider.Type != ProviderTypeOpenAICompatible {
			errs = append(errs, fmt.Sprintf("%s: agent_tools requires an %s root model", prefix, ProviderTypeOpenAICompatible))
		}

		uniqueNames, uniqueErrs := checkUnique(prefix, "agent tool", owner.AgentTools)
		errs = append(errs, uniqueErrs...)
		for _, name := range uniqueNames {
			target, exists := defs.Agents[name]
			if !exists {
				errs = append(errs, fmt.Sprintf("%s: unknown agent tool %q", prefix, name))
				continue
			}
			if target.Name == owner.Name {
				errs = append(errs, fmt.Sprintf("%s: cannot reference itself as an agent tool", prefix))
				continue
			}
			if target.Role != "" {
				errs = append(errs, fmt.Sprintf("agent tool %q: durable_session and role are not supported", name))
			}
			if IsReservedAgentName(target.Name) {
				errs = append(errs, ValidateAgentEligibility(target, defs.Providers)...)
			}
		}
	}
	return errs
}

func providerForAgent(agent AgentDef, providers map[string]Provider) (Provider, bool) {
	ref := agent.Model
	if agent.AgentClass == "AcpAgent" {
		ref = agent.Runtime
	}
	providerName, _, ok := splitModelReference(ref)
	if !ok {
		return Provider{}, false
	}
	provider, ok := providers[providerName]
	return provider, ok
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

func validateWorkflowTools(defs *Definitions) []string {
	var errs []string
	for _, owner := range defs.Agents {
		if len(owner.WorkflowTools) == 0 {
			continue
		}
		prefix := fmt.Sprintf("agent %q", owner.Name)
		if owner.Name != "root_agent" {
			errs = append(errs, fmt.Sprintf("%s: workflow_tools is only allowed on root_agent", prefix))
			continue
		}
		if provider, ok := providerForAgent(owner, defs.Providers); !ok || provider.Type != ProviderTypeOpenAICompatible {
			errs = append(errs, fmt.Sprintf("%s: workflow_tools requires an %s root model", prefix, ProviderTypeOpenAICompatible))
		}

		uniqueIDs, uniqueErrs := checkUnique(prefix, "workflow tool", owner.WorkflowTools)
		errs = append(errs, uniqueErrs...)
		for _, id := range uniqueIDs {
			if !agentNamePattern.MatchString(id) {
				errs = append(errs, fmt.Sprintf("%s: workflow tool id %q is not a valid identifier", prefix, id))
			}

		}
	}

	return errs
}

func checkUnique(prefix, label string, values []string) ([]string, []string) {
	field := strings.ReplaceAll(label, " ", "_") + "s"
	seen := make(map[string]struct{}, len(values))
	unique := make([]string, 0, len(values))
	var errs []string
	for index, value := range values {
		if strings.TrimSpace(value) == "" {
			errs = append(errs, fmt.Sprintf("%s: %s[%d] must not be empty", prefix, field, index))
			continue
		}
		if _, duplicate := seen[value]; duplicate {
			errs = append(errs, fmt.Sprintf("%s: duplicate %s %q", prefix, label, value))
			continue
		}
		seen[value] = struct{}{}
		unique = append(unique, value)
	}
	return unique, errs
}

func validateAcpAgent(prefix string, a AgentDef, providers map[string]Provider) []string {
	var errs []string

	if a.Runtime == "" {
		errs = append(errs, fmt.Sprintf("%s: runtime must not be empty", prefix))
	} else {
		providerName, profileName, ok := splitModelReference(a.Runtime)
		if !ok {
			errs = append(errs, fmt.Sprintf("%s: runtime must be provider/profile format", prefix))
		} else {
			if p, exists := providers[providerName]; !exists {
				errs = append(errs, fmt.Sprintf("%s: unknown runtime provider %q", prefix, providerName))
			} else if p.Type != ProviderTypeACP {
				errs = append(errs, fmt.Sprintf("%s: runtime provider %q must be type acp", prefix, providerName))
			} else if _, exists := p.Profiles[profileName]; !exists {
				errs = append(errs, fmt.Sprintf("%s: unknown runtime profile %q in provider %q", prefix, profileName, providerName))
			}
		}
	}
	if a.Model != "" {
		errs = append(errs, fmt.Sprintf("%s: model is not valid for AcpAgent (use runtime instead)", prefix))
	}
	if a.IncludeContents != "" {
		errs = append(errs, fmt.Sprintf("%s: include_contents is not valid for AcpAgent", prefix))
	}
	if a.Mode != "" {
		errs = append(errs, fmt.Sprintf("%s: mode is not valid for AcpAgent", prefix))
	}
	if a.DurableSession {
		errs = append(errs, fmt.Sprintf("%s: durable_session is not valid for AcpAgent", prefix))
	}
	if a.ToolScope != "" {
		errs = append(errs, fmt.Sprintf("%s: tool_scope is not valid for AcpAgent", prefix))
	}
	if len(a.AgentTools) > 0 {
		errs = append(errs, fmt.Sprintf("%s: agent_tools is not valid for AcpAgent", prefix))
	}
	if len(a.WorkflowTools) > 0 {
		errs = append(errs, fmt.Sprintf("%s: workflow_tools is not valid for AcpAgent", prefix))
	}
	if a.TimeoutSeconds < 0 || a.TimeoutSeconds > MaxACPTimeoutSeconds {
		errs = append(errs, fmt.Sprintf("%s: timeout_seconds must be between 0 and %d", prefix, MaxACPTimeoutSeconds))
	}
	switch a.ExecutionMode {
	case "", ExecutionModeForeground, ExecutionModeDurableJob:
	default:
		errs = append(errs, fmt.Sprintf("%s: execution_mode must be foreground or durable_job", prefix))
	}
	if a.Role != "" {
		errs = append(errs, fmt.Sprintf("%s: role is not valid for AcpAgent", prefix))
	}
	if a.GlobalInstruction != "" {
		errs = append(errs, fmt.Sprintf("%s: global_instruction is not valid for AcpAgent", prefix))
	}
	if a.DelegatedGlobalInstruction != "" {
		errs = append(errs, fmt.Sprintf("%s: delegated_global_instruction is not valid for AcpAgent", prefix))
	}
	switch a.Confirmation {
	case "required":
	default:
		errs = append(errs, fmt.Sprintf("%s: confirmation must be required", prefix))
	}

	return errs
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
	if strings.HasSuffix(strings.TrimRight(parsed.Path, "/"), "/chat/completions") {
		return fmt.Errorf("base_url must be an API root, not a concrete /chat/completions operation URL")
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

// ValidateProfileCapability validates the complete capability required to
// compose an OpenAI-compatible model context guard.
func ValidateProfileCapability(resolved *ResolvedModel) []string {
	if resolved == nil {
		return []string{"cannot validate profile capability for nil model"}
	}
	if resolved.Type() != ProviderTypeOpenAICompatible {
		return nil
	}
	prefix := fmt.Sprintf("openai_compatible model %q", resolved.Model)
	return validateTokenBudgets(prefix, &resolved.ContextWindowTokens, &resolved.MaxOutputTokens, &TokenCounterDef{
		Strategy: resolved.CounterStrategy,
		ID:       resolved.CounterID,
	}, true)
}

// ValidateAttachmentModelCapability validates that the profile selected for
// attachment_analyzer can both run the ADK visual pipeline and value media
// requests. P0 requires an openai_compatible profile with the versioned
// visual estimator (FR-18); anything else fails before serving traffic so a
// visual profile can never be served by an incapable counter.
func ValidateAttachmentModelCapability(resolved *ResolvedModel) []string {
	if resolved == nil {
		return []string{"attachment_analyzer resolved to no model"}
	}
	var errs []string
	if resolved.Type() != ProviderTypeOpenAICompatible {
		errs = append(errs, "attachment_analyzer cannot use an agent_cli or acp provider because image processing requires the ADK load_artifacts tool; select an openai_compatible profile")
		return errs
	}
	if resolved.CounterStrategy != "estimator" {
		errs = append(errs, fmt.Sprintf("attachment_analyzer profile %q must configure token_counter.strategy: estimator because image requests need a visual token estimate; byte_bound cannot value media", resolved.Model))
	} else if resolved.CounterID != VisualEstimatorID {
		errs = append(errs, fmt.Sprintf("attachment_analyzer profile %q must configure token_counter.id: %s; %q is not implemented for media", resolved.Model, VisualEstimatorID, resolved.CounterID))
	}
	return errs
}
