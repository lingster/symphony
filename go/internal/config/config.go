// Package config handles WORKFLOW.md parsing and configuration.
package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config represents the parsed WORKFLOW.md configuration.
type Config struct {
	Tracker   TrackerConfig   `yaml:"tracker"`
	Polling   PollingConfig   `yaml:"polling"`
	Workspace WorkspaceConfig `yaml:"workspace"`
	Hooks     HooksConfig     `yaml:"hooks"`
	Agent     AgentConfig     `yaml:"agent"`
	Codex     CodexConfig     `yaml:"codex"`
	Server    ServerConfig    `yaml:"server"`
}

// TrackerConfig holds issue tracker settings.
type TrackerConfig struct {
	Kind           string   `yaml:"kind"`
	Endpoint       string   `yaml:"endpoint"`
	APIKey         string   `yaml:"api_key"`
	ProjectSlug    string   `yaml:"project_slug"`
	ActiveStates   []string `yaml:"active_states"`
	TerminalStates []string `yaml:"terminal_states"`
}

// PollingConfig holds polling settings.
type PollingConfig struct {
	IntervalMS int `yaml:"interval_ms"`
}

// WorkspaceConfig holds workspace settings.
type WorkspaceConfig struct {
	Root string `yaml:"root"`
}

// HooksConfig holds workspace hook scripts.
type HooksConfig struct {
	AfterCreate  string `yaml:"after_create"`
	BeforeRun    string `yaml:"before_run"`
	AfterRun     string `yaml:"after_run"`
	BeforeRemove string `yaml:"before_remove"`
	TimeoutMS    int    `yaml:"timeout_ms"`
}

// AgentConfig holds agent execution settings.
type AgentConfig struct {
	MaxConcurrentAgents        int            `yaml:"max_concurrent_agents"`
	MaxTurns                   int            `yaml:"max_turns"`
	MaxRetryBackoffMS          int            `yaml:"max_retry_backoff_ms"`
	MaxConcurrentAgentsByState map[string]int `yaml:"max_concurrent_agents_by_state"`
	Default                    string         `yaml:"default"`
}

// CodexConfig holds Codex-specific settings.
type CodexConfig struct {
	Command           string `yaml:"command"`
	ApprovalPolicy    string `yaml:"approval_policy"`
	ThreadSandbox     string `yaml:"thread_sandbox"`
	TurnSandboxPolicy string `yaml:"turn_sandbox_policy"`
	TurnTimeoutMS     int    `yaml:"turn_timeout_ms"`
	ReadTimeoutMS     int    `yaml:"read_timeout_ms"`
	StallTimeoutMS    int    `yaml:"stall_timeout_ms"`
}

// ServerConfig holds optional HTTP server settings.
type ServerConfig struct {
	Port int `yaml:"port"`
}

// Workflow represents a parsed WORKFLOW.md file.
type Workflow struct {
	Config         Config
	PromptTemplate string
}

// LoadWorkflow loads and parses a WORKFLOW.md file.
func LoadWorkflow(path string) (*Workflow, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read workflow file: %w", err)
	}

	return ParseWorkflow(data)
}

// ParseWorkflow parses WORKFLOW.md content.
func ParseWorkflow(data []byte) (*Workflow, error) {
	workflow := &Workflow{
		Config: DefaultConfig(),
	}

	// Check for YAML front matter
	if bytes.HasPrefix(data, []byte("---")) {
		parts := bytes.SplitN(data[3:], []byte("---"), 2)
		if len(parts) == 2 {
			// Parse YAML front matter
			if err := yaml.Unmarshal(parts[0], &workflow.Config); err != nil {
				return nil, fmt.Errorf("failed to parse YAML front matter: %w", err)
			}
			workflow.PromptTemplate = strings.TrimSpace(string(parts[1]))
		} else {
			// Malformed front matter
			return nil, fmt.Errorf("malformed YAML front matter")
		}
	} else {
		// No front matter, entire file is prompt
		workflow.PromptTemplate = strings.TrimSpace(string(data))
	}

	// Apply defaults and resolve environment variables
	workflow.Config = applyDefaults(workflow.Config)
	workflow.Config = resolveEnvVars(workflow.Config)

	return workflow, nil
}

// DefaultConfig returns the default configuration.
func DefaultConfig() Config {
	return Config{
		Tracker: TrackerConfig{
			Kind:           "linear",
			Endpoint:       "https://api.linear.app/graphql",
			ActiveStates:   []string{"Todo", "In Progress"},
			TerminalStates: []string{"Closed", "Cancelled", "Canceled", "Duplicate", "Done"},
		},
		Polling: PollingConfig{
			IntervalMS: 30000,
		},
		Workspace: WorkspaceConfig{
			Root: filepath.Join(os.TempDir(), "symphony_workspaces"),
		},
		Hooks: HooksConfig{
			TimeoutMS: 60000,
		},
		Agent: AgentConfig{
			MaxConcurrentAgents:        10,
			MaxTurns:                   20,
			MaxRetryBackoffMS:          300000,
			MaxConcurrentAgentsByState: make(map[string]int),
			Default:                    "codex",
		},
		Codex: CodexConfig{
			Command:        "codex app-server",
			TurnTimeoutMS:  3600000,
			ReadTimeoutMS:  5000,
			StallTimeoutMS: 300000,
		},
	}
}

func applyDefaults(cfg Config) Config {
	defaults := DefaultConfig()

	if cfg.Tracker.Kind == "" {
		cfg.Tracker.Kind = defaults.Tracker.Kind
	}
	if cfg.Tracker.Endpoint == "" {
		cfg.Tracker.Endpoint = defaults.Tracker.Endpoint
	}
	if len(cfg.Tracker.ActiveStates) == 0 {
		cfg.Tracker.ActiveStates = defaults.Tracker.ActiveStates
	}
	if len(cfg.Tracker.TerminalStates) == 0 {
		cfg.Tracker.TerminalStates = defaults.Tracker.TerminalStates
	}
	if cfg.Polling.IntervalMS == 0 {
		cfg.Polling.IntervalMS = defaults.Polling.IntervalMS
	}
	if cfg.Workspace.Root == "" {
		cfg.Workspace.Root = defaults.Workspace.Root
	}
	if cfg.Hooks.TimeoutMS == 0 {
		cfg.Hooks.TimeoutMS = defaults.Hooks.TimeoutMS
	}
	if cfg.Agent.MaxConcurrentAgents == 0 {
		cfg.Agent.MaxConcurrentAgents = defaults.Agent.MaxConcurrentAgents
	}
	if cfg.Agent.MaxTurns == 0 {
		cfg.Agent.MaxTurns = defaults.Agent.MaxTurns
	}
	if cfg.Agent.MaxRetryBackoffMS == 0 {
		cfg.Agent.MaxRetryBackoffMS = defaults.Agent.MaxRetryBackoffMS
	}
	if cfg.Agent.Default == "" {
		cfg.Agent.Default = defaults.Agent.Default
	}
	if cfg.Codex.Command == "" {
		cfg.Codex.Command = defaults.Codex.Command
	}
	if cfg.Codex.TurnTimeoutMS == 0 {
		cfg.Codex.TurnTimeoutMS = defaults.Codex.TurnTimeoutMS
	}
	if cfg.Codex.ReadTimeoutMS == 0 {
		cfg.Codex.ReadTimeoutMS = defaults.Codex.ReadTimeoutMS
	}
	if cfg.Codex.StallTimeoutMS == 0 {
		cfg.Codex.StallTimeoutMS = defaults.Codex.StallTimeoutMS
	}

	return cfg
}

// envVarRegex matches $VAR_NAME or ${VAR_NAME} patterns
var envVarRegex = regexp.MustCompile(`\$\{?([A-Za-z_][A-Za-z0-9_]*)\}?`)

func resolveEnvVars(cfg Config) Config {
	cfg.Tracker.APIKey = resolveEnvVar(cfg.Tracker.APIKey)
	cfg.Workspace.Root = expandPath(cfg.Workspace.Root)
	return cfg
}

func resolveEnvVar(s string) string {
	if strings.HasPrefix(s, "$") {
		matches := envVarRegex.FindStringSubmatch(s)
		if len(matches) > 1 {
			return os.Getenv(matches[1])
		}
	}
	return s
}

func expandPath(path string) string {
	// Expand ~ to home directory
	if strings.HasPrefix(path, "~") {
		home, err := os.UserHomeDir()
		if err == nil {
			path = filepath.Join(home, path[1:])
		}
	}
	// Resolve environment variables
	path = os.ExpandEnv(path)
	return path
}

// Validate checks that the configuration is valid for dispatch.
func (c *Config) Validate() error {
	if c.Tracker.Kind == "" {
		return fmt.Errorf("tracker.kind is required")
	}
	if c.Tracker.Kind != "linear" {
		return fmt.Errorf("unsupported tracker kind: %s", c.Tracker.Kind)
	}
	if c.Tracker.APIKey == "" {
		return fmt.Errorf("tracker.api_key is required (set LINEAR_API_KEY)")
	}
	if c.Tracker.ProjectSlug == "" {
		return fmt.Errorf("tracker.project_slug is required")
	}
	if c.Codex.Command == "" {
		return fmt.Errorf("codex.command is required")
	}
	return nil
}
