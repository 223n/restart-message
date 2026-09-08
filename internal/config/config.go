// Package config loads runtime configuration for restart-message.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
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
// config file (if any) and the DISCORD_WEBHOOK_URL environment variable.
//
// It returns the effective config and the path of the file whose contents were
// actually applied — empty when the defaults (plus the environment) stand on
// their own, so a caller reporting "設定ファイル: ..." never names a file it did
// not read. On error the path is empty for that same reason, with one exception:
// a file that was read and would not decode is named, because that file really is
// the one in use. Every other failure already spells its path out inside the
// error text.
func Load(explicit string) (*Config, string, error) {
	cfg := Default()
	path, named, err := resolvePath(explicit)
	if err != nil {
		return nil, "", err
	}
	used := ""
	if path != "" {
		b, err := os.ReadFile(path)
		switch {
		case err == nil:
			if err := json.Unmarshal(b, cfg); err != nil {
				return nil, path, err
			}
			used = path
		case named || !errors.Is(err, fs.ErrNotExist):
			// Two failures that must never be swallowed. A file the operator named
			// (-config or RESTART_MESSAGE_CONFIG) is a statement of intent, so the same
			// typo has to be as loud through the variable as through the flag — the
			// documented install registers the scheduled task without -config, which
			// leaves the variable as the SYSTEM task's only channel. And on any path,
			// only plain absence justifies falling back: a permission or I/O error
			// means a config file is there and unusable, and running the built-in
			// defaults instead would silently be a different tool.
			//
			// No path is returned with either failure: nothing was read, and status
			// prints the returned path as 「設定ファイル: ...」 beside its warning. The
			// path is not lost — os.ReadFile's *PathError spells it out — so naming
			// it here would only let that line claim a file is in use.
			return nil, "", err
		}
		// What is left is a speculative candidate that disappeared between
		// resolvePath's stat and this read. The defaults stand and `used` stays
		// empty, because nothing from a file was applied.
	}
	if v := strings.TrimSpace(os.Getenv("DISCORD_WEBHOOK_URL")); v != "" {
		cfg.WebhookURL = v
	}
	if len(cfg.NotifyOn) == 0 {
		cfg.NotifyOn = Default().NotifyOn
	}
	// Normalised before it is checked, and stored normalised: validating a trimmed
	// copy while returning the padded original would let a config.json written with
	// stray whitespace pass here and fail inside notify instead — at notification
	// time, the one moment nobody is watching a console, which is the exact failure
	// this check exists to move to startup. The environment branch above already
	// trims; this covers the file.
	cfg.WebhookURL = strings.TrimSpace(cfg.WebhookURL)
	// Checked last, on the effective URL: the environment variable wins over the
	// file, so validating the file's value alone would leave the override open.
	if err := validateWebhookURL(cfg.WebhookURL); err != nil {
		return nil, used, err
	}
	return cfg, used, nil
}

// validateWebhookURL rejects a webhook URL that would carry its secret token in
// cleartext. The token sits in the URL path, so the first hop already has to be
// encrypted — a redirect to https cannot un-send the plaintext request that
// preceded it. Discord serves the API over https only, so http can be nothing
// but a typo or a downgrade; it stays allowed for loopback hosts, which is how a
// stub server or a local experiment is addressed.
//
// An empty value is valid here: status and a dry run legitimately run with no
// webhook, and the subcommands that send report the missing URL themselves.
//
// A URL that will not parse is rejected rather than left to notify: if it cannot
// be parsed its scheme cannot be shown to be https, and notify would fail only
// at notification time — the one moment nobody is watching a console — whereas
// this fails at startup, where `restart-message test` and `status` show it. A URL
// that parses but names no host is rejected on the same ground: it reaches that
// same unwatched moment, so the check would be inconsistent with its own reason
// for existing if it let one through.
//
// The messages never quote the URL, because it is the secret.
func validateWebhookURL(raw string) error {
	// Load normalises before calling, so this only matters to a direct caller.
	// Padding is typing, not part of the URL, and a whitespace-only value means
	// "not set".
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		// Not wrapped: a *url.Error carries the URL, token and all.
		return errors.New("Webhook URL を URL として解釈できません（DISCORD_WEBHOOK_URL または config.json の discord_webhook_url を確認してください）")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && isLoopbackHost(u.Hostname())) {
		return errors.New("Webhook URL は https である必要があります（http はループバック宛てのみ許可。DISCORD_WEBHOOK_URL または config.json の discord_webhook_url を確認してください）")
	}
	// Checked after the scheme so the message matches what is actually missing.
	// The http branch above already required a loopback host, so this only ever
	// catches https — "https://", "https:///api/webhooks/1/…", "https://:443/…":
	// a truncated paste or a half-edited config.json, which parses cleanly and
	// would otherwise fail at the same unwatched moment as an unparsable one.
	if u.Hostname() == "" {
		return errors.New("Webhook URL にホスト名が含まれていません（DISCORD_WEBHOOK_URL または config.json の discord_webhook_url を確認してください）")
	}
	return nil
}

// isLoopbackHost reports whether host addresses this machine. The host is parsed
// rather than prefix-matched, so a remote name that merely begins with a
// loopback spelling (127.0.0.1.example.com, localhost.example.com) is not
// mistaken for one. url.URL.Hostname strips the port and the brackets around an
// IPv6 literal, so "[::1]:8080" arrives here as "::1".
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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
//
// The bool reports whether the operator named the file. The first two channels
// are documented as equivalent ways of saying "use this file", so both are taken
// as given and left for Load to fail on; the last two are locations this tool
// probes on its own, where absence is the normal case.
func resolvePath(explicit string) (string, bool, error) {
	if explicit != "" {
		return explicit, true, nil
	}
	if v := strings.TrimSpace(os.Getenv("RESTART_MESSAGE_CONFIG")); v != "" {
		return v, true, nil
	}
	for _, p := range searchPaths() {
		ok, err := isConfigFile(p)
		if err != nil {
			return p, false, err
		}
		if ok {
			return p, false, nil
		}
	}
	return "", false, nil
}

// searchPaths lists the locations probed when the operator named none, highest
// priority first: config.json beside the executable, then %ProgramData%.
func searchPaths() []string {
	var paths []string
	if exe, err := os.Executable(); err == nil {
		paths = append(paths, filepath.Join(filepath.Dir(exe), "config.json"))
	}
	return append(paths, filepath.Join(ProgramDataDir(), "config.json"))
}

// isConfigFile reports whether p is something this tool can read as its config.
// Existence is not enough. For an entry that is there but is not a regular file
// both of the quiet answers are wrong: skipping it runs the built-in defaults
// (or quietly demotes to a lower-priority file) while the operator looks at the
// config.json they created, and reading it is worse still — os.ReadFile on a
// FIFO or a device would block the boot notification for as long as nothing
// writes to it. So it is reported as the error it is. A failed stat is an error
// here too, with one exception: plain absence, the one quiet answer a location
// this tool probes on its own is entitled to.
func isConfigFile(p string) (bool, error) {
	fi, err := os.Stat(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Absence is the normal case for a location this tool probes on its own —
			// on a fresh install neither candidate exists — so the next one gets its
			// turn.
			return false, nil
		}
		// Anything else means a config file may well be there and we cannot tell:
		// an ACL on %ProgramData%\restart-message that denies traverse to the
		// SYSTEM identity the scheduled task runs as answers ERROR_ACCESS_DENIED,
		// not "not found". Passing that over would run the built-in defaults —
		// no webhook URL, so the boot notification goes nowhere and exits 0 doing
		// it — while the operator looks at the config.json they installed.
		return false, err
	}
	if !fi.Mode().IsRegular() {
		return false, fmt.Errorf("%s は通常ファイルではないため、設定ファイルとして読み込めません", p)
	}
	return true, nil
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
