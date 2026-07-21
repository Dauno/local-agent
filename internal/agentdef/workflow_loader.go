package agentdef

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

var agentNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

const (
	maxWorkflowNodes     = 50
	maxWorkflowDepth     = 15
	maxLoopIterations    = 100
	maxLoopIterationsMin = 1
)

type WorkflowBlueprint struct {
	ID          string
	Description string
	Root        AgentDocument
	Documents   map[string]AgentDocument
}

var workflowReadOnlyToolNames = []string{
	"list_messages",
	"list_repos",
	"list_directory",
	"read_file",
	"list_worktrees",
}

var workflowReadOnlyTools = func() map[string]struct{} {
	result := make(map[string]struct{}, len(workflowReadOnlyToolNames))
	for _, name := range workflowReadOnlyToolNames {
		result[name] = struct{}{}
	}
	return result
}()

// LoadWorkflow loads and validates a complete workflow tree from the given
// workflow root file. It returns the validated blueprint or an error with
// source context.
func (d *Definitions) LoadWorkflow(stateDir, workflowID string) (*WorkflowBlueprint, error) {
	if !agentNamePattern.MatchString(workflowID) {
		return nil, fmt.Errorf("workflow %q: id is not a valid identifier", workflowID)
	}
	workflowDir := filepath.Join(stateDir, "workflows", workflowID)
	rootFile := filepath.Join(workflowDir, "root_agent.yaml")
	canonicalDir, err := filepath.EvalSymlinks(workflowDir)
	if err != nil {
		return nil, fmt.Errorf("workflow %q: resolve directory: %w", workflowID, err)
	}
	canonicalDir = filepath.Clean(canonicalDir)

	documents := make(map[string]AgentDocument)
	root, err := loadWorkflowDocument(rootFile, canonicalDir, documents, make(map[string]int), nil, 1)
	if err != nil {
		return nil, fmt.Errorf("workflow %q: %w", workflowID, err)
	}

	ordered := (&WorkflowBlueprint{Root: *root, Documents: documents}).OrderedDocuments()
	namePaths := make(map[string]string, len(ordered))
	for _, doc := range ordered {
		if firstPath, duplicate := namePaths[doc.Name]; duplicate {
			return nil, fmt.Errorf("workflow %q: duplicate agent name %q in %q and %q", workflowID, doc.Name, firstPath, doc.Path)
		}
		namePaths[doc.Name] = doc.Path
	}

	if err := validateWorkflowBlueprint(*root, documents, d); err != nil {
		return nil, fmt.Errorf("workflow %q: %w", workflowID, err)
	}

	return &WorkflowBlueprint{
		ID:          workflowID,
		Description: root.Description,
		Root:        *root,
		Documents:   documents,
	}, nil
}

// OrderedDocuments returns the workflow tree in declaration order.
func (b *WorkflowBlueprint) OrderedDocuments() []AgentDocument {
	if b == nil {
		return nil
	}
	result := make([]AgentDocument, 0, len(b.Documents))
	var walk func(AgentDocument)
	walk = func(doc AgentDocument) {
		result = append(result, doc)
		for _, ref := range doc.SubAgents {
			if child, ok := b.Documents[ref.Path]; ok {
				walk(child)
			}
		}
	}
	walk(b.Root)
	return result
}

func loadWorkflowDocument(path string, canonicalRoot string, documents map[string]AgentDocument, active map[string]int, stack []string, depth int) (*AgentDocument, error) {
	if depth > maxWorkflowDepth {
		return nil, fmt.Errorf("exceeded maximum depth (%d) at %q", maxWorkflowDepth, path)
	}
	if filepath.Ext(path) != ".yaml" {
		return nil, fmt.Errorf("referenced target %q must be a .yaml file", path)
	}

	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, fmt.Errorf("resolve path %q: %w", path, err)
	}
	canonical = filepath.Clean(canonical)
	if filepath.Ext(canonical) != ".yaml" {
		return nil, fmt.Errorf("referenced target %q must resolve to a .yaml file", path)
	}

	if !pathWithin(canonicalRoot, canonical) {
		return nil, fmt.Errorf("file %q escapes workflow directory", path)
	}
	if cycleStart, exists := active[canonical]; exists {
		chain := append(append([]string{}, stack[cycleStart:]...), canonical)
		for index := range chain {
			if relative, relErr := filepath.Rel(canonicalRoot, chain[index]); relErr == nil {
				chain[index] = filepath.ToSlash(relative)
			}
		}
		return nil, fmt.Errorf("cyclic sub_agents reference: %s", strings.Join(chain, " -> "))
	}
	if dup, exists := documents[canonical]; exists {
		return nil, fmt.Errorf("duplicate reference to %q (first loaded as %q)", path, dup.Path)
	}
	if len(documents)+len(active) >= maxWorkflowNodes {
		return nil, fmt.Errorf("too many nodes (max %d) at %q", maxWorkflowNodes, path)
	}

	info, err := os.Stat(canonical)
	if err != nil {
		return nil, fmt.Errorf("stat %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%q is not a regular file", path)
	}

	data, err := os.ReadFile(canonical)
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", path, err)
	}

	doc, err := decodeWorkflowDocument(data, canonical, path)
	if err != nil {
		return nil, err
	}
	active[canonical] = len(stack)
	stack = append(stack, canonical)
	defer delete(active, canonical)

	for index := range doc.SubAgents {
		ref := &doc.SubAgents[index]
		if filepath.IsAbs(ref.ConfigPath) {
			return nil, fmt.Errorf("%q: sub_agents[%d].config_path must be relative", path, index)
		}
		refPath := filepath.Join(filepath.Dir(canonical), ref.ConfigPath)
		child, err := loadWorkflowDocument(refPath, canonicalRoot, documents, active, stack, depth+1)
		if err != nil {
			return nil, err
		}
		ref.Path = child.Path
	}

	documents[canonical] = doc
	return &doc, nil
}

func pathWithin(root, target string) bool {
	relative, err := filepath.Rel(root, target)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

type workflowRawDoc struct {
	AgentClass  string `yaml:"agent_class"`
	Name        string `yaml:"name"`
	Description string `yaml:"description,omitempty"`
	Model       string `yaml:"model,omitempty"`
	Instruction string `yaml:"instruction,omitempty"`
	Runtime     string `yaml:"runtime,omitempty"`

	IncludeContents string `yaml:"include_contents,omitempty"`
	OutputKey       string `yaml:"output_key,omitempty"`
	OutputSchema    string `yaml:"output_schema,omitempty"`
	Project         string `yaml:"project,omitempty"`

	AdditionalDirectories []string `yaml:"additional_directories,omitempty"`

	Tools []struct {
		Name string         `yaml:"name"`
		Args map[string]any `yaml:"args,omitempty"`
	} `yaml:"tools,omitempty"`

	SubAgents []struct {
		ConfigPath string `yaml:"config_path"`
		Code       string `yaml:"code,omitempty"`
	} `yaml:"sub_agents,omitempty"`

	MaxIterations int `yaml:"max_iterations,omitempty"`

	DisallowTransferToParent bool `yaml:"disallow_transfer_to_parent"`
	DisallowTransferToPeers  bool `yaml:"disallow_transfer_to_peers"`
}

var unsupportedWorkflowFields = map[string]string{
	"model_code":              "model_code",
	"static_instruction":      "static_instruction",
	"input_schema":            "input_schema",
	"before_agent_callbacks":  "before_agent_callbacks",
	"after_agent_callbacks":   "after_agent_callbacks",
	"before_model_callbacks":  "before_model_callbacks",
	"after_model_callbacks":   "after_model_callbacks",
	"before_tool_callbacks":   "before_tool_callbacks",
	"after_tool_callbacks":    "after_tool_callbacks",
	"generate_content_config": "generate_content_config",
	"agent_tools":             "agent_tools",
	"workflow_tools":          "workflow_tools",
	"durable_session":         "durable_session",
	"role":                    "role",
	"tool_scope":              "tool_scope",
	"global_instruction":      "global_instruction",
	"mode":                    "mode",
	"timeout_seconds":         "timeout_seconds",
}

func decodeWorkflowDocument(data []byte, canonicalPath, displayPath string) (AgentDocument, error) {
	fields, err := inspectWorkflowDocument(data, displayPath)
	if err != nil {
		return AgentDocument{}, err
	}
	var raw workflowRawDoc
	if err := decodeStrictYAMLWorkflow(data, &raw, displayPath); err != nil {
		return AgentDocument{}, err
	}

	agentClass, ok := agentClasses[raw.AgentClass]
	if raw.AgentClass == "" {
		agentClass = AgentClassLLM
	} else if !ok {
		return AgentDocument{}, fmt.Errorf("%q: unsupported agent_class %q", displayPath, raw.AgentClass)
	}
	if err := validateWorkflowClassFields(fields, agentClass, displayPath); err != nil {
		return AgentDocument{}, err
	}

	if !agentNamePattern.MatchString(raw.Name) {
		return AgentDocument{}, fmt.Errorf("%q: name %q is not a valid identifier", displayPath, raw.Name)
	}
	if raw.Name == "user" {
		return AgentDocument{}, fmt.Errorf("%q: name %q is reserved by ADK", displayPath, raw.Name)
	}

	doc := AgentDocument{
		Path:        canonicalPath,
		AgentClass:  agentClass,
		Name:        raw.Name,
		Description: raw.Description,
	}

	for _, ref := range raw.SubAgents {
		if ref.Code != "" {
			return AgentDocument{}, fmt.Errorf("%q: sub_agents code reference is not supported by local-agent", displayPath)
		}
		if strings.TrimSpace(ref.ConfigPath) == "" {
			return AgentDocument{}, fmt.Errorf("%q: sub_agents[].config_path must not be empty", displayPath)
		}
		doc.SubAgents = append(doc.SubAgents, AgentRef{ConfigPath: ref.ConfigPath})
	}

	switch agentClass {
	case AgentClassLLM:
		llm := &LLMAgentDocument{
			Model:                    raw.Model,
			Instruction:              raw.Instruction,
			IncludeContents:          raw.IncludeContents,
			OutputKey:                raw.OutputKey,
			DisallowTransferToParent: raw.DisallowTransferToParent,
			DisallowTransferToPeers:  raw.DisallowTransferToPeers,
		}
		for _, t := range raw.Tools {
			llm.Tools = append(llm.Tools, ToolRef{Name: t.Name, Args: t.Args})
		}
		doc.LLM = llm
	case AgentClassAcp:
		doc.ACP = &AcpAgentDocument{
			Runtime:               raw.Runtime,
			Instruction:           raw.Instruction,
			Project:               raw.Project,
			AdditionalDirectories: raw.AdditionalDirectories,
			OutputKey:             raw.OutputKey,
			OutputSchema:          raw.OutputSchema,
		}
	case AgentClassSequential:
	case AgentClassLoop:
		if raw.MaxIterations <= 0 {
			return AgentDocument{}, fmt.Errorf("%q: max_iterations must be positive", displayPath)
		}
		doc.Loop = &LoopAgentDocument{MaxIterations: raw.MaxIterations}
	}

	return doc, nil
}

func decodeStrictYAMLWorkflow(data []byte, target *workflowRawDoc, displayPath string) error {
	if err := decodeStrictYAML(data, target); err != nil {
		return fmt.Errorf("parse %q: %w", displayPath, err)
	}
	return nil
}

func inspectWorkflowDocument(data []byte, displayPath string) (map[string]struct{}, error) {
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	var document yaml.Node
	if err := dec.Decode(&document); err != nil {
		return nil, fmt.Errorf("parse %q: %w", displayPath, err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("parse %q: expected one YAML document", displayPath)
		}
		return nil, fmt.Errorf("parse %q: %w", displayPath, err)
	}
	root := yamlDocumentRoot(&document)
	if root == nil || root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("parse %q: workflow document must be a mapping", displayPath)
	}

	fields := make(map[string]struct{}, len(root.Content)/2)
	for index := 0; index+1 < len(root.Content); index += 2 {
		key := root.Content[index].Value
		fields[key] = struct{}{}
		if known, unsupported := unsupportedWorkflowFields[key]; unsupported {
			return nil, fmt.Errorf("workflow %q: %s is an ADK field but is not supported by local-agent", displayPath, known)
		}
	}
	if subAgents := mappingValue(root, "sub_agents"); subAgents != nil && subAgents.Kind == yaml.SequenceNode {
		for _, child := range subAgents.Content {
			if mappingHasKey(child, "code") {
				return nil, fmt.Errorf("workflow %q: sub_agents code reference is not supported by local-agent", displayPath)
			}
		}
	}
	return fields, nil
}

func validateWorkflowClassFields(fields map[string]struct{}, class AgentClass, displayPath string) error {
	allowed := map[string]struct{}{
		"agent_class": {},
		"name":        {},
		"description": {},
	}
	switch class {
	case AgentClassLLM:
		for _, field := range []string{"model", "instruction", "include_contents", "output_key", "tools", "disallow_transfer_to_parent", "disallow_transfer_to_peers"} {
			allowed[field] = struct{}{}
		}
	case AgentClassAcp:
		for _, field := range []string{"runtime", "instruction", "output_key", "output_schema", "project", "additional_directories"} {
			allowed[field] = struct{}{}
		}
	case AgentClassSequential:
		allowed["sub_agents"] = struct{}{}
	case AgentClassLoop:
		allowed["sub_agents"] = struct{}{}
		allowed["max_iterations"] = struct{}{}
	}
	for field := range fields {
		if _, ok := allowed[field]; !ok {
			return fmt.Errorf("%q: field %s is not valid for %s", displayPath, field, class)
		}
	}
	return nil
}

func validateWorkflowBlueprint(root AgentDocument, documents map[string]AgentDocument, defs *Definitions) error {
	var errs []string
	rootDir := filepath.Dir(root.Path)

	if strings.TrimSpace(root.Description) == "" {
		errs = append(errs, fmt.Sprintf("%s: description must not be empty on workflow root", workflowDisplayPath(rootDir, root.Path)))
	}

	// Build ancestor map for exit_loop validation.
	loopAncestors := buildLoopAncestorMap(root, documents)

	ordered := (&WorkflowBlueprint{Root: root, Documents: documents}).OrderedDocuments()
	for _, doc := range ordered {
		isLoopDescendant := loopAncestors[doc.Name]
		errs = append(errs, validateWorkflowNode(doc, defs, isLoopDescendant, workflowDisplayPath(rootDir, doc.Path))...)
	}

	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

func workflowDisplayPath(rootDir, path string) string {
	relative, err := filepath.Rel(rootDir, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(relative)
}

func buildLoopAncestorMap(root AgentDocument, documents map[string]AgentDocument) map[string]bool {
	result := make(map[string]bool)
	var walk func(doc AgentDocument, hasLoopAncestor bool)
	walk = func(doc AgentDocument, hasLoopAncestor bool) {
		if doc.AgentClass == AgentClassLoop {
			hasLoopAncestor = true
		}
		result[doc.Name] = hasLoopAncestor
		for _, ref := range doc.SubAgents {
			if child, ok := documents[ref.Path]; ok {
				walk(child, hasLoopAncestor)
			}
		}
	}
	walk(root, false)
	return result
}

func validateWorkflowNode(doc AgentDocument, defs *Definitions, isLoopDescendant bool, sourcePath string) []string {
	var errs []string
	prefix := fmt.Sprintf("%s agent %q", sourcePath, doc.Name)

	switch doc.AgentClass {
	case AgentClassLLM:
		if doc.LLM == nil {
			errs = append(errs, fmt.Sprintf("%s: LLM agent document is missing", prefix))
			return errs
		}
		if strings.TrimSpace(doc.LLM.Model) == "" {
			errs = append(errs, fmt.Sprintf("%s: model must not be empty", prefix))
		} else {
			providerName, profileName, ok := splitModelReference(doc.LLM.Model)
			if !ok {
				errs = append(errs, fmt.Sprintf("%s: model must be provider/profile format", prefix))
			} else {
				p, exists := defs.Providers[providerName]
				if !exists {
					errs = append(errs, fmt.Sprintf("%s: unknown provider %q", prefix, providerName))
				} else if _, exists := p.Profiles[profileName]; !exists {
					errs = append(errs, fmt.Sprintf("%s: unknown profile %q in provider %q", prefix, profileName, providerName))
				} else {
					if p.Type == ProviderTypeACP {
						errs = append(errs, fmt.Sprintf("%s: ACP providers require agent_class: AcpAgent", prefix))
					}
					if p.Type == ProviderTypeAgentCLI {
						if doc.LLM.IncludeContents != "none" {
							errs = append(errs, fmt.Sprintf("%s: include_contents must be none for %s nodes", prefix, ProviderTypeAgentCLI))
						}
						if len(doc.LLM.Tools) > 0 {
							errs = append(errs, fmt.Sprintf("%s: tools are not supported for %s nodes", prefix, ProviderTypeAgentCLI))
						}
					}
				}
			}
		}
		if strings.TrimSpace(doc.LLM.Instruction) == "" {
			errs = append(errs, fmt.Sprintf("%s: instruction must not be empty", prefix))
		}
		switch doc.LLM.IncludeContents {
		case "", "default", "none":
		default:
			errs = append(errs, fmt.Sprintf("%s: include_contents must be default or none", prefix))
		}
		seenTools := make(map[string]struct{}, len(doc.LLM.Tools))
		for _, toolRef := range doc.LLM.Tools {
			if _, duplicate := seenTools[toolRef.Name]; duplicate {
				errs = append(errs, fmt.Sprintf("%s: duplicate tool %q", prefix, toolRef.Name))
				continue
			}
			seenTools[toolRef.Name] = struct{}{}
			if err := validateWorkflowTool(toolRef, isLoopDescendant); err != nil {
				errs = append(errs, fmt.Sprintf("%s: %s", prefix, err))
			}
		}
	case AgentClassAcp:
		if doc.ACP == nil {
			errs = append(errs, fmt.Sprintf("%s: AcpAgent document is missing", prefix))
			return errs
		}
		if strings.TrimSpace(doc.ACP.Runtime) == "" {
			errs = append(errs, fmt.Sprintf("%s: runtime must not be empty", prefix))
		} else {
			providerName, profileName, ok := splitModelReference(doc.ACP.Runtime)
			if !ok {
				errs = append(errs, fmt.Sprintf("%s: runtime must be provider/profile format", prefix))
			} else {
				p, exists := defs.Providers[providerName]
				if !exists {
					errs = append(errs, fmt.Sprintf("%s: unknown runtime provider %q", prefix, providerName))
				} else if p.Type != ProviderTypeACP {
					errs = append(errs, fmt.Sprintf("%s: runtime provider %q must be type acp", prefix, providerName))
				} else if _, exists := p.Profiles[profileName]; !exists {
					errs = append(errs, fmt.Sprintf("%s: unknown runtime profile %q in provider %q", prefix, profileName, providerName))
				}
			}
		}
		if strings.TrimSpace(doc.ACP.Instruction) == "" {
			errs = append(errs, fmt.Sprintf("%s: instruction must not be empty", prefix))
		}
		if doc.ACP.Project != "{target_project}" {
			errs = append(errs, fmt.Sprintf("%s: project must be the trusted {target_project} state template", prefix))
		}
		for _, directory := range doc.ACP.AdditionalDirectories {
			if directory != "{worktree_root}" {
				errs = append(errs, fmt.Sprintf("%s: additional_directories may only contain {worktree_root}", prefix))
			}
		}
		if doc.ACP.OutputSchema != "" && doc.ACP.OutputSchema != "git_delivery_result" {
			errs = append(errs, fmt.Sprintf("%s: output_schema must be git_delivery_result", prefix))
		}
		if doc.ACP.OutputSchema != "" && strings.TrimSpace(doc.ACP.OutputKey) == "" {
			errs = append(errs, fmt.Sprintf("%s: output_key is required when output_schema is set", prefix))
		}
	case AgentClassSequential:
		if len(doc.SubAgents) == 0 {
			errs = append(errs, fmt.Sprintf("%s: SequentialAgent requires at least one sub_agent", prefix))
		}
	case AgentClassLoop:
		if len(doc.SubAgents) == 0 {
			errs = append(errs, fmt.Sprintf("%s: LoopAgent requires at least one sub_agent", prefix))
		}
		if doc.Loop == nil {
			errs = append(errs, fmt.Sprintf("%s: LoopAgent document is missing", prefix))
		} else if doc.Loop.MaxIterations < maxLoopIterationsMin || doc.Loop.MaxIterations > maxLoopIterations {
			errs = append(errs, fmt.Sprintf("%s: max_iterations must be between %d and %d", prefix, maxLoopIterationsMin, maxLoopIterations))
		}
	default:
		errs = append(errs, fmt.Sprintf("%s: unsupported agent class %q", prefix, doc.AgentClass))
	}

	return errs
}

func validateWorkflowTool(toolRef ToolRef, isLoopDescendant bool) error {
	if strings.TrimSpace(toolRef.Name) == "" {
		return fmt.Errorf("tool name must not be empty")
	}
	if len(toolRef.Args) > 0 {
		return fmt.Errorf("tool %q: arguments are not supported for workflow tools", toolRef.Name)
	}
	if toolRef.Name == "exit_loop" {
		if !isLoopDescendant {
			return fmt.Errorf("exit_loop is only allowed inside a LoopAgent subtree")
		}
		return nil
	}
	if _, allowed := workflowReadOnlyTools[toolRef.Name]; !allowed {
		return fmt.Errorf("tool %q is not registered for workflow use", toolRef.Name)
	}
	return nil
}

// ValidateWorkflowComposition checks root tool-name collisions and whether all
// requested application tools are available under the current sandbox policy.
func (d *Definitions) ValidateWorkflowComposition(root AgentDef, blueprints []*WorkflowBlueprint, sandboxEnabled bool) error {
	seen := make(map[string]string)
	for _, name := range workflowReadOnlyToolNames {
		seen[name] = "direct application tool"
	}
	for _, name := range root.AgentTools {
		definition, ok := d.Agents[name]
		if !ok {
			continue
		}
		if owner, duplicate := seen[definition.Name]; duplicate {
			return fmt.Errorf("agent tool %q collides with %s", definition.Name, owner)
		}
		seen[definition.Name] = "agent tool"
	}
	for _, blueprint := range blueprints {
		if blueprint == nil {
			return fmt.Errorf("workflow blueprint is nil")
		}
		name := blueprint.Root.Name
		if owner, duplicate := seen[name]; duplicate {
			return fmt.Errorf("workflow %q root tool %q collides with %s", blueprint.ID, name, owner)
		}
		seen[name] = fmt.Sprintf("workflow %q", blueprint.ID)
		for _, doc := range blueprint.OrderedDocuments() {
			if doc.LLM == nil {
				continue
			}
			for _, toolRef := range doc.LLM.Tools {
				if toolRef.Name != "exit_loop" && toolRef.Name != "list_messages" && !sandboxEnabled {
					return fmt.Errorf("workflow %q agent %q: tool %q requires sandbox.enabled", blueprint.ID, doc.Name, toolRef.Name)
				}
			}
		}
	}
	return nil
}
