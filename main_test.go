//go:build windows

package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/223n/restart-message/internal/config"
	"github.com/223n/restart-message/internal/detect"
	"github.com/223n/restart-message/internal/notify"
)

// present has a default arm, so a category added to detect renders as 「不明」
// and nothing fails — the omission is invisible at build time and only shows up
// as a wrong-looking notification. CLAUDE.md instructs adding new categories to
// present; this pins that instruction so it cannot be forgotten silently.
//
// The category list is taken from config.Default().NotifyOn rather than being
// repeated here: internal/config's own tests already assert that list is exactly
// the set detect classifies into, so a new category has to appear there first.
// That makes this test fail for the right reason instead of needing its own copy
// of the set to be remembered too.
func TestPresent_EveryCategoryHasItsOwnPresentation(t *testing.T) {
	defLabel, defColor, defEmoji := present(detect.Category("a category that will never exist"))

	seenLabel := map[string]detect.Category{}
	seenEmoji := map[string]detect.Category{}

	for _, name := range config.Default().NotifyOn {
		c := detect.Category(name)
		label, color, emoji := present(c)

		// CatUnknown is the one category the default arm is the right answer for.
		if c == detect.CatUnknown {
			if label != defLabel || color != defColor || emoji != defEmoji {
				t.Errorf("present(%q) = %q/%#06x/%q, want the default presentation %q/%#06x/%q",
					c, label, color, emoji, defLabel, defColor, defEmoji)
			}
			continue
		}

		if label == defLabel && emoji == defEmoji {
			t.Errorf("present(%q) falls through to the default presentation %q %q — add a case for it",
				c, defEmoji, defLabel)
			continue
		}
		if label == "" || emoji == "" {
			t.Errorf("present(%q) = label %q, emoji %q, want both non-empty", c, label, emoji)
		}
		// Distinct labels and emoji matter because the notification shows only
		// these: two categories sharing one is indistinguishable to the reader,
		// which is the likely outcome of adding a case by copy-paste.
		if prev, dup := seenLabel[label]; dup {
			t.Errorf("present(%q) and present(%q) share the label %q", prev, c, label)
		}
		seenLabel[label] = c
		if prev, dup := seenEmoji[emoji]; dup {
			t.Errorf("present(%q) and present(%q) share the emoji %q", prev, c, emoji)
		}
		seenEmoji[emoji] = c
	}

	if len(seenLabel) == 0 {
		t.Fatal("no categories were checked; config.Default().NotifyOn is empty")
	}
}

// runLogLine is the only record a boot-time run leaves: the task runs as SYSTEM
// in session 0, where its stdout and stderr go nowhere and task history is off by
// default on Windows 10/11. These pin the two properties an operator's triage
// depends on — that the line says which of the several silent outcomes happened,
// and that one run stays one line.
func TestRunLogLine(t *testing.T) {
	boot := time.Date(2026, 9, 8, 1, 2, 3, 0, time.UTC)
	res := &detect.Result{Category: detect.CatUpdate, BootTime: boot, RecordID: 42}

	tests := []struct {
		name      string
		cfgUsed   string
		res       *detect.Result
		outcome   string
		err       error
		wantIn    []string
		wantNotIn []string
	}{
		{
			name:    "sent names the config file, the category and the boot",
			cfgUsed: `C:\ProgramData\restart-message\config.json`,
			res:     res,
			outcome: runSent,
			wantIn: []string{
				"outcome=" + runSent,
				`config=C:\ProgramData\restart-message\config.json`,
				"category=update",
				"boot=2026-09-08T01:02:03Z",
			},
			wantNotIn: []string{"error="},
		},
		{
			// The empty path is the diagnosis of the documented misinstall
			// (config.json left in bin\ while the task runs the Program Files
			// copy), so it has to appear as something, not as nothing.
			name:    "no config file applied is recorded explicitly",
			cfgUsed: "",
			res:     res,
			outcome: runNoWebhook,
			err:     errors.New("discord_webhook_url が未設定です"),
			wantIn:  []string{"config=(none)", "outcome=" + runNoWebhook, "error=discord_webhook_url"},
		},
		{
			// Nothing was classified, so claiming a category would be a lie.
			name:      "detection failure carries no category",
			cfgUsed:   `C:\cfg.json`,
			res:       nil,
			outcome:   runDetectError,
			err:       errors.New("起動イベント(Event 12)が見つかりませんでした"),
			wantIn:    []string{"outcome=" + runDetectError, "error=起動イベント"},
			wantNotIn: []string{"category=", "boot="},
		},
		{
			name:    "the skip outcomes are distinguishable from a failure",
			cfgUsed: `C:\cfg.json`,
			res:     res,
			outcome: runAlreadyNotified,
			wantIn:  []string{"outcome=skipped-already-notified"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := runLogLine(tt.cfgUsed, tt.res, tt.outcome, tt.err)
			for _, want := range tt.wantIn {
				if !strings.Contains(got, want) {
					t.Errorf("runLogLine = %q, want it to contain %q", got, want)
				}
			}
			for _, unwanted := range tt.wantNotIn {
				if strings.Contains(got, unwanted) {
					t.Errorf("runLogLine = %q, want it NOT to contain %q", got, unwanted)
				}
			}
		})
	}

	// A Discord error body reaches the line verbatim (minus its token) and can be
	// multi-line; the log is appended to one line per event and read with grep.
	t.Run("a multi-line error stays one line", func(t *testing.T) {
		got := runLogLine("", res, runSendFailed, errors.New("discord webhook returned 404:\r\n{\n  \"message\": \"Unknown Webhook\"\n}"))
		if strings.ContainsAny(got, "\r\n") {
			t.Errorf("runLogLine = %q, want no newline or carriage return", got)
		}
		if !strings.Contains(got, "Unknown Webhook") {
			t.Errorf("runLogLine = %q, want the error text preserved", got)
		}
	})

	// notify.Send keeps up to 4096 bytes of response body inside HTTPError and
	// Error() renders all of it, so an outage page or a proxy's error HTML lands
	// here whole — once per boot, appended to a file with no rotation, for as long
	// as the webhook stays broken. That is the scenario this log exists for, which
	// is why the bound is a test and not a comment.
	t.Run("a huge error is bounded, keeping the informative head", func(t *testing.T) {
		body := "discord webhook returned 502: " + strings.Repeat("x", 5000)
		got := runLogLine(`C:\cfg.json`, res, runSendFailed, errors.New(body))

		if n := len([]rune(got)); n > maxLoggedErrorRunes+200 {
			t.Errorf("runLogLine produced %d runes for a %d-rune error; want it bounded near %d",
				n, len([]rune(body)), maxLoggedErrorRunes)
		}
		// Truncating is only acceptable because the head is what triage reads: the
		// status code has to survive, and the cut has to be visible.
		if !strings.Contains(got, "discord webhook returned 502") {
			t.Errorf("runLogLine = %q, want the status line kept", got)
		}
		if !strings.Contains(got, "…(truncated)") {
			t.Errorf("runLogLine = %q, want a marker showing the error was cut", got)
		}
		// The fields that identify the run must not be what got cut.
		for _, want := range []string{"outcome=" + runSendFailed, `config=C:\cfg.json`, "category=update"} {
			if !strings.Contains(got, want) {
				t.Errorf("runLogLine = %q, want it to still contain %q", got, want)
			}
		}
	})

	t.Run("an error just under the bound is untouched", func(t *testing.T) {
		body := strings.Repeat("あ", maxLoggedErrorRunes)
		got := runLogLine("", res, runSendFailed, errors.New(body))
		if !strings.Contains(got, body) {
			t.Errorf("runLogLine truncated a %d-rune error, which is at the %d-rune bound", len([]rune(body)), maxLoggedErrorRunes)
		}
		if strings.Contains(got, "…(truncated)") {
			t.Errorf("runLogLine = %q, want no truncation marker on an error that fits", got)
		}
	})
}

// checkInstallConfig is the gate that turns "registered a task that can never
// work" into a refusal while a human is still watching. The cases below are the
// ones the audit named: a working configuration, a missing one, and one that
// only looks like it works because of a variable set in the installing user's
// environment — which the SYSTEM identity the task runs as does not have.
func TestCheckInstallConfig(t *testing.T) {
	const pdCfg = `C:\ProgramData\restart-message\config.json`
	const applied = `C:\Program Files\restart-message\config.json`

	tests := []struct {
		name      string
		facts     installConfigFacts
		wantOK    bool
		wantIn    []string
		wantNotIn []string
	}{
		{
			name:   "a config file with a webhook URL satisfies the gate",
			facts:  installConfigFacts{Used: applied, WebhookSet: true, ProgramDataCfg: pdCfg},
			wantOK: true,
		},
		{
			name:   "no config file at all is refused, with both remedies",
			facts:  installConfigFacts{ProgramDataCfg: pdCfg},
			wantOK: false,
			wantIn: []string{"設定ファイル", pdCfg, "-config", "-allow-unconfigured"},
			// 対処4 is the machine-wide-variable remedy; with no variable set
			// there is nothing for it to be right about, and offering it would
			// invite -allow-unconfigured on an install that simply has no config.
			wantNotIn: []string{"対処4"},
		},
		{
			name:   "a config file without a webhook URL is refused and named",
			facts:  installConfigFacts{Used: applied, ProgramDataCfg: pdCfg},
			wantOK: false,
			wantIn: []string{applied, "discord_webhook_url"},
		},
		{
			// The trap: `test` passes in the installing shell because the variable
			// is set there, and the registered task sees neither the variable nor a
			// file. gatherInstallFacts loads with it removed, so it arrives here as
			// "no webhook, but the variable is set" — refused, and told why.
			//
			// 判別できない and 対処4 are asserted because the refusal must not claim
			// what os.Getenv cannot know: a machine-wide DISCORD_WEBHOOK_URL is
			// inherited by the SYSTEM task, so this operator may be looking at a
			// false negative and needs the flag that carries them past it. 対処3
			// alone tells them their working setup 「失敗し続けます」.
			// 「システム環境変数（マシン全体）」 is asserted rather than just 「対処4」
			// because the note points at 対処4 by name: matching the name alone
			// passes even when the remedy it points at was never appended, which
			// is the failure this case is here to catch.
			name:   "a user DISCORD_WEBHOOK_URL does not satisfy the gate",
			facts:  installConfigFacts{EnvWebhookSet: true, ProgramDataCfg: pdCfg},
			wantOK: false,
			wantIn: []string{"DISCORD_WEBHOOK_URL", "SYSTEM", pdCfg, "判別できない", "対処4", "システム環境変数（マシン全体）", "-allow-unconfigured"},
		},
		{
			name:   "a user RESTART_MESSAGE_CONFIG does not satisfy the gate",
			facts:  installConfigFacts{EnvConfigSet: true, ProgramDataCfg: pdCfg},
			wantOK: false,
			wantIn: []string{"RESTART_MESSAGE_CONFIG", "SYSTEM", "判別できない"},
		},
		{
			// Kept even when the check passes: the operator set a variable this
			// check did not honour, so they must not be left believing it is what
			// makes the install work. What it must NOT do on a pass is repeat the
			// failure wording — nothing failed, there is no 対処 list under it, and
			// -allow-unconfigured is not a thing to do here.
			name:      "a set variable is reported even when the gate passes",
			facts:     installConfigFacts{Used: applied, WebhookSet: true, EnvWebhookSet: true, ProgramDataCfg: pdCfg},
			wantOK:    true,
			wantIn:    []string{"DISCORD_WEBHOOK_URL", applied},
			wantNotIn: []string{"-allow-unconfigured", "対処", "毎回の起動で失敗"},
		},
		{
			// The symmetric case, and the one whose absence let the EnvConfigSet
			// note keep pointing at a 対処 list that is only printed on a refusal.
			name:      "a set RESTART_MESSAGE_CONFIG is reported even when the gate passes",
			facts:     installConfigFacts{Used: applied, WebhookSet: true, EnvConfigSet: true, ProgramDataCfg: pdCfg},
			wantOK:    true,
			wantIn:    []string{"RESTART_MESSAGE_CONFIG", applied},
			wantNotIn: []string{"対処", "-config"},
		},
		{
			name:   "an unreadable config refuses install and quotes the error",
			facts:  installConfigFacts{LoadErr: errors.New("は通常ファイルではないため"), ProgramDataCfg: pdCfg},
			wantOK: false,
			wantIn: []string{"は通常ファイルではないため"},
		},
		{
			// notify_shutdown_start false is a choice, not a broken install, so it
			// is a note rather than a refusal — but the service would then be
			// installed and silent, which is worth saying out loud.
			name:   "install-service warns when notify_shutdown_start is false",
			facts:  installConfigFacts{Used: applied, WebhookSet: true, ForService: true, ProgramDataCfg: pdCfg},
			wantOK: true,
			wantIn: []string{"notify_shutdown_start"},
		},
		{
			name:      "install-service with the notice enabled says nothing extra",
			facts:     installConfigFacts{Used: applied, WebhookSet: true, ForService: true, ShutdownStart: true, ProgramDataCfg: pdCfg},
			wantOK:    true,
			wantNotIn: []string{"notify_shutdown_start"},
		},
		{
			// The config never loaded, so its NotifyShutdownStart is the zero value
			// and not a statement about the operator's configuration.
			name:      "a load failure does not produce a notify_shutdown_start claim",
			facts:     installConfigFacts{LoadErr: errors.New("boom"), ForService: true, ProgramDataCfg: pdCfg},
			wantOK:    false,
			wantNotIn: []string{"notify_shutdown_start"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := checkInstallConfig(tt.facts)
			if v.OK != tt.wantOK {
				t.Errorf("checkInstallConfig(...).OK = %v, want %v (reason %q)", v.OK, tt.wantOK, v.Reason)
			}
			if tt.wantOK && v.Reason != "" {
				t.Errorf("checkInstallConfig(...) passed but still gave a reason %q", v.Reason)
			}
			if !tt.wantOK && v.Reason == "" {
				t.Error("checkInstallConfig(...) refused without saying why")
			}
			text := v.Reason + "\n" + strings.Join(v.Notes, "\n")
			for _, want := range tt.wantIn {
				if !strings.Contains(text, want) {
					t.Errorf("checkInstallConfig(...) output = %q, want it to contain %q", text, want)
				}
			}
			for _, unwanted := range tt.wantNotIn {
				if strings.Contains(text, unwanted) {
					t.Errorf("checkInstallConfig(...) output = %q, want it NOT to contain %q", text, unwanted)
				}
			}
		})
	}
}

// shutdownNoticeGate is what makes `shutdown-notice` a check of the service
// rather than of itself: before this, the manual command passed force=true, so
// with notify_shutdown_start false it sent a notice that the service it claims
// to verify would never send.
func TestShutdownNoticeGate(t *testing.T) {
	tests := []struct {
		name                string
		notifyShutdownStart bool
		force               bool
		wantSend            bool
	}{
		{name: "enabled sends", notifyShutdownStart: true, wantSend: true},
		{name: "enabled with -force sends", notifyShutdownStart: true, force: true, wantSend: true},
		{name: "disabled skips, as the service would", notifyShutdownStart: false, wantSend: false},
		{name: "disabled with -force sends anyway", notifyShutdownStart: false, force: true, wantSend: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			send, reason := shutdownNoticeGate(tt.notifyShutdownStart, tt.force)
			if send != tt.wantSend {
				t.Errorf("shutdownNoticeGate(%v, %v) send = %v, want %v", tt.notifyShutdownStart, tt.force, send, tt.wantSend)
			}
			switch {
			case send && reason != "":
				t.Errorf("shutdownNoticeGate(%v, %v) sends but gave a skip reason %q", tt.notifyShutdownStart, tt.force, reason)
			case !send && !strings.Contains(reason, "notify_shutdown_start"):
				// The console line is the only place an operator learns which
				// setting silenced the check, so it has to name it.
				t.Errorf("shutdownNoticeGate(%v, %v) skip reason = %q, want it to name notify_shutdown_start", tt.notifyShutdownStart, tt.force, reason)
			}
		})
	}
}

// clearEnv is the half of W2 that decides whether the trap is caught at all.
// checkInstallConfig refuses the user-variable case only because the facts reach
// it with WebhookSet false, and that is true only while this function actually
// removes DISCORD_WEBHOOK_URL for the duration of the load. If it stopped doing
// so, config.Load would apply the variable, the gate would pass exactly the
// configuration it exists to refuse, and every subtest of TestCheckInstallConfig
// would still be green.
func TestClearEnv(t *testing.T) {
	const (
		wasSet   = "RESTART_MESSAGE_TEST_CLEARENV_SET"
		wasEmpty = "RESTART_MESSAGE_TEST_CLEARENV_EMPTY"
		wasUnset = "RESTART_MESSAGE_TEST_CLEARENV_UNSET"
	)
	names := []string{wasSet, wasEmpty, wasUnset}
	// t.Setenv first on all three so the real environment is restored whatever
	// this test does to them; the lines below then put each into the state under
	// test. wasEmpty is not padding: clearEnv uses LookupEnv precisely to keep
	// "set to empty" distinct from "unset", and only a set-to-empty variable can
	// exercise that.
	for _, n := range names {
		t.Setenv(n, "sentinel")
	}
	const setValue = `C:\ProgramData\restart-message\config.json`
	os.Setenv(wasSet, setValue)
	os.Setenv(wasEmpty, "")
	os.Unsetenv(wasUnset)

	type envState struct {
		value string
		set   bool
	}
	snapshot := func() map[string]envState {
		m := map[string]envState{}
		for _, n := range names {
			v, ok := os.LookupEnv(n)
			m[n] = envState{value: v, set: ok}
		}
		return m
	}
	// The restore is asserted against the state actually observed rather than
	// against the state this test asked for. It is the same assertion wherever
	// SetEnvironmentVariable's treatment of an empty value lands — a Windows
	// question this test cannot answer for itself — and it stays an assertion
	// about clearEnv instead of becoming one about the platform.
	before := snapshot()
	if s := before[wasSet]; !s.set || s.value != setValue {
		t.Fatalf("setup: %s = %q (set=%v), want %q", wasSet, s.value, s.set, setValue)
	}
	if before[wasUnset].set {
		t.Fatalf("setup: %s is set, want it absent", wasUnset)
	}

	restore := clearEnv(names...)

	t.Run("every named variable is absent inside the window", func(t *testing.T) {
		for _, n := range names {
			if v, ok := os.LookupEnv(n); ok {
				t.Errorf("%s = %q and still set; gatherInstallFacts would then measure the installing shell, not SYSTEM", n, v)
			}
		}
	})

	restore()

	t.Run("the prior state comes back exactly, unset staying unset", func(t *testing.T) {
		after := snapshot()
		for _, n := range names {
			if after[n] != before[n] {
				t.Errorf("after restore %s = %q (set=%v), want %q (set=%v) — config.Load's other callers in this process see the difference",
					n, after[n].value, after[n].set, before[n].value, before[n].set)
			}
		}
		// Spelled out separately because it is the one direction a map compare
		// would also satisfy by leaving everything cleared.
		if v, ok := os.LookupEnv(wasSet); !ok || v != setValue {
			t.Errorf("after restore %s = %q (set=%v), want %q", wasSet, v, ok, setValue)
		}
	})
}

// The round trip TestCheckInstallConfig cannot see: that the facts handed to the
// gate are the ones a SYSTEM process would find, not the ones this shell has.
// The -config path is explicit so the result does not depend on whichever
// config.json the runner happens to have beside the test binary or in
// %ProgramData%.
func TestGatherInstallFacts_TheEnvironmentDoesNotSatisfyTheWebhook(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	// A real config file, and deliberately no discord_webhook_url: the only thing
	// that could supply one is the variable set below.
	if err := os.WriteFile(cfgPath, []byte(`{"username":"test","notify_shutdown_start":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	const envURL = "https://discord.com/api/webhooks/1/not-a-real-token"
	t.Setenv(envWebhookVar, envURL)

	f := gatherInstallFacts(cfgPath, false)

	if f.LoadErr != nil {
		t.Fatalf("gatherInstallFacts LoadErr = %v, want nil", f.LoadErr)
	}
	if f.Used != cfgPath {
		t.Errorf("gatherInstallFacts Used = %q, want the explicit -config %q", f.Used, cfgPath)
	}
	if !f.EnvWebhookSet {
		t.Errorf("gatherInstallFacts EnvWebhookSet = false, want true — the note that explains the refusal depends on it")
	}
	if f.WebhookSet {
		t.Error("gatherInstallFacts WebhookSet = true from the environment alone: the gate would now pass an install whose SYSTEM task resolves no webhook URL at all")
	}
	if v := os.Getenv(envWebhookVar); v != envURL {
		t.Errorf("after gatherInstallFacts %s = %q, want it restored to %q", envWebhookVar, v, envURL)
	}
	if v := checkInstallConfig(f); v.OK {
		t.Error("checkInstallConfig accepted facts with no webhook URL; the refusal is the whole point of the gate")
	}
}

// Both schtasks call sites used to blame privileges for every failure, including
// the one schtasksPath's literal fallback produces on a Windows installed off C:
// — where the file is simply not there and elevating changes nothing.
func TestSchtasksFailure(t *testing.T) {
	const resolved = `D:\Windows\System32\schtasks.exe`
	notFound := &fs.PathError{Op: "fork/exec", Path: resolved, Err: fs.ErrNotExist}
	exitStatus := errors.New("exit status 1")

	tests := []struct {
		name        string
		op          string
		viaFallback bool
		err         error
		wantIn      []string
		wantNotIn   []string
	}{
		{
			name:      "an exit status still points at elevation, and names what ran",
			op:        "/Create",
			err:       exitStatus,
			wantIn:    []string{"/Create", resolved, "管理者権限"},
			wantNotIn: []string{"見つかりません"},
		},
		{
			name:      "a missing schtasks is not an elevation problem",
			op:        "/Delete",
			err:       notFound,
			wantIn:    []string{"見つかりません", resolved},
			wantNotIn: []string{"管理者権限"},
		},
		{
			// The case the fallback path exists to make visible: neither
			// GetSystemDirectory nor GetWindowsDirectory answered, so the path in
			// the message is a guess and the operator has to be told that.
			name:        "a missing schtasks at the guessed path says the path was guessed",
			op:          "/Create",
			viaFallback: true,
			err:         notFound,
			wantIn:      []string{"見つかりません", resolved, "システムディレクトリ"},
			wantNotIn:   []string{"管理者権限"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := schtasksFailure(tt.op, resolved, tt.viaFallback, tt.err)
			if got == nil {
				t.Fatal("schtasksFailure returned nil for a failure")
			}
			if !errors.Is(got, tt.err) {
				t.Errorf("schtasksFailure = %v, want it to wrap %v", got, tt.err)
			}
			text := got.Error()
			for _, want := range tt.wantIn {
				if !strings.Contains(text, want) {
					t.Errorf("schtasksFailure = %q, want it to contain %q", text, want)
				}
			}
			for _, unwanted := range tt.wantNotIn {
				if strings.Contains(text, unwanted) {
					t.Errorf("schtasksFailure = %q, want it NOT to contain %q", text, unwanted)
				}
			}
		})
	}
}

// The boot-time run has a hard deadline: Task Scheduler kills it at
// executionTimeLimit with AllowHardTerminate, mid-send. Four numbers decide
// whether it fits, and they live in three files — two of them //go:build windows,
// which internal/notify cannot import. notify's own test restates the windows-side
// numbers by hand, so a hand-copy that drifts SMALLER than reality leaves that
// test green while a real boot overruns.
//
// This is the only test that reads all four from where they actually are. It runs
// on the windows-latest CI runner, which is the whole reason it can.
func TestBootRunFitsTheScheduledTaskLimit(t *testing.T) {
	send := notify.WorstCaseDuration(bootSendAttempts, bootSendWait)
	worst := collectWorstCase + send

	// The margin is not decoration. collect's six winevent queries carry no
	// timeout of their own, and process start, config load and the state write are
	// all outside the bound — so a worst case that merely squeaks under the limit
	// is not actually under it.
	const margin = 60 * time.Second
	if worst+margin > executionTimeLimit {
		t.Fatalf("worst case %v (collect %v + send %v) leaves less than %v under the %v limit: "+
			"lower notify.maxRetryAfter, or bootSendAttempts/bootSendWait, but do not raise the limit",
			worst, time.Duration(collectWorstCase), send, margin, executionTimeLimit)
	}
}

// isoMinutes feeds the task XML, so a wrong rendering silently registers a task
// with the wrong deadline — or with none the schema accepts.
func TestIsoMinutes(t *testing.T) {
	cases := map[time.Duration]string{
		10 * time.Minute: "PT10M",
		time.Minute:      "PT1M",
		90 * time.Second: "PT1M", // truncates; the limit has only ever been whole minutes
		2 * time.Hour:    "PT120M",
	}
	for d, want := range cases {
		if got := isoMinutes(d); got != want {
			t.Errorf("isoMinutes(%v) = %q, want %q", d, got, want)
		}
	}
	// The constant the template actually uses has to render as whole minutes with
	// nothing lost, or the comment above is a lie about the deadline in the XML.
	if executionTimeLimit%time.Minute != 0 {
		t.Fatalf("executionTimeLimit = %v, want a whole number of minutes: isoMinutes truncates", executionTimeLimit)
	}
}

// README tells the reader to copy config.example.json, so the likeliest way to
// reach a syntactically perfect webhook URL that 404s on every boot forever is to
// forget to edit one line of it. Every other check in the gate passes that file.
func TestIsPlaceholderWebhook(t *testing.T) {
	const real = "https://discord.com/api/webhooks/1234567890123456789/" +
		"abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ012345"
	cases := []struct {
		name string
		url  string
		want bool
	}{
		{"the sample shipped in config.example.json", "https://discord.com/api/webhooks/XXXXXXXXXXXX/YYYYYYYYYYYYYYYYYYYYYYYY", true},
		{"only the id was edited", "https://discord.com/api/webhooks/1234567890123456789/YYYYYYYYYYYYYYYYYYYYYYYY", true},
		{"only the token was edited", "https://discord.com/api/webhooks/XXXXXXXXXXXX/" + real[len(real)-68:], true},
		{"padded, as a hand-edited file often is", "  https://discord.com/api/webhooks/XXXXXXXXXXXX/YYYYYYYYYYYYYYYYYYYYYYYY  ", true},
		{"a fully configured URL", real, false},
		{"empty", "", false},
		// Deliberately narrow: this is not a "does it look like Discord" test. A
		// proxy or a compatible endpoint is a legitimate target and must pass.
		{"a proxy that is not discord.com", "https://hooks.example.internal/relay/abc123", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPlaceholderWebhook(tc.url); got != tc.want {
				t.Fatalf("isPlaceholderWebhook = %v, want %v", got, tc.want)
			}
		})
	}
}

// The sample really does contain what isPlaceholderWebhook looks for. Editing
// config.example.json without editing the detector would silently reopen the gap.
func TestConfigExampleIsRecognisedAsAPlaceholder(t *testing.T) {
	b, err := os.ReadFile("config.example.json")
	if err != nil {
		t.Fatalf("config.example.json: %v", err)
	}
	var cfg struct {
		WebhookURL string `json:"discord_webhook_url"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("config.example.json is not valid JSON: %v", err)
	}
	if !isPlaceholderWebhook(cfg.WebhookURL) {
		t.Fatalf("config.example.json's discord_webhook_url is not recognised as a placeholder; "+
			"the install gate would accept a copy of it verbatim (value length %d)", len(cfg.WebhookURL))
	}
}

// The gate must refuse the placeholder, and must say which file to edit.
func TestCheckInstallConfig_RefusesThePlaceholder(t *testing.T) {
	const pd = `C:\ProgramData\restart-message\config.json`
	v := checkInstallConfig(installConfigFacts{
		Used:               pd,
		WebhookSet:         true,
		PlaceholderWebhook: true,
		ShutdownStart:      true,
		ProgramDataCfg:     pd,
	})
	if v.OK {
		t.Fatal("the gate accepted config.example.json's placeholder URL")
	}
	if !strings.Contains(v.Reason, pd) {
		t.Fatalf("reason = %q, want it to name the file to edit", v.Reason)
	}
}
