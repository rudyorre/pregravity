package config

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Schedule defines a recurring accountability or briefing check-in.
type Schedule struct {
	Name    string `json:"name"`
	Cron    string `json:"cron"`
	Prompt  string `json:"prompt"`
	Enabled bool   `json:"enabled"`
}

// TelegramConfig configures the long-polling Telegram transport.
// The bot token itself is read from PREGRAVITY_TELEGRAM_BOT_TOKEN (never stored in JSON).
type TelegramConfig struct {
	Enabled        bool    `json:"enabled"`
	AllowedChatIDs []int64 `json:"allowed_chat_ids"`
	PollTimeoutSec int     `json:"poll_timeout_sec"`
}

// NotifyConfig configures optional push notification delivery (ntfy or webhook).
type NotifyConfig struct {
	NtfyURL    string `json:"ntfy_url,omitempty"`
	NtfyTopic  string `json:"ntfy_topic,omitempty"`
	WebhookURL string `json:"webhook_url,omitempty"`
}

// HTTPConfig configures an optional loopback-only local HTTP control API.
// If enabled, PREGRAVITY_HTTP_TOKEN must be set in the environment.
type HTTPConfig struct {
	Enabled bool   `json:"enabled"`
	Listen  string `json:"listen"`
}

// AntigravityConfig stores metadata used to build handoff instructions.
// Pregravity never invokes Antigravity OAuth or proxies subscription tokens.
type AntigravityConfig struct {
	DashboardURL string `json:"dashboard_url"`
	InstanceName string `json:"instance_name"`
	PickupSkill  string `json:"pickup_skill"`
}

// Config is the top-level Pregravity configuration.
type Config struct {
	DataDir          string            `json:"data_dir"`
	Timezone         string            `json:"timezone"`
	Model            string            `json:"model"`
	APIBaseURL       string            `json:"api_base_url"`
	MaxHistoryTurns  int               `json:"max_history_turns"`
	MaxToolRounds    int               `json:"max_tool_rounds"`
	RequestTimeout   int               `json:"request_timeout_sec"`
	Persona          string            `json:"persona"`
	Antigravity      AntigravityConfig `json:"antigravity"`
	Telegram         TelegramConfig    `json:"telegram"`
	Notify           NotifyConfig      `json:"notify"`
	HTTP             HTTPConfig        `json:"http"`
	Schedules        []Schedule        `json:"schedules"`
	GeminiAPIKey     string            `json:"-"`
	TelegramBotToken string            `json:"-"`
	NtfyToken        string            `json:"-"`
	HTTPToken        string            `json:"-"`
}

// DefaultConfigDir returns ~/.config/pregravity.
func DefaultConfigDir() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "pregravity")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".pregravity"
	}
	return filepath.Join(home, ".config", "pregravity")
}

// DefaultDataDir returns ~/.local/share/pregravity.
func DefaultDataDir() string {
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, "pregravity")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".pregravity-data"
	}
	return filepath.Join(home, ".local", "share", "pregravity")
}

// DefaultConfigPath returns ~/.config/pregravity/config.json.
func DefaultConfigPath() string {
	return filepath.Join(DefaultConfigDir(), "config.json")
}

// DefaultEnvPath returns ~/.config/pregravity/env.
func DefaultEnvPath() string {
	return filepath.Join(DefaultConfigDir(), "env")
}

// Default returns a safe, sensible baseline configuration.
func Default() *Config {
	return &Config{
		DataDir:         DefaultDataDir(),
		Timezone:        "Local",
		Model:           "gemini-3.8-flash",
		APIBaseURL:      "https://generativelanguage.googleapis.com/v1beta",
		MaxHistoryTurns: 24,
		MaxToolRounds:   6,
		RequestTimeout:  45,
		Persona:         "You are Pregravity, a sharp, concise personal executive assistant and accountability partner. Keep the user organized, direct, and honest about commitments. Never fabricate tool results. When a request requires code changes, shell access, or deep engineering work, package a clean handoff for Antigravity via create_handoff instead of pretending you can touch the repo.",
		Antigravity: AntigravityConfig{
			DashboardURL: "https://antigravity.google.com",
			InstanceName: "devbox",
			PickupSkill:  "/pickup",
		},
		Telegram: TelegramConfig{
			Enabled:        false,
			AllowedChatIDs: []int64{},
			PollTimeoutSec: 30,
		},
		HTTP: HTTPConfig{
			Enabled: false,
			Listen:  "127.0.0.1:8787",
		},
		Schedules: []Schedule{
			{
				Name:    "morning-brief",
				Cron:    "0 9 * * 1-5",
				Prompt:  "Run my morning preflight check: review open tasks and overdue commitments, note any completed Antigravity handoffs, and give me a crisp top-3 focus plan for today.",
				Enabled: true,
			},
			{
				Name:    "afternoon-checkin",
				Cron:    "0 14 * * 1-5",
				Prompt:  "Quick afternoon drift check: look at today's open tasks and journal notes, ask me directly about anything slipping, and keep it to 3-4 short bullets.",
				Enabled: true,
			},
			{
				Name:    "evening-wrap",
				Cron:    "0 18 * * 1-5",
				Prompt:  "Evening wrap-up: summarize what got done today, flag unresolved commitments to carry forward or drop, and ask if anything should be queued as an Antigravity handoff for tomorrow.",
				Enabled: true,
			},
		},
	}
}

// LoadEnvFile reads KEY=VALUE pairs from an env file and sets them only if not
// already present in the process environment.
func LoadEnvFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		v = strings.Trim(v, `"'`)
		if k == "" {
			continue
		}
		if _, exists := os.LookupEnv(k); !exists {
			_ = os.Setenv(k, v)
		}
	}
	return scanner.Err()
}

// Load reads config from path, overlays secrets from environment, and validates it.
func Load(path string) (*Config, error) {
	if path == "" {
		path = DefaultConfigPath()
	}

	// Best-effort load sibling env file next to config.json.
	_ = LoadEnvFile(filepath.Join(filepath.Dir(path), "env"))

	cfg := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read config %s: %w", path, err)
		}
	} else {
		if err := json.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("parse config %s: %w", path, err)
		}
	}

	cfg.DataDir = expandHome(cfg.DataDir)
	if cfg.DataDir == "" {
		cfg.DataDir = DefaultDataDir()
	}
	if cfg.Model == "" {
		cfg.Model = "gemini-3.8-flash"
	}
	if cfg.APIBaseURL == "" {
		cfg.APIBaseURL = "https://generativelanguage.googleapis.com/v1beta"
	}
	if cfg.MaxHistoryTurns <= 0 {
		cfg.MaxHistoryTurns = 24
	}
	if cfg.MaxToolRounds <= 0 {
		cfg.MaxToolRounds = 6
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 45
	}
	if cfg.Telegram.PollTimeoutSec <= 0 {
		cfg.Telegram.PollTimeoutSec = 30
	}
	if cfg.Antigravity.DashboardURL == "" {
		cfg.Antigravity.DashboardURL = "https://antigravity.google.com"
	}
	if cfg.Antigravity.PickupSkill == "" {
		cfg.Antigravity.PickupSkill = "/pickup"
	}

	// Load secrets exclusively from environment variables.
	cfg.GeminiAPIKey = firstEnv("PREGRAVITY_GEMINI_API_KEY", "GEMINI_API_KEY")
	cfg.TelegramBotToken = os.Getenv("PREGRAVITY_TELEGRAM_BOT_TOKEN")
	cfg.NtfyToken = os.Getenv("PREGRAVITY_NTFY_TOKEN")
	cfg.HTTPToken = os.Getenv("PREGRAVITY_HTTP_TOKEN")

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Save writes cfg to path atomically with 0600 permissions.
func Save(path string, cfg *Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Location resolves cfg.Timezone to a *time.Location.
func (c *Config) Location() (*time.Location, error) {
	if c.Timezone == "" || strings.EqualFold(c.Timezone, "local") {
		return time.Local, nil
	}
	return time.LoadLocation(c.Timezone)
}

// Validate checks structural and security invariants.
func (c *Config) Validate() error {
	if _, err := c.Location(); err != nil {
		return fmt.Errorf("invalid timezone %q: %w", c.Timezone, err)
	}
	if c.Telegram.Enabled {
		if len(c.Telegram.AllowedChatIDs) == 0 {
			return errors.New("telegram.enabled is true, but telegram.allowed_chat_ids is empty (fail-closed for security)")
		}
	}
	if c.HTTP.Enabled {
		host := c.HTTP.Listen
		if !strings.HasPrefix(host, "127.0.0.1:") && !strings.HasPrefix(host, "localhost:") && !strings.HasPrefix(host, "[::1]:") {
			return fmt.Errorf("http.listen must bind to loopback (127.0.0.1 / localhost / [::1]), got %q", host)
		}
	}
	return nil
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

func expandHome(p string) string {
	if p == "" || !strings.HasPrefix(p, "~") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}
