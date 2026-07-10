// Package config owns the non-sensitive, typed project configuration.
package config

const (
	DefaultProjectStateDir = ".local-agent"
	DefaultDatabaseFile    = ".local-agent/local-agent.db"
	DefaultConfigFile      = ".local-agent/config.yaml"
	DefaultManifestFile    = ".local-agent/app-manifest.local.yaml"
	DefaultEnvExampleFile  = ".local-agent/local.env.example"
	DefaultEnvFile         = ".env"
)

const (
	DefaultBusyMessage         = "El bot está ocupado procesando otras solicitudes. Intenta de nuevo en unos minutos."
	DefaultModelErrorMessage   = "No pude completar la respuesta por un error del modelo. Intenta de nuevo."
	DefaultUnauthorizedMessage = "No tienes permiso para usar este bot. Pide acceso a quien administra local-agent."
)

// Config is the complete non-sensitive configuration stored in config.yaml.
// Secrets are resolved separately through Model.APIKeyEnv and Slack's fixed
// environment variable names.
type Config struct {
	Agent   AgentConfig   `yaml:"agent"`
	State   StateConfig   `yaml:"state"`
	Context ContextConfig `yaml:"context"`
	Runtime RuntimeConfig `yaml:"runtime"`
	Model   ModelConfig   `yaml:"model"`
	Slack   SlackConfig   `yaml:"slack"`

	document *sourceDocument
}

type AgentConfig struct {
	Name string `yaml:"name"`
}

type StateConfig struct {
	Dir string `yaml:"dir"`
	DB  string `yaml:"db"`
}

type ContextConfig struct {
	MaxMessages                   int `yaml:"max_messages"`
	MaxChars                      int `yaml:"max_chars"`
	RetainMessagesPerConversation int `yaml:"retain_messages_per_conversation"`
}

type RuntimeConfig struct {
	LogLevel                string `yaml:"log_level"`
	ModelTimeoutSeconds     int    `yaml:"model_timeout_seconds"`
	SlackAPITimeoutSeconds  int    `yaml:"slack_api_timeout_seconds"`
	MaxConcurrentModelCalls int    `yaml:"max_concurrent_model_calls"`
	BusyMessage             string `yaml:"busy_message"`
	ModelErrorMessage       string `yaml:"model_error_message"`
}

type ModelConfig struct {
	Name            string            `yaml:"name"`
	BaseURL         string            `yaml:"base_url"`
	APIKeyEnv       string            `yaml:"api_key_env"`
	Headers         map[string]string `yaml:"headers,omitempty"`
	ReasoningEffort string            `yaml:"reasoning_effort"`
	ExtraBody       map[string]any    `yaml:"extra_body,omitempty"`
}

type SlackConfig struct {
	AppName             string   `yaml:"app_name"`
	BotDisplayName      string   `yaml:"bot_display_name"`
	UnauthorizedMessage string   `yaml:"unauthorized_message"`
	AllowAllUsers       bool     `yaml:"allow_all_users"`
	AllowedUserIDs      []string `yaml:"allowed_user_ids"`
	AllowedTeamIDs      []string `yaml:"allowed_team_ids"`
	AllowedChannelIDs   []string `yaml:"allowed_channel_ids"`
}

// Default returns a new Config populated with the PRD defaults.
func Default() Config {
	return Config{
		Agent: AgentConfig{
			Name: "Dev Agent",
		},
		State: StateConfig{
			Dir: DefaultProjectStateDir,
			DB:  DefaultDatabaseFile,
		},
		Context: ContextConfig{
			MaxMessages:                   30,
			MaxChars:                      20_000,
			RetainMessagesPerConversation: 100,
		},
		Runtime: RuntimeConfig{
			LogLevel:                "info",
			ModelTimeoutSeconds:     0,
			SlackAPITimeoutSeconds:  30,
			MaxConcurrentModelCalls: 4,
			BusyMessage:             DefaultBusyMessage,
			ModelErrorMessage:       DefaultModelErrorMessage,
		},
		Model: ModelConfig{
			Name:            "deepseek-v4-flash",
			BaseURL:         "https://api.deepseek.com",
			APIKeyEnv:       "DEEPSEEK_API_KEY",
			ReasoningEffort: "high",
			ExtraBody: map[string]any{
				"thinking": map[string]any{
					"type": "enabled",
				},
			},
		},
		Slack: SlackConfig{
			AppName:             "Local Agent",
			BotDisplayName:      "Dev Agent",
			UnauthorizedMessage: DefaultUnauthorizedMessage,
			AllowAllUsers:       false,
			AllowedUserIDs:      []string{},
			AllowedTeamIDs:      []string{},
			AllowedChannelIDs:   []string{},
		},
	}
}

// Defaults is an alias kept for call sites that read more naturally in plural.
func Defaults() Config {
	return Default()
}
