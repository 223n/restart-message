package config

import (
	"os"
	"path/filepath"
	"reflect"
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

// A file that cannot be read is fatal only when the operator named it: asking
// for a specific config and silently getting the defaults would be a trap,
// while a path merely inherited from the environment or a stale ProgramData
// directory must not stop the notifier from running.
//
// The unreadable file here is a missing one, because permission bits are not
// portable between Linux and Windows (and a test running as root can read a
// 0000 file anyway).
func TestLoad_UnreadableFile(t *testing.T) {
	cases := []struct {
		name     string
		explicit bool
		wantErr  bool
	}{
		{"explicitly requested path that is not there", true, true},
		{"resolved path that is not there", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			missing := filepath.Join(t.TempDir(), "absent.json")
			arg := ""
			if tc.explicit {
				arg = missing
			} else {
				t.Setenv("RESTART_MESSAGE_CONFIG", missing)
			}
			cfg, gotPath, err := Load(arg)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Load = %+v, want an error", cfg)
				}
				if cfg != nil {
					t.Errorf("cfg = %+v, want nil alongside the error", cfg)
				}
			} else if err != nil {
				t.Fatalf("Load: %v, want the missing file to be tolerated", err)
			} else if !reflect.DeepEqual(cfg, Default()) {
				t.Errorf("cfg = %+v, want the defaults %+v", cfg, Default())
			}
			// Either way the resolved path is reported, so a caller logging it can
			// show which file was looked for.
			if gotPath != missing {
				t.Errorf("path = %q, want %q", gotPath, missing)
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
// this list: it is returned unchecked (see TestLoad_UnreadableFile).
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
