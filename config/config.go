package config

import (
	"encoding/json"
	"os"
)

type LLMConfig struct {
	Provider    string  `json:"provider"`
	Model       string  `json:"model"`
	APIKey      string  `json:"api_key"`
	BaseURL     string  `json:"base_url,omitempty"`
	MaxTokens   int     `json:"max_tokens"`
	Temperature float64 `json:"temperature"`
}

type RepoConfig struct {
	Name        string            `json:"name"`
	Path        string            `json:"path"`
	Remote      string            `json:"remote,omitempty"`       // HTTPS+PAT for cloud mode
	RemoteLocal string            `json:"remote_local,omitempty"` // SSH for local mode
	Branch      string            `json:"branch"`                 // default branch (fallback)
	Branches    map[string]string `json:"branches,omitempty"`     // per-environment: {"development":"develop","production":"release-*"}
}

type ServiceConfig struct {
	Name       string `json:"name"`
	BaseURL    string `json:"base_url"`
	HealthPath string `json:"health_path"`
	Token      string `json:"token"`
	McpURL     string `json:"mcp_url,omitempty"`
	McpCmd     string `json:"mcp_cmd,omitempty"`
}

type CronJobConfig struct {
	Name     string `json:"name"`
	Schedule string `json:"schedule"`
	Task     string `json:"task"`
}

type ChannelConfig struct {
	Type              string   `json:"type"`
	Enabled           bool     `json:"enabled"`
	AppID             string   `json:"app_id,omitempty"`
	AppSecret         string   `json:"app_secret,omitempty"`
	EncryptKey        string   `json:"encrypt_key,omitempty"`
	VerificationToken string   `json:"verification_token,omitempty"`
	AllowFrom         []string `json:"allow_from,omitempty"`
}

type HeartbeatConfig struct {
	Enabled         bool `json:"enabled"`
	IntervalMinutes int  `json:"interval_minutes"`
}

type DashboardConfig struct {
	Enabled  bool   `json:"enabled"`
	Port     int    `json:"port,omitempty"`      // default 8080
	Password string `json:"password,omitempty"`  // basic auth password (user: admin)
}

type Config struct {
	Mode          string          `json:"mode"`               // "local" or "cloud" (default: "local")
	Environment   string          `json:"environment"`        // "development" or "production" (default: "development")
	Workspace     string          `json:"workspace"`
	LLM           LLMConfig       `json:"llm"`
	Repos         []RepoConfig    `json:"repos"`
	Services      []ServiceConfig `json:"services"`
	Channels      []ChannelConfig `json:"channels"`
	Heartbeat     HeartbeatConfig  `json:"heartbeat"`
	Dashboard     DashboardConfig   `json:"dashboard,omitempty"`
	Cron          []CronJobConfig `json:"cron,omitempty"`
	MaxIter        int             `json:"max_iterations"`
	MemoryWindow   int             `json:"memory_window"`
	MaxMemoryBytes int             `json:"max_memory_bytes,omitempty"` // max MEMORY.md size before compression (default 4096)
	BraveAPIKey   string          `json:"brave_api_key"`
	SendProgress  *bool           `json:"send_progress,omitempty"`   // send "working on..." progress messages (default true)
	SendToolHints *bool           `json:"send_tool_hints,omitempty"` // send detailed tool call descriptions (default false)
}

// IsLocal returns true when running on a developer's machine.
func (c *Config) IsLocal() bool {
	return c.Mode != "cloud"
}

// ProgressEnabled returns whether progress messages should be sent (default true).
func (c *Config) ProgressEnabled() bool {
	if c.SendProgress == nil {
		return true
	}
	return *c.SendProgress
}

// ToolHintsEnabled returns whether detailed tool call hints should be sent (default false).
func (c *Config) ToolHintsEnabled() bool {
	if c.SendToolHints == nil {
		return false
	}
	return *c.SendToolHints
}

// Env returns the active environment, defaulting to "development".
func (c *Config) Env() string {
	if c.Environment == "" {
		return "development"
	}
	return c.Environment
}

// RepoBranch returns the branch for a repo in the active environment.
// Priority: branches[environment] > branch (fallback).
// Supports glob patterns like "release-*" — the repo manager resolves these.
func (c *Config) RepoBranch(repo RepoConfig) string {
	if repo.Branches != nil {
		if b, ok := repo.Branches[c.Env()]; ok && b != "" {
			return b
		}
	}
	return repo.Branch
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return defaultConfig(), nil
	}
	cfg := defaultConfig()
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func defaultConfig() *Config {
	return &Config{
		Workspace: "./workspace",
		LLM: LLMConfig{
			Provider:    "openrouter",
			Model:       "openrouter/arcee-ai/trinity-large-preview:free",
			MaxTokens:   8192,
			Temperature: 0.3,
		},
		Heartbeat:    HeartbeatConfig{Enabled: false, IntervalMinutes: 30},
		MaxIter:        20,
		MemoryWindow:   50,
		MaxMemoryBytes: 4096,
	}
}
