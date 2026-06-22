// Package config loads runtime configuration for restart-message.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Config is the on-disk configuration (config.json).
type Config struct {
	WebhookURL      string   `json:"discord_webhook_url"`
	Username        string   `json:"username"`
	AvatarURL       string   `json:"avatar_url"`
	Mention         string   `json:"mention"`          // e.g. "<@&ROLE_ID>", "@here" — prepended as message content
	NotifyOn        []string `json:"notify_on"`        // categories to notify on; empty means all
	IncludeDowntime bool     `json:"include_downtime"` // include downtime field in the embed

	// NotifyShutdownStart enables the pre-shutdown notice sent by the service
	// (restart-message service) when Windows begins shutting down/restarting.
	NotifyShutdownStart bool `json:"notify_shutdown_start"`
}

// Default returns the built-in defaults (notify on every category).
func Default() *Config {
	return &Config{
		Username:            "Restart Notifier",
		NotifyOn:            []string{"update", "manual", "shutdown", "unexpected", "crash", "unknown"},
		IncludeDowntime:     true,
		NotifyShutdownStart: true,
	}
}

// Load reads configuration, starting from defaults and overlaying the resolved
// config file (if any) and the DISCORD_WEBHOOK_URL environment variable. It
// returns the effective config and the path that was used (empty if none).
func Load(explicit string) (*Config, string, error) {
	cfg := Default()
	path := resolvePath(explicit)
	if path != "" {
		b, err := os.ReadFile(path)
		switch {
		case err == nil:
			if err := json.Unmarshal(b, cfg); err != nil {
				return nil, path, err
			}
		case explicit != "":
			// An explicitly requested config file that cannot be read is an error.
			return nil, path, err
		}
	}
	if v := strings.TrimSpace(os.Getenv("DISCORD_WEBHOOK_URL")); v != "" {
		cfg.WebhookURL = v
	}
	if len(cfg.NotifyOn) == 0 {
		cfg.NotifyOn = Default().NotifyOn
	}
	return cfg, path, nil
}

// ShouldNotify reports whether the given category is enabled.
func (c *Config) ShouldNotify(category string) bool {
	if len(c.NotifyOn) == 0 {
		return true
	}
	for _, x := range c.NotifyOn {
		if strings.EqualFold(strings.TrimSpace(x), category) {
			return true
		}
	}
	return false
}

// resolvePath chooses the config path: explicit flag, then RESTART_MESSAGE_CONFIG,
// then config.json beside the executable, then %ProgramData%\restart-message.
func resolvePath(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if v := strings.TrimSpace(os.Getenv("RESTART_MESSAGE_CONFIG")); v != "" {
		return v
	}
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), "config.json")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	p := filepath.Join(ProgramDataDir(), "config.json")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

// ProgramDataDir is the machine-wide data directory for this tool
// (%ProgramData%\restart-message), used for config and state when running as a
// service/scheduled task without a user profile.
func ProgramDataDir() string {
	pd := os.Getenv("ProgramData")
	if pd == "" {
		pd = `C:\ProgramData`
	}
	return filepath.Join(pd, "restart-message")
}
