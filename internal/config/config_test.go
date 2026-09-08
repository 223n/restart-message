package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/223n/restart-message/internal/detect"
)

// The package models Windows paths but carries no build tag, so every test here
// has to hold on Linux too: never spell a separator, build paths with
// filepath.Join, and reach the environment only through t.Setenv (which restores
// the previous value, including for the host's real ProgramData).

// isolate removes every ambient input Load consults so a test only sees what it
// sets up itself: both environment variables cleared, and ProgramData pointed at
// an empty directory instead of the host's real one. An empty value takes the
// same branch as a missing variable in this package, which is what lets t.Setenv
// stand in for unsetting.
func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("DISCORD_WEBHOOK_URL", "")
	t.Setenv("RESTART_MESSAGE_CONFIG", "")
	t.Setenv("ProgramData", t.TempDir())
	if p := exeConfigPath(t); fileExists(p) {
		// Only possible if something outside the test planted it; resolvePath
		// would silently pick it up and every expectation below would be wrong.
		t.Skipf("unexpected %s; refusing to overwrite it", p)
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// exeConfigPath is the executable-adjacent candidate resolvePath probes. Under
// `go test` the "executable" is the compiled test binary, so this points into
// the build cache rather than at an installed copy of the tool.
//
// A failure here is fatal rather than a skip: os.Executable is reliable on both
// platforms this project supports, and isolate calls this for every Load test,
// so skipping would take the whole suite down quietly (see
// TestNoExeAdjacentConfig).
func exeConfigPath(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return filepath.Join(filepath.Dir(exe), "config.json")
}

// writeExeConfig plants a config.json beside the running test binary, the only
// way to exercise that branch of resolvePath, and removes it again when the test
// ends. `go test` runs the binary out of a work directory it just created, so a
// refused write means a broken environment, not an untestable one — fail rather
// than skip, because a skip would leave the exe-adjacent branch unverified while
// the run still reports success.
func writeExeConfig(t *testing.T, body string) string {
	t.Helper()
	p := exeConfigPath(t)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("cannot write %s: %v", p, err)
	}
	t.Cleanup(func() { os.Remove(p) })
	return p
}

// TestNoExeAdjacentConfig guards the guard: a config.json left beside the test
// binary changes what resolvePath picks for nearly every case below, and
// isolate's answer to that is to skip. Skips keep the run green and are
// invisible without -v, so the whole Load suite could vanish while `go test`
// still prints ok. Fail once here instead, naming the file to delete.
func TestNoExeAdjacentConfig(t *testing.T) {
	if p := exeConfigPath(t); fileExists(p) {
		t.Fatalf("%s exists; remove it — it makes every Load test skip", p)
	}
}

// writeProgramDataConfig writes the %ProgramData%\restart-message\config.json
// candidate. isolate has already redirected ProgramData at a temp directory, so
// this touches nothing outside the test.
func writeProgramDataConfig(t *testing.T, body string) string {
	t.Helper()
	dir := ProgramDataDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll(%s): %v", dir, err)
	}
	return writeConfig(t, filepath.Join(dir, "config.json"), body)
}

// mkdirExeConfig and mkdirProgramDataConfig plant a *directory* where each
// search candidate expects a file — what an install script does when it runs one
// `mkdir -p` too deep. Neither is a config file, and both are removed again when
// the test ends (ProgramData is a t.TempDir, so only the exe-adjacent one needs
// explicit cleanup).
func mkdirExeConfig(t *testing.T) string {
	t.Helper()
	p := exeConfigPath(t)
	if err := os.Mkdir(p, 0o700); err != nil {
		t.Fatalf("cannot create %s: %v", p, err)
	}
	t.Cleanup(func() { os.Remove(p) })
	return p
}

func mkdirProgramDataConfig(t *testing.T) string {
	t.Helper()
	return tempDir(t, filepath.Join(ProgramDataDir(), "config.json"))
}

// tempConfigDir plants such a directory in a scratch directory of its own, for
// the cases where the operator names it directly.
func tempConfigDir(t *testing.T, name string) string {
	t.Helper()
	return tempDir(t, filepath.Join(t.TempDir(), name))
}

func tempDir(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatalf("MkdirAll(%s): %v", path, err)
	}
	return path
}

func writeConfig(t *testing.T, path, body string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
	return path
}

// tempConfig writes a config file into a directory of its own, so several
// candidates in one test cannot collide.
func tempConfig(t *testing.T, name, body string) string {
	t.Helper()
	return writeConfig(t, filepath.Join(t.TempDir(), name), body)
}

// allCategories is every category detect can produce. The defaults promise to
// notify on all of them, and present() in main.go maps each one to a label.
var allCategories = []detect.Category{
	detect.CatUpdate,
	detect.CatManual,
	detect.CatShutdown,
	detect.CatUnexpected,
	detect.CatCrash,
	detect.CatUnknown,
}

func TestDefault(t *testing.T) {
	d := Default()
	if d.Username != "Restart Notifier" {
		t.Errorf("Username = %q, want %q", d.Username, "Restart Notifier")
	}
	if !d.IncludeDowntime {
		t.Error("IncludeDowntime = false, want true")
	}
	if !d.NotifyShutdownStart {
		t.Error("NotifyShutdownStart = false, want true")
	}
	// Nothing is invented for the fields that carry deployment-specific values;
	// a non-empty default webhook URL would be a leaked secret.
	if d.WebhookURL != "" || d.AvatarURL != "" || d.Mention != "" {
		t.Errorf("WebhookURL/AvatarURL/Mention = %q/%q/%q, want all empty", d.WebhookURL, d.AvatarURL, d.Mention)
	}
}

// The default notify_on is documented as "every category", so it has to be
// exactly the set detect classifies into: a name missing here silently drops
// those notifications, and a name detect never produces matches nothing and
// hides the omission.
func TestDefault_NotifyOnMatchesDetectCategories(t *testing.T) {
	listed := make(map[string]bool, len(Default().NotifyOn))
	for _, name := range Default().NotifyOn {
		if listed[name] {
			t.Errorf("category %q listed twice in the defaults", name)
		}
		listed[name] = true
	}
	known := make(map[string]bool, len(allCategories))
	for _, c := range allCategories {
		known[string(c)] = true
		if !listed[string(c)] {
			t.Errorf("category %q is never notified on by default", c)
		}
	}
	for name := range listed {
		if !known[name] {
			t.Errorf("default notify_on lists %q, which detect never produces", name)
		}
	}
}

// Load falls back to Default().NotifyOn and hands that slice to the caller, so
// each call has to build a fresh one: a shared backing array would let one
// caller's edit rewrite every later default.
func TestDefault_IsIndependentPerCall(t *testing.T) {
	first := Default()
	first.NotifyOn[0] = "mutated"
	if got := Default().NotifyOn[0]; got != "update" {
		t.Fatalf("Default().NotifyOn[0] = %q after mutating an earlier copy, want %q", got, "update")
	}
}

func TestLoad_NoFileNoEnv(t *testing.T) {
	isolate(t)
	cfg, path, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if path != "" {
		t.Fatalf("path = %q, want empty when no config file exists anywhere", path)
	}
	if !reflect.DeepEqual(cfg, Default()) {
		t.Fatalf("cfg = %+v, want the defaults %+v", cfg, Default())
	}
}

// The file is overlaid on the defaults rather than replacing them, so keys it
// omits keep their default value.
func TestLoad_FileOverlaysDefaults(t *testing.T) {
	isolate(t)
	p := tempConfig(t, "config.json", `{
		"discord_webhook_url": "https://discord.example/api/webhooks/1/from-file",
		"avatar_url": "https://example.invalid/avatar.png",
		"mention": "<@&123>",
		"notify_on": ["update", "crash"],
		"include_downtime": false
	}`)
	cfg, gotPath, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if gotPath != p {
		t.Errorf("path = %q, want %q", gotPath, p)
	}
	if cfg.WebhookURL != "https://discord.example/api/webhooks/1/from-file" {
		t.Errorf("WebhookURL = %q, want the value from the file", cfg.WebhookURL)
	}
	if cfg.AvatarURL != "https://example.invalid/avatar.png" || cfg.Mention != "<@&123>" {
		t.Errorf("AvatarURL/Mention = %q/%q, want the values from the file", cfg.AvatarURL, cfg.Mention)
	}
	if !reflect.DeepEqual(cfg.NotifyOn, []string{"update", "crash"}) {
		// The defaults are decoded into first, so a leftover tail here would mean
		// notifications the operator asked to switch off still going out.
		t.Errorf("NotifyOn = %v, want exactly [update crash]", cfg.NotifyOn)
	}
	if cfg.IncludeDowntime {
		t.Error("IncludeDowntime = true, want the file's false to win over the default")
	}
	if cfg.Username != "Restart Notifier" {
		t.Errorf("Username = %q, want the default kept for a key the file omits", cfg.Username)
	}
	if !cfg.NotifyShutdownStart {
		t.Error("NotifyShutdownStart = false, want the default kept for a key the file omits")
	}
}

// Unparsable JSON is always an error, however the path was found: unlike an
// unreadable file it means the operator wrote something they expect to take
// effect, and running on the defaults instead would hide that.
func TestLoad_MalformedJSON(t *testing.T) {
	cases := []struct{ name, body string }{
		{"truncated object", `{"username": "x"`},
		{"not an object", `[1, 2, 3]`},
		{"wrong type for a field", `{"notify_on": "update"}`},
		{"empty file", ``},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("explicit path", func(t *testing.T) {
				isolate(t)
				p := tempConfig(t, "config.json", tc.body)
				cfg, gotPath, err := Load(p)
				if err == nil {
					t.Fatalf("Load = %+v, want an error", cfg)
				}
				if cfg != nil {
					t.Errorf("cfg = %+v, want nil alongside the error", cfg)
				}
				if gotPath != p {
					t.Errorf("path = %q, want %q so the caller can name the broken file", gotPath, p)
				}
			})
			t.Run("resolved path", func(t *testing.T) {
				isolate(t)
				p := tempConfig(t, "config.json", tc.body)
				t.Setenv("RESTART_MESSAGE_CONFIG", p)
				if cfg, _, err := Load(""); err == nil {
					t.Fatalf("Load = %+v, want an error", cfg)
				}
			})
		})
	}
}

// A config file the operator named must never be swallowed, and -config and
// RESTART_MESSAGE_CONFIG are documented (README, SECURITY.md) as equivalent ways
// to name it — so the same typo has to be as fatal through one channel as
// through the other. The environment variable is not the exotic channel either:
// the documented install registers the scheduled task with no -config, so the
// SYSTEM task's only way to point at a file is the variable.
//
// The failure modes exercised here are a missing file and a directory in its
// place, because permission bits are not portable between Linux and Windows (and
// a test running as root can read a 0000 file anyway).
func TestLoad_NamedFileThatCannotBeRead(t *testing.T) {
	failures := []struct {
		name  string
		plant func(t *testing.T) string
	}{
		{"the file is not there", func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent.json") }},
		{"a directory sits in the file's place", func(t *testing.T) string { return tempConfigDir(t, "config.json") }},
	}
	channels := []struct {
		name    string
		useFlag bool
	}{
		{"named with -config", true},
		{"named with RESTART_MESSAGE_CONFIG", false},
	}
	for _, f := range failures {
		for _, ch := range channels {
			t.Run(f.name+"/"+ch.name, func(t *testing.T) {
				isolate(t)
				named := f.plant(t)
				arg := ""
				if ch.useFlag {
					arg = named
				} else {
					t.Setenv("RESTART_MESSAGE_CONFIG", named)
				}
				cfg, gotPath, err := Load(arg)
				if err == nil {
					t.Fatalf("Load = %+v, want an error", cfg)
				}
				if cfg != nil {
					t.Errorf("cfg = %+v, want nil alongside the error", cfg)
				}
				// Nothing was read, so nothing may be named: status prints the returned
				// path as 「設定ファイル: ...」 next to its warning, and a file that could
				// not be opened must not be reported there as the one in use.
				if gotPath != "" {
					t.Errorf("path = %q, want empty — no file was read", gotPath)
				}
				// The path is not lost by that: os.ReadFile's *PathError spells it out,
				// which is what makes returning it separately unnecessary.
				if msg := err.Error(); !strings.Contains(msg, named) {
					t.Errorf("error = %q, want it to name %q", msg, named)
				}
			})
		}
	}
}

// The two candidates the tool probes on its own stay lenient about absence — on
// a fresh install neither exists, and that must not stop the notifier. Something
// that is there and is not a regular file is the other case: a directory named
// config.json is a botched install, not "no config here". Neither running the
// built-in defaults nor quietly demoting to the next candidate would tell the
// operator that the file they are looking at is being ignored.
func TestLoad_SearchCandidateMustBeARegularFile(t *testing.T) {
	t.Run("a directory in ProgramData", func(t *testing.T) {
		isolate(t)
		dir := mkdirProgramDataConfig(t)
		cfg, gotPath, err := Load("")
		if err == nil {
			t.Fatalf("Load = %+v, want an error", cfg)
		}
		if cfg != nil {
			t.Errorf("cfg = %+v, want nil alongside the error", cfg)
		}
		if gotPath != "" {
			t.Errorf("path = %q, want empty — nothing was read from it", gotPath)
		}
		if msg := err.Error(); !strings.Contains(msg, dir) {
			t.Errorf("error = %q, want it to name what is in the way, %q", msg, dir)
		}
	})
	t.Run("a directory beside the executable is not demoted to ProgramData", func(t *testing.T) {
		isolate(t)
		dir := mkdirExeConfig(t)
		// A perfectly good lower-priority file exists; falling through to it would
		// run a config the operator did not choose.
		writeProgramDataConfig(t, `{"mention": "programdata"}`)
		cfg, gotPath, err := Load("")
		if err == nil {
			t.Fatalf("Load = %+v, want an error", cfg)
		}
		if gotPath != "" {
			t.Errorf("path = %q, want empty — nothing was read from it", gotPath)
		}
		if msg := err.Error(); !strings.Contains(msg, dir) {
			t.Errorf("error = %q, want it to name the broken candidate %q", msg, dir)
		}
	})
}

// Absence is the only reason a probed candidate may be passed over in silence.
// A stat that fails for any other reason means a config file may well be there
// and we cannot tell: on the target platform that is an ACL on
// %ProgramData%\restart-message denying traverse to the SYSTEM identity the
// scheduled task runs as (ERROR_ACCESS_DENIED — fs.ErrPermission, not
// fs.ErrNotExist). Falling back to the built-in defaults there runs a different
// tool than the operator configured: no webhook URL, so `run` at boot notifies
// nobody and exits 0 doing it.
//
// Permission bits are not portable between Linux and Windows (and a test running
// as root reads a 0000 file anyway), so the trigger here is a regular file
// planted where the tool's directory belongs, which makes the candidate below it
// unstattable. What that stat reports differs by OS — ENOTDIR on Linux, whereas
// Windows answers this shape with ERROR_PATH_NOT_FOUND, which *is* fs.ErrNotExist
// — so the assertion follows what the host actually reported rather than assuming
// either one.
func TestLoad_UnreachableSearchCandidateIsNotAbsence(t *testing.T) {
	isolate(t)
	blocked := ProgramDataDir()
	if err := os.WriteFile(blocked, []byte("a file where the tool's directory belongs"), 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", blocked, err)
	}
	candidate := filepath.Join(blocked, "config.json")
	_, statErr := os.Stat(candidate)
	if statErr == nil {
		t.Fatalf("stat %s succeeded; the planted file did not block the path", candidate)
	}
	if errors.Is(statErr, fs.ErrNotExist) {
		// This host cannot tell the two apart for this shape, and absence is the
		// case where skipping the candidate is right.
		if _, _, err := Load(""); err != nil {
			t.Fatalf("Load: %v, want the candidate skipped where the OS reports absence (%v)", err, statErr)
		}
		return
	}
	cfg, gotPath, err := Load("")
	if err == nil {
		t.Fatalf("Load = %+v, want the unreadable candidate reported; stat said: %v", cfg, statErr)
	}
	if cfg != nil {
		t.Errorf("cfg = %+v, want nil alongside the error", cfg)
	}
	if gotPath != "" {
		t.Errorf("path = %q, want empty — nothing was read from it", gotPath)
	}
	if msg := err.Error(); !strings.Contains(msg, candidate) {
		t.Errorf("error = %q, want it to name the candidate %q", msg, candidate)
	}
}

// The webhook URL carries a bearer-equivalent token in its path, so the very
// first hop has to be encrypted: with http the token is on the wire in cleartext
// before any redirect could upgrade the connection. Loopback is the one
// exception, because that is how a stub server or a local experiment is
// addressed.
func TestLoad_WebhookURLScheme(t *testing.T) {
	const token = "s3cret-webhook-token"
	cases := []struct {
		name string
		url  string
		ok   bool
	}{
		{"https", "https://discord.example/api/webhooks/1/" + token, true},
		{"https spelled in upper case", "HTTPS://discord.example/api/webhooks/1/" + token, true},
		{"http to the loopback address", "http://127.0.0.1:8080/" + token, true},
		{"http elsewhere in 127.0.0.0/8", "http://127.0.0.2/" + token, true},
		{"http to localhost", "http://localhost/" + token, true},
		{"http to the IPv6 loopback address", "http://[::1]:8080/" + token, true},
		// Accepted AND normalised — see the trimmed-value assertion below. Storing
		// the padding would push the failure into notify, at notification time.
		{"padded with whitespace", "  https://discord.example/api/webhooks/1/" + token + "\n", true},
		{"http to a real host", "http://discord.example/api/webhooks/1/" + token, false},
		// The loopback exception is decided by parsing the host, so a name that
		// merely begins with one of the loopback spellings is still remote.
		{"a host that only starts with the loopback address", "http://127.0.0.1.evil.example/" + token, false},
		{"a host that only starts with localhost", "http://localhost.evil.example/" + token, false},
		{"no scheme at all", "discord.example/api/webhooks/1/" + token, false},
		{"scheme-relative", "//discord.example/api/webhooks/1/" + token, false},
		{"some other scheme", "ftp://discord.example/api/webhooks/1/" + token, false},
		// Rejected here rather than left to notify: a URL that will not parse cannot
		// be shown to be https, and notify would fail only at notification time —
		// the one moment nobody is watching a console.
		{"unparsable", "https://discord.example:notaport/" + token, false},
		// A URL with no host reaches that same unwatched moment, so it is caught
		// here for the same reason. These are the shapes a truncated paste and a
		// half-edited config.json produce.
		{"https with no host at all", "https://", false},
		{"https with the host edited out", "https:///api/webhooks/1/" + token, false},
		{"https with a port left behind but no host", "https://:443/" + token, false},
	}
	check := func(t *testing.T, url string, ok bool, cfg *Config, err error) {
		t.Helper()
		if ok {
			if err != nil {
				t.Fatalf("Load: %v, want the URL accepted", err)
			}
			// Accepting is not enough: the value handed back has to be one notify
			// can actually use. Validating a trimmed copy while storing the padded
			// original would move the failure to notification time, which is what
			// this check exists to prevent.
			if want := strings.TrimSpace(url); cfg.WebhookURL != want {
				t.Fatalf("stored WebhookURL is not normalised: %q, want %q", cfg.WebhookURL, want)
			}
			return
		}
		if err == nil {
			t.Fatalf("Load = %+v, want the URL rejected", cfg)
		}
		if cfg != nil {
			t.Errorf("cfg = %+v, want nil alongside the error", cfg)
		}
		// The URL is a secret; an operator pasting the error into an issue must not
		// paste the token with it.
		if msg := err.Error(); strings.Contains(msg, token) || strings.Contains(msg, url) {
			t.Errorf("error message leaks the webhook URL: %q", msg)
		}
	}
	for _, tc := range cases {
		// Both channels, because the check has to look at the effective URL: the
		// environment variable overrides the file, so validating only what the file
		// said would leave the override unguarded.
		t.Run(tc.name+"/from the config file", func(t *testing.T) {
			isolate(t)
			p := tempConfig(t, "config.json", `{"discord_webhook_url": `+strconv.Quote(tc.url)+`}`)
			cfg, _, err := Load(p)
			check(t, tc.url, tc.ok, cfg, err)
		})
		t.Run(tc.name+"/from DISCORD_WEBHOOK_URL", func(t *testing.T) {
			isolate(t)
			t.Setenv("DISCORD_WEBHOOK_URL", tc.url)
			cfg, _, err := Load("")
			check(t, tc.url, tc.ok, cfg, err)
		})
	}
}

// No webhook at all stays valid: status and a dry run legitimately run without
// one, and the subcommands that send report the missing URL themselves with a
// message naming both places it can come from. A variable or key that was
// declared and never filled in has the same meaning as an absent one.
func TestLoad_WebhookURLMayBeUnset(t *testing.T) {
	cases := []struct{ name, body string }{
		{"key absent", `{"username": "x"}`},
		{"empty string", `{"discord_webhook_url": ""}`},
		{"only whitespace", `{"discord_webhook_url": "   \t"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			if _, _, err := Load(tempConfig(t, "config.json", tc.body)); err != nil {
				t.Fatalf("Load: %v, want a config with no webhook URL to be valid", err)
			}
		})
	}
}

// DISCORD_WEBHOOK_URL exists so the secret can stay out of config.json, so it
// wins over the file — but only when it actually carries a value. A variable
// that is empty or holds nothing but padding (a common shape for "declared but
// never filled in") must leave the file's URL alone rather than blanking it.
func TestLoad_WebhookEnvOverride(t *testing.T) {
	const fromFile = "https://discord.example/api/webhooks/1/from-file"
	const fromEnv = "https://discord.example/api/webhooks/2/from-env"
	cases := []struct {
		name string
		env  string
		want string
	}{
		{"set", fromEnv, fromEnv},
		{"padded with whitespace", "  " + fromEnv + "\t\n", fromEnv},
		{"empty", "", fromFile},
		{"only whitespace", "   \t ", fromFile},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			p := tempConfig(t, "config.json", `{"discord_webhook_url": "`+fromFile+`"}`)
			t.Setenv("DISCORD_WEBHOOK_URL", tc.env)
			cfg, _, err := Load(p)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.WebhookURL != tc.want {
				t.Fatalf("WebhookURL = %q, want %q", cfg.WebhookURL, tc.want)
			}
		})
	}
}

// The environment variable also works on its own, with no config file at all —
// that is the whole deployment story for "just set DISCORD_WEBHOOK_URL".
func TestLoad_WebhookEnvWithoutFile(t *testing.T) {
	isolate(t)
	const url = "https://discord.example/api/webhooks/3/env-only"
	t.Setenv("DISCORD_WEBHOOK_URL", url)
	cfg, path, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if path != "" {
		t.Errorf("path = %q, want empty", path)
	}
	if cfg.WebhookURL != url {
		t.Errorf("WebhookURL = %q, want %q", cfg.WebhookURL, url)
	}
}

// An empty notify_on is "notify on everything" for ShouldNotify, but Load
// restores the defaults instead of leaving the list empty, so anything that
// reads cfg.NotifyOn (a config dump, a status message) sees the real set.
func TestLoad_EmptyNotifyOnFallsBackToDefaults(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{"empty array", `{"notify_on": []}`, Default().NotifyOn},
		{"json null", `{"notify_on": null}`, Default().NotifyOn},
		{"key absent", `{"username": "x"}`, Default().NotifyOn},
		{"populated list is kept as written", `{"notify_on": ["crash"]}`, []string{"crash"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			cfg, _, err := Load(tempConfig(t, "config.json", tc.body))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if !reflect.DeepEqual(cfg.NotifyOn, tc.want) {
				t.Fatalf("NotifyOn = %v, want %v", cfg.NotifyOn, tc.want)
			}
		})
	}
}

func TestShouldNotify(t *testing.T) {
	cases := []struct {
		name     string
		notifyOn []string
		category string
		want     bool
	}{
		{"nil list means every category", nil, "crash", true},
		{"empty list means every category", []string{}, "unknown", true},
		{"listed", []string{"update", "crash"}, "crash", true},
		{"not listed", []string{"update", "crash"}, "manual", false},
		// config.json is hand-written, so casing and stray padding around an entry
		// are the operator's typing, not a different category.
		{"upper-case entry", []string{"UPDATE"}, "update", true},
		{"mixed-case entry", []string{"Unexpected"}, "unexpected", true},
		{"entry padded with spaces and a tab", []string{"  manual\t"}, "manual", true},
		{"blank entry matches nothing", []string{""}, "manual", false},
		{"substring is not a match", []string{"update"}, "up", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{NotifyOn: tc.notifyOn}
			if got := c.ShouldNotify(tc.category); got != tc.want {
				t.Fatalf("ShouldNotify(%q) with %v = %v, want %v", tc.category, tc.notifyOn, got, tc.want)
			}
		})
	}
}

// The pairing that matters in practice: nothing detect classifies may be
// dropped by a default install.
func TestShouldNotify_DefaultsCoverEveryCategory(t *testing.T) {
	d := Default()
	for _, c := range allCategories {
		if !d.ShouldNotify(string(c)) {
			t.Errorf("default config drops category %q", c)
		}
	}
}

// resolvePath's order is: the -config flag, then RESTART_MESSAGE_CONFIG, then
// config.json beside the executable, then %ProgramData%\restart-message. The
// last two are candidates only if the file is really there, so each case drops
// one source and expects the next one down to win. Every file carries its origin
// in "mention", which is how the winner is identified.
func TestLoad_PathPrecedence(t *testing.T) {
	type sources struct{ explicit, env, exe, programData bool }
	cases := []struct {
		name string
		have sources
		want string // origin tag of the file that must win; "" = no file at all
	}{
		{"explicit path beats every other source", sources{true, true, true, true}, "explicit"},
		{"env var beats the files when no path was given", sources{false, true, true, true}, "env"},
		{"file beside the executable beats ProgramData", sources{false, false, true, true}, "exe"},
		{"ProgramData is the last resort", sources{false, false, false, true}, "programdata"},
		{"no candidate exists anywhere", sources{false, false, false, false}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			body := func(origin string) string { return `{"mention": "` + origin + `"}` }
			paths := map[string]string{}
			if tc.have.programData {
				paths["programdata"] = writeProgramDataConfig(t, body("programdata"))
			}
			if tc.have.exe {
				paths["exe"] = writeExeConfig(t, body("exe"))
			}
			if tc.have.env {
				paths["env"] = tempConfig(t, "config.json", body("env"))
				t.Setenv("RESTART_MESSAGE_CONFIG", paths["env"])
			}
			explicit := ""
			if tc.have.explicit {
				explicit = tempConfig(t, "explicit.json", body("explicit"))
				paths["explicit"] = explicit
			}

			cfg, gotPath, err := Load(explicit)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if tc.want == "" {
				if gotPath != "" {
					t.Fatalf("path = %q, want empty", gotPath)
				}
				if cfg.Mention != "" {
					t.Fatalf("Mention = %q, want empty (no file should have been read)", cfg.Mention)
				}
				return
			}
			if gotPath != paths[tc.want] {
				t.Errorf("path = %q, want the %s candidate %q", gotPath, tc.want, paths[tc.want])
			}
			if cfg.Mention != tc.want {
				t.Errorf("Mention = %q, want %q — the wrong file was read", cfg.Mention, tc.want)
			}
		})
	}
}

// The two file candidates are skipped when the file is not there, rather than
// being returned and failing to open later. The env var is deliberately not in
// this list: it is returned unchecked, so that naming a file that is not there
// fails instead of being skipped (see TestLoad_NamedFileThatCannotBeRead).
func TestLoad_FileCandidatesRequireExistence(t *testing.T) {
	t.Run("empty ProgramData directory yields no path", func(t *testing.T) {
		isolate(t)
		if err := os.MkdirAll(ProgramDataDir(), 0o700); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		_, path, err := Load("")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if path != "" {
			t.Fatalf("path = %q, want empty when the directory holds no config.json", path)
		}
	})
	t.Run("missing file beside the executable falls through to ProgramData", func(t *testing.T) {
		isolate(t)
		want := writeProgramDataConfig(t, `{"mention": "programdata"}`)
		// isolate has already asserted there is no config.json beside the test
		// binary, so this is the fall-through case.
		_, path, err := Load("")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if path != want {
			t.Fatalf("path = %q, want %q", path, want)
		}
	})
}

func TestProgramDataDir(t *testing.T) {
	t.Run("honours ProgramData", func(t *testing.T) {
		root := t.TempDir()
		t.Setenv("ProgramData", root)
		want := filepath.Join(root, "restart-message")
		if got := ProgramDataDir(); got != want {
			t.Fatalf("ProgramDataDir() = %q, want %q", got, want)
		}
	})
	t.Run("falls back to the Windows default when unset", func(t *testing.T) {
		// t.Setenv cannot unset a variable, but ProgramDataDir only inspects the
		// value, and an empty one takes the same branch as a missing variable.
		t.Setenv("ProgramData", "")
		// The fallback root is spelled out here so it cannot drift unnoticed; the
		// separator is left to filepath, since this test also runs on Linux.
		want := filepath.Join(`C:\ProgramData`, "restart-message")
		if got := ProgramDataDir(); got != want {
			t.Fatalf("ProgramDataDir() = %q, want %q", got, want)
		}
	})
}
