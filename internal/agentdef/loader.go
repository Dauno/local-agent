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
	"slices"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

var environmentNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

const maxContextWindowTokens = 10_000_000

type SemanticVersion struct {
	major int
	minor int
	patch int
}

// semanticVersionPattern is compiled once. Version parsing runs for every
// descriptor bound and again for every live version probe.
var semanticVersionPattern = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)$`)

func ParseSemanticVersion(value string) (SemanticVersion, bool) {
	match := semanticVersionPattern.FindStringSubmatch(strings.TrimSpace(value))
	if match == nil {
		return SemanticVersion{}, false
	}
	var version SemanticVersion
	for index, target := range []*int{&version.major, &version.minor, &version.patch} {
		parsed, err := strconv.Atoi(match[index+1])
		if err != nil {
			return SemanticVersion{}, false
		}
		*target = parsed
	}
	return version, true
}

func CompareSemanticVersions(left, right SemanticVersion) int {
	if left.major != right.major {
		return left.major - right.major
	}
	if left.minor != right.minor {
		return left.minor - right.minor
	}
	return left.patch - right.patch
}

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
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
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
		providerForbidden = []string{"base_url", "api_key_env", "headers", "command", "args", "shim"}
		profileForbidden = []string{"reasoning_effort", "extra_body", "generate_content_config", "config_options", "permission_option_kind"}
	case ProviderTypeOpenAICompatible:
		providerForbidden = []string{"executable", "version", "preconditions", "invocation", "stream", "session", "auth", "shim", "command", "args"}
		profileForbidden = []string{"agent", "approval", "variant", "config_options", "permission_option_kind"}
	default:
		return nil
	}

	prefix := fmt.Sprintf("provider %q", provider.Name)
	var errs []string
	for _, field := range providerForbidden {
		if mappingHasKey(root, field) {
			if provider.Type == ProviderTypeOpenAICompatible && field != "shim" {
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
	if candidate.AgentClass != "LlmAgent" {
		return fmt.Errorf("agent %q: agent_class must be LlmAgent", candidate.Name)
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
	if agent.AgentClass != "" && agent.AgentClass != "LlmAgent" {
		errs = append(errs, fmt.Sprintf("%s: agent_class must be LlmAgent", prefix))
	}
	// A durable job is delivered after the turn ends, so the user must have
	// approved it before it started.
	if agent.ExecutionMode == ExecutionModeDurableJob && agent.Confirmation != "required" {
		errs = append(errs, fmt.Sprintf("%s: durable_job requires confirmation: required", prefix))
	}
	if provider, ok := providerForAgent(agent, providers); ok {
		switch provider.Type {
		case ProviderTypeAgentCLI:
			if len(agent.ToolScope) > 0 {
				errs = append(errs, fmt.Sprintf("%s: tool_scope is not supported for %s agent tools", prefix, ProviderTypeAgentCLI))
			}
		case ProviderTypeOpenAICompatible:
			if !agent.ToolScope.Contains("invocation_scoped") {
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
		if agent.AgentClass != "LlmAgent" {
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
	default:
		errs = append(errs, fmt.Sprintf("%s: type must be %s or %s", prefix, ProviderTypeOpenAICompatible, ProviderTypeAgentCLI))
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
	if p.Shim != nil {
		errs = append(errs, fmt.Sprintf("%s: shim is invalid for %s providers; declare executable, version, invocation, and stream", prefix, ProviderTypeAgentCLI))
	}
	if strings.TrimSpace(p.Executable) == "" {
		errs = append(errs, fmt.Sprintf("%s: executable is required for %s providers", prefix, ProviderTypeAgentCLI))
	} else if strings.ContainsAny(p.Executable, "\r\n\x00") {
		errs = append(errs, fmt.Sprintf("%s: executable must be a single line", prefix))
	}
	if p.Version == nil {
		errs = append(errs, fmt.Sprintf("%s: version is required for %s providers", prefix, ProviderTypeAgentCLI))
	} else {
		errs = append(errs, validateCLIVersion(prefix, *p.Version)...)
	}
	if p.Invocation == nil {
		errs = append(errs, fmt.Sprintf("%s: invocation is required for %s providers", prefix, ProviderTypeAgentCLI))
	} else {
		errs = append(errs, validateCLIInvocation(prefix, *p.Invocation)...)
	}
	if p.Stream == nil {
		errs = append(errs, fmt.Sprintf("%s: stream is required for %s providers", prefix, ProviderTypeAgentCLI))
	} else {
		errs = append(errs, validateCLIStream(prefix, *p.Stream)...)
	}
	for index, condition := range p.Preconditions {
		conditionPrefix := fmt.Sprintf("%s: preconditions[%d]", prefix, index)
		if strings.TrimSpace(condition.Name) == "" || strings.TrimSpace(condition.Message) == "" || len(condition.Command) == 0 {
			errs = append(errs, conditionPrefix+" requires name, command, and message")
		}
		errs = append(errs, validateArgumentList(conditionPrefix+".command", condition.Command)...)
	}
	if p.Session != nil {
		errs = append(errs, validateCLISession(prefix, *p.Session, p.Invocation)...)
	}
	if p.Auth != nil {
		errs = append(errs, validateCLIAuth(prefix, *p.Auth)...)
	}
	return errs
}

// validateCLIAuth keeps the authentication command literal. A template would
// let a runtime value reach an argv that `doctor --live` executes, and the
// check needs no runtime value at all.
func validateCLIAuth(prefix string, auth CLIAuth) []string {
	var errs []string
	if len(auth.Command) == 0 {
		errs = append(errs, fmt.Sprintf("%s: auth.command must not be empty", prefix))
	}
	errs = append(errs, validateArgumentList(prefix+": auth.command", auth.Command)...)
	for index, argument := range auth.Command {
		if strings.Contains(argument, "{{") {
			errs = append(errs, fmt.Sprintf("%s: auth.command[%d] must not use a template", prefix, index))
		}
	}
	return errs
}

func validateCLIVersion(prefix string, version CLIVersion) []string {
	var errs []string
	if len(version.Command) == 0 {
		errs = append(errs, fmt.Sprintf("%s: version.command must not be empty", prefix))
	}
	errs = append(errs, validateArgumentList(prefix+".version.command", version.Command)...)
	pattern, err := regexp.Compile(version.Pattern)
	if err != nil || pattern.SubexpIndex("version") < 0 {
		errs = append(errs, fmt.Sprintf("%s: version.pattern must compile and define named group version", prefix))
	}
	if _, ok := ParseSemanticVersion(version.Min); !ok {
		errs = append(errs, fmt.Sprintf("%s: version.min must be a semantic version", prefix))
	}
	if version.Max != "" {
		maxVersion, maxOK := ParseSemanticVersion(version.Max)
		minVersion, minOK := ParseSemanticVersion(version.Min)
		if !maxOK {
			errs = append(errs, fmt.Sprintf("%s: version.max must be a semantic version", prefix))
		} else if minOK && CompareSemanticVersions(maxVersion, minVersion) < 0 {
			errs = append(errs, fmt.Sprintf("%s: version.max must not be less than version.min", prefix))
		}
	}
	return errs
}

func validateCLIInvocation(prefix string, invocation CLIInvocation) []string {
	var errs []string
	if invocation.Prompt != "stdin" {
		errs = append(errs, fmt.Sprintf("%s: invocation.prompt must be stdin", prefix))
	}
	if len(invocation.Args) == 0 {
		errs = append(errs, fmt.Sprintf("%s: invocation.args must not be empty", prefix))
	}
	errs = append(errs, validateArgumentList(prefix+".invocation.args_prefix", invocation.ArgsPrefix)...)
	errs = append(errs, validateArgumentList(prefix+".invocation.args", invocation.Args)...)
	if system := invocation.SystemPrompt; system != nil {
		if strings.TrimSpace(system.Flag) == "" {
			errs = append(errs, fmt.Sprintf("%s: invocation.system_prompt.flag must not be empty", prefix))
		}
		errs = append(errs, validateArgumentList(prefix+".invocation.system_prompt.flag", []string{system.Flag})...)
	}
	for name, option := range invocation.Options {
		optionPrefix := fmt.Sprintf("%s: invocation.options.%s", prefix, name)
		forms := 0
		if option.Flag != "" {
			forms++
		}
		if len(option.Template) > 0 {
			forms++
		}
		if len(option.Values) > 0 {
			forms++
		}
		if forms != 1 {
			errs = append(errs, optionPrefix+" must declare exactly one of flag, template, or value mappings")
		}
		if option.Position != "" && option.Position != "prefix" {
			errs = append(errs, optionPrefix+".position must be prefix")
		}
		errs = append(errs, validateArgumentList(optionPrefix+".template", option.Template)...)
		for value, mapping := range option.Values {
			if strings.TrimSpace(value) == "" || (len(mapping.Substitutions) == 0 && len(mapping.Args) == 0) {
				errs = append(errs, optionPrefix+" value mappings must not be empty")
			}
			errs = append(errs, validateArgumentList(optionPrefix+"."+value+".args", mapping.Args)...)
		}
	}
	if workspace := invocation.Workspace; workspace != nil {
		if workspace.CWDFlag == "" {
			errs = append(errs, fmt.Sprintf("%s: invocation.workspace.cwd_flag must not be empty", prefix))
		}
		if workspace.AddDirFlag != "" && len(workspace.AddDirWhen) == 0 {
			errs = append(errs, fmt.Sprintf("%s: invocation.workspace.add_dir_when is required with add_dir_flag", prefix))
		}
	}
	return errs
}

func validateCLIStream(prefix string, stream CLIStream) []string {
	var errs []string
	if stream.Format != "ndjson" {
		errs = append(errs, fmt.Sprintf("%s: stream.format must be ndjson", prefix))
	}
	if len(stream.FinalText.When) == 0 || stream.FinalText.Path == "" {
		errs = append(errs, fmt.Sprintf("%s: stream.final_text requires when and path", prefix))
	}
	if len(stream.Failure.WhenAny) == 0 {
		errs = append(errs, fmt.Sprintf("%s: stream.failure.when_any must not be empty", prefix))
	}
	if len(stream.TerminalTypes) == 0 {
		errs = append(errs, fmt.Sprintf("%s: stream.terminal_types must not be empty", prefix))
	}
	if stream.Activity == nil {
		errs = append(errs, fmt.Sprintf("%s: stream.activity is required", prefix))
	} else {
		if len(stream.Activity.When) == 0 || stream.Activity.TypeField == "" {
			errs = append(errs, fmt.Sprintf("%s: stream.activity requires when and type_field", prefix))
		}
		if stream.Activity.DiscardTypes == nil {
			errs = append(errs, fmt.Sprintf("%s: stream.activity.discard_types is required", prefix))
		}
	}
	return errs
}

func validateCLISession(prefix string, session CLISession, invocation *CLIInvocation) []string {
	var errs []string
	if len(session.ID.When) == 0 || session.ID.Path == "" {
		errs = append(errs, fmt.Sprintf("%s: session.id requires when and path", prefix))
	}
	if !strings.Contains(session.Transcript.PathGlob, "{{session_id}}") {
		errs = append(errs, fmt.Sprintf("%s: session.transcript.path_glob must contain {{session_id}}", prefix))
	}
	fullResume := len(session.Resume.Args) > 0 || len(session.Resume.ArgsPrefix) > 0
	if len(session.Resume.ResumeFlag) == 0 && !fullResume {
		errs = append(errs, fmt.Sprintf("%s: session.resume requires resume_flag or args", prefix))
	}
	if len(session.Resume.ResumeFlag) > 0 && fullResume {
		errs = append(errs, fmt.Sprintf("%s: session.resume.resume_flag and args are exclusive", prefix))
	}
	if invocation != nil && containsArgument(invocation.Args, "--ephemeral") {
		errs = append(errs, fmt.Sprintf("%s: session requires an invocation that persists sessions", prefix))
	}
	return errs
}

func validateArgumentList(prefix string, args []string) []string {
	var errs []string
	for index, arg := range args {
		if strings.TrimSpace(arg) == "" || strings.ContainsAny(arg, "\r\n\x00") {
			errs = append(errs, fmt.Sprintf("%s[%d] must be a non-empty single line", prefix, index))
		}
	}
	return errs
}

func containsArgument(args []string, want string) bool {
	return slices.Contains(args, want)
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

func validateProfile(providerPrefix, providerType, name string, profile Profile) []string {
	var errs []string
	prefix := fmt.Sprintf("%s profile %q", providerPrefix, name)

	if strings.TrimSpace(profile.Model) == "" {
		errs = append(errs, fmt.Sprintf("%s: model must not be empty", prefix))
	}
	if profile.ResultHandles.MaxDirectInlineBytes < 0 || profile.ResultHandles.MaxDirectInlineBytes > HardMaxDirectInlineBytes {
		errs = append(errs, fmt.Sprintf("%s: result_handles.max_direct_inline_bytes must be between 0 and %d", prefix, HardMaxDirectInlineBytes))
	}

	switch providerType {
	case ProviderTypeAgentCLI:
		if profile.ReasoningEffort != "" {
			errs = append(errs, fmt.Sprintf("%s: reasoning_effort is invalid for %s profiles", prefix, providerType))
		}
		if len(profile.ExtraBody) > 0 {
			errs = append(errs, fmt.Sprintf("%s: extra_body is invalid for %s profiles", prefix, providerType))
		}
		if profile.GenerateContentConfig != nil {
			errs = append(errs, fmt.Sprintf("%s: generate_content_config is invalid for %s profiles", prefix, providerType))
		}
		{
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
	if a.ContextBudget != nil {
		if a.ContextBudget.MaxRequestPercent != 0 && (a.ContextBudget.MaxRequestPercent < 20 || a.ContextBudget.MaxRequestPercent > 80) {
			errs = append(errs, fmt.Sprintf("%s: context_budget.max_request_percent must be between 20 and 80", prefix))
		}
		if a.Name == "root_agent" {
			errs = append(errs, fmt.Sprintf("%s: context_budget is only valid for delegated LlmAgent children", prefix))
		}
	}

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
	if a.Runtime != "" {
		errs = append(errs, fmt.Sprintf("%s: runtime is not supported; use model with an agent_cli provider", prefix))
	}
	// Durable execution is available to agent_cli leaves as well as ACP ones.
	// Both are external agents that outlive one model call, so both may declare
	// a confirmation gate and a durable execution mode. Every other agent class
	// runs inside the model call and may declare neither.
	if isAgentCLIModel(a.Model, providers) {
		errs = append(errs, validateExternalAgentExecution(prefix, a)...)
	} else {
		if a.Confirmation != "" {
			errs = append(errs, fmt.Sprintf("%s: confirmation is only valid for agent_cli agents", prefix))
		}
		if a.ExecutionMode != "" {
			errs = append(errs, fmt.Sprintf("%s: execution_mode is only valid for agent_cli agents", prefix))
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
	if a.ToolScope != nil {
		for _, scope := range a.ToolScope {
			if strings.TrimSpace(scope) == "" {
				errs = append(errs, fmt.Sprintf("%s: tool_scope entries must not be empty", prefix))
				continue
			}
			if scope != "invocation_scoped" && !validAgentNamePattern.MatchString(scope) {
				errs = append(errs, fmt.Sprintf("%s: tool_scope entry %q must be invocation_scoped or a valid declarative tool name", prefix, scope))
			}
		}
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
	providerName, _, ok := splitModelReference(agent.Model)
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
	for i := range len(value) {
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
		errs = append(
			errs,
			fmt.Sprintf(
				"attachment_analyzer profile %q must configure token_counter.strategy: estimator because image requests need a visual token estimate; byte_bound cannot value media",
				resolved.Model,
			),
		)
	} else if resolved.CounterID != VisualEstimatorID {
		errs = append(errs, fmt.Sprintf("attachment_analyzer profile %q must configure token_counter.id: %s; %q is not implemented for media", resolved.Model, VisualEstimatorID, resolved.CounterID))
	}
	return errs
}

// isAgentCLIModel reports whether a model reference resolves to an agent_cli
// provider. It answers false for an unresolvable reference, so the caller's
// own model validation reports that problem instead of this one.
func isAgentCLIModel(reference string, providers map[string]Provider) bool {
	providerName, _, ok := splitModelReference(reference)
	if !ok {
		return false
	}
	provider, exists := providers[providerName]
	return exists && provider.Type == ProviderTypeAgentCLI
}

// validateExternalAgentExecution checks the durable-execution fields shared by
// the two external-agent families. The bounds match validateAcpAgent so a leaf
// cannot gain a longer timeout by switching provider family.
func validateExternalAgentExecution(prefix string, a AgentDef) []string {
	var errs []string
	switch a.Confirmation {
	case "", "required":
	default:
		errs = append(errs, fmt.Sprintf("%s: confirmation must be required", prefix))
	}
	switch a.ExecutionMode {
	case "", ExecutionModeForeground, ExecutionModeDurableJob:
	default:
		errs = append(errs, fmt.Sprintf("%s: execution_mode must be foreground or durable_job", prefix))
	}
	if a.TimeoutSeconds < 0 || a.TimeoutSeconds > MaxExternalAgentTimeoutSeconds {
		errs = append(errs, fmt.Sprintf("%s: timeout_seconds must be between 0 and %d", prefix, MaxExternalAgentTimeoutSeconds))
	}
	// A durable job is delivered to Slack after the root turn ends, so the user
	// must have approved it before it started.
	if a.ExecutionMode == ExecutionModeDurableJob && a.Confirmation != "required" {
		errs = append(errs, fmt.Sprintf("%s: execution_mode durable_job requires confirmation: required", prefix))
	}
	return errs
}
