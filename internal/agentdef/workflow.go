package agentdef

type AgentClass string

const (
	AgentClassLLM        AgentClass = "LlmAgent"
	AgentClassSequential AgentClass = "SequentialAgent"
	AgentClassLoop       AgentClass = "LoopAgent"
	AgentClassAcp        AgentClass = "AcpAgent"
)

var agentClasses = map[string]AgentClass{
	"LlmAgent":        AgentClassLLM,
	"SequentialAgent": AgentClassSequential,
	"LoopAgent":       AgentClassLoop,
	"AcpAgent":        AgentClassAcp,
}

type AgentDocument struct {
	Path        string
	AgentClass  AgentClass
	Name        string
	Description string
	SubAgents   []AgentRef
	LLM         *LLMAgentDocument
	Loop        *LoopAgentDocument
	ACP         *AcpAgentDocument
}

type AcpAgentDocument struct {
	Runtime               string
	Instruction           string
	Project               string
	AdditionalDirectories []string
	OutputKey             string
	OutputSchema          string
}

type AgentRef struct {
	ConfigPath string `yaml:"config_path"`
	Path       string `yaml:"-"`
}

type ToolRef struct {
	Name string         `yaml:"name"`
	Args map[string]any `yaml:"args,omitempty"`
}

type LLMAgentDocument struct {
	Model                    string
	Instruction              string
	IncludeContents          string
	OutputKey                string
	Tools                    []ToolRef
	DisallowTransferToParent bool
	DisallowTransferToPeers  bool
}

type LoopAgentDocument struct {
	MaxIterations int
}
