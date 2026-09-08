package detect

import (
	"testing"
	"time"

	"github.com/223n/restart-message/internal/winevent"
)

var base = time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)

func bootRec(rid int64) winevent.Record {
	return winevent.Record{EventID: 12, Provider: providerKernelGeneral, Time: base, RecordID: rid, Computer: "PC"}
}

func rec1074(offset time.Duration, data map[string]string) winevent.Record {
	return winevent.Record{EventID: 1074, Provider: "User32", Time: base.Add(offset), Data: data}
}

func TestLatest_WindowsUpdate(t *testing.T) {
	records := []winevent.Record{
		bootRec(100),
		rec1074(-1*time.Minute, map[string]string{
			"param1": `C:\windows\servicing\TrustedInstaller.exe (PC)`,
			"param3": "オペレーティング システム: アップグレード (計画済)",
			"param4": "0x80020003",
			"param5": "再起動",
			"param7": `NT AUTHORITY\SYSTEM`,
		}),
		{EventID: 13, Provider: providerKernelGeneral, Time: base.Add(-90 * time.Second)},
	}
	res, ok := Latest(records)
	if !ok {
		t.Fatal("expected a boot result")
	}
	if res.Category != CatUpdate {
		t.Fatalf("category = %q, want update", res.Category)
	}
	if res.ReasonCode != 0x80020003 {
		t.Fatalf("reason code = 0x%X, want 0x80020003", res.ReasonCode)
	}
	if res.Downtime != 90*time.Second {
		t.Fatalf("downtime = %v, want 90s", res.Downtime)
	}
}

func TestLatest_UpdateByReasonCodeOnly(t *testing.T) {
	// Unknown process, but planned + Operating System major reason -> update.
	records := []winevent.Record{
		bootRec(101),
		rec1074(-1*time.Minute, map[string]string{
			"param1": `C:\windows\system32\svchost.exe (PC)`,
			"param4": "0x80020010", // planned | OS | service pack
			"param5": "再起動",
			"param7": `NT AUTHORITY\SYSTEM`,
		}),
	}
	res, _ := Latest(records)
	if res.Category != CatUpdate {
		t.Fatalf("category = %q, want update", res.Category)
	}
}

func TestLatest_Manual(t *testing.T) {
	records := []winevent.Record{
		bootRec(102),
		rec1074(-30*time.Second, map[string]string{
			"param1": `C:\WINDOWS\SystemApps\...\StartMenuExperienceHost.exe (PC)`,
			"param3": "その他 (計画外)",
			"param4": "0x0",
			"param5": "再起動",
			"param7": `PC\User`,
		}),
	}
	res, _ := Latest(records)
	if res.Category != CatManual {
		t.Fatalf("category = %q, want manual", res.Category)
	}
}

func TestLatest_Unexpected(t *testing.T) {
	records := []winevent.Record{
		{EventID: 6008, Provider: providerEventLog, Time: base.Add(30 * time.Second)},
		bootRec(103),
	}
	res, _ := Latest(records)
	if res.Category != CatUnexpected {
		t.Fatalf("category = %q, want unexpected", res.Category)
	}
}

func TestLatest_Crash(t *testing.T) {
	// A bug-check (BSOD) must win over the unexpected-shutdown marker.
	records := []winevent.Record{
		{EventID: 41, Provider: providerKernelPower, Time: base.Add(10 * time.Second), Data: map[string]string{"BugcheckCode": "0x50"}},
		{EventID: 6008, Provider: providerEventLog, Time: base.Add(30 * time.Second)},
		bootRec(104),
	}
	res, _ := Latest(records)
	if res.Category != CatCrash {
		t.Fatalf("category = %q, want crash", res.Category)
	}
}

func TestLatest_Event41NoBugcheckIsNotUnexpected(t *testing.T) {
	// Event 41 with BugcheckCode 0 (ordinary modern reboot) + a normal 1074.
	records := []winevent.Record{
		{EventID: 41, Provider: providerKernelPower, Time: base.Add(10 * time.Second), Data: map[string]string{"BugcheckCode": "0"}},
		bootRec(105),
		rec1074(-20*time.Second, map[string]string{
			"param1": `C:\...\StartMenuExperienceHost.exe (PC)`,
			"param4": "0x0",
			"param5": "再起動",
			"param7": `PC\User`,
		}),
	}
	res, _ := Latest(records)
	if res.Category != CatManual {
		t.Fatalf("category = %q, want manual (41 w/o bugcheck must not be crash/unexpected)", res.Category)
	}
}

func TestLatest_NoBoot(t *testing.T) {
	records := []winevent.Record{
		rec1074(-1*time.Minute, map[string]string{"param4": "0x0"}),
	}
	if _, ok := Latest(records); ok {
		t.Fatal("expected ok=false when no boot marker present")
	}
}

// A large cumulative update may log its 1074 well over 30 minutes before the
// boot completes; it must still classify as update (regression for the old
// 30-minute window that dropped it to "unknown").
func TestLatest_UpdateLongShutdownGap(t *testing.T) {
	records := []winevent.Record{
		bootRec(200),
		rec1074(-45*time.Minute, map[string]string{
			"param1": `C:\windows\servicing\TrustedInstaller.exe (PC)`,
			"param4": "0x80020003",
			"param5": "再起動",
			"param7": `NT AUTHORITY\SYSTEM`,
		}),
	}
	res, _ := Latest(records)
	if res.Category != CatUpdate {
		t.Fatalf("category = %q, want update (45-min gap)", res.Category)
	}
}

// A planned full shutdown initiated by SYSTEM must be "shutdown", not "update".
func TestLatest_SystemPlannedPowerOffIsShutdown(t *testing.T) {
	records := []winevent.Record{
		bootRec(201),
		rec1074(-1*time.Minute, map[string]string{
			"param1": `C:\WINDOWS\system32\shutdown.exe (PC)`,
			"param4": "0x80000000", // planned, major OTHER
			"param5": "シャットダウン",
			"param7": `NT AUTHORITY\SYSTEM`,
		}),
	}
	res, _ := Latest(records)
	if res.Category != CatShutdown {
		t.Fatalf("category = %q, want shutdown", res.Category)
	}
}

// Taken verbatim from a real Event 1074 on Japanese Windows 11 (only the
// computer and user names are anonymised). Japanese renders a power-off as
// 電源を切る, not シャットダウン, so this record classified as "manual" until
// isPowerOff learned the string. param4 is "0x0" here, which also confirms
// that Windows renders the reason code as 0x-prefixed hex.
func TestLatest_JapanesePowerOffIsShutdown(t *testing.T) {
	records := []winevent.Record{
		bootRec(203),
		rec1074(-1*time.Minute, map[string]string{
			"param1": `C:\Windows\SystemApps\Microsoft.Windows.StartMenuExperienceHost_cw5n1h2txyewy\StartMenuExperienceHost.exe (PC)`,
			"param3": "その他 (計画外)",
			"param4": "0x0",
			"param5": "電源を切る",
			"param7": `PC\User`,
		}),
	}
	res, _ := Latest(records)
	if res.Category != CatShutdown {
		t.Fatalf("category = %q, want shutdown", res.Category)
	}
	if res.ReasonCode != 0 {
		t.Fatalf("ReasonCode = 0x%X, want 0x0", res.ReasonCode)
	}
}

// A cold boot after a clean shutdown (no surviving 1074) should be "shutdown",
// not "unknown".
func TestLatest_CleanShutdownColdBoot(t *testing.T) {
	records := []winevent.Record{
		bootRec(202),
		{EventID: 13, Provider: providerKernelGeneral, Time: base.Add(-8 * time.Hour)},
	}
	res, _ := Latest(records)
	if res.Category != CatShutdown {
		t.Fatalf("category = %q, want shutdown (clean cold boot)", res.Category)
	}
	if res.Downtime != 8*time.Hour {
		t.Fatalf("downtime = %v, want 8h", res.Downtime)
	}
}

func TestParseHex(t *testing.T) {
	cases := map[string]uint32{
		"0x80020003":  0x80020003,
		"80020003":    0x80020003,
		"0x0":         0,
		"":            0,
		"0xZZZ":       0, // invalid -> 0
		"0x1FFFFFFFF": 0, // exceeds uint32 -> rejected -> 0 (no silent truncation)
	}
	for in, want := range cases {
		if got := parseHex(in); got != want {
			t.Errorf("parseHex(%q) = 0x%X, want 0x%X", in, got, want)
		}
	}
}

func TestPendingShutdown_Update(t *testing.T) {
	records := []winevent.Record{
		rec1074(-10*time.Second, map[string]string{
			"param1": `C:\windows\servicing\TrustedInstaller.exe (PC)`,
			"param4": "0x80020003",
			"param5": "再起動",
			"param7": `NT AUTHORITY\SYSTEM`,
		}),
	}
	res, ok := PendingShutdown(records, base, 5*time.Minute)
	if !ok {
		t.Fatal("expected a pending shutdown")
	}
	if res.Category != CatUpdate {
		t.Fatalf("category = %q, want update", res.Category)
	}
}

func TestPendingShutdown_StaleIgnored(t *testing.T) {
	records := []winevent.Record{
		rec1074(-1*time.Hour, map[string]string{"param4": "0x0", "param5": "再起動"}),
	}
	if _, ok := PendingShutdown(records, base, 5*time.Minute); ok {
		t.Fatal("a 1074 older than the recent window must be ignored")
	}
}

// A 1074 from a previous boot cycle (older than the previous boot marker) must
// not be paired with the current boot.
func TestLatest_Stray1074FromPrevCycleExcluded(t *testing.T) {
	records := []winevent.Record{
		bootRec(203), // current boot at base
		{EventID: 6005, Provider: providerEventLog, Time: base.Add(-1 * time.Minute)}, // previous boot marker
		rec1074(-3*time.Minute, map[string]string{ // 1074 from before the previous boot
			"param1": `C:\...\StartMenuExperienceHost.exe (PC)`,
			"param4": "0x0",
			"param5": "再起動",
			"param7": `PC\User`,
		}),
	}
	res, _ := Latest(records)
	if res.Category != CatUnknown {
		t.Fatalf("category = %q, want unknown (stray 1074 must be excluded)", res.Category)
	}
}

func rec41(offset time.Duration, bugcheck string) winevent.Record {
	return winevent.Record{
		EventID:  41,
		Provider: providerKernelPower,
		Time:     base.Add(offset),
		Data:     map[string]string{"BugcheckCode": bugcheck},
	}
}

// newest-first orders two records the way winevent.Query hands them to Latest,
// so a table case only has to state when its record happened.
func newestFirst(a, b winevent.Record) []winevent.Record {
	if a.Time.Before(b.Time) {
		return []winevent.Record{b, a}
	}
	return []winevent.Record{a, b}
}

// Event 41 is logged on ordinary reboots too, so only a non-zero BugcheckCode
// inside this boot's window may become "crash" — everything else here must fall
// through to the remaining rules (no other records, hence "unknown"), never to
// crash or unexpected.
func TestLatest_Event41Bugcheck(t *testing.T) {
	boot := bootRec(300)
	cases := []struct {
		name string
		rec  winevent.Record
		want Category
	}{
		{"stop error, decimal code as EvtRender writes it", rec41(10*time.Second, "159"), CatCrash},
		{"stop error, hex-rendered code", rec41(10*time.Second, "0x50"), CatCrash},
		{"ordinary reboot logs 41 with code 0", rec41(10*time.Second, "0"), CatUnknown},
		{"code 0 padded by XML chardata whitespace", rec41(10*time.Second, "\n      0\n    "), CatUnknown},
		// D4: zero has more than one rendering, and every one of them means
		// "no bug check". Reading the field as text made all of these outrank an
		// ordinary 1074 and post the loudest alert the tool has.
		{"code 0 with an 0x prefix", rec41(10*time.Second, "0x0"), CatUnknown},
		{"code 0 written out to full width", rec41(10*time.Second, "0x00000000"), CatUnknown},
		{"code 0 zero-padded without a prefix", rec41(10*time.Second, "00"), CatUnknown},
		{"code 0 with an upper-case prefix and padding", rec41(10*time.Second, " 0X0 "), CatUnknown},
		{"unparsable code is not a stop error", rec41(10*time.Second, "0xZZZ"), CatUnknown},
		{"no code at all", rec41(10*time.Second, ""), CatUnknown},
		{"stop error belonging to an earlier boot", rec41(-2*time.Hour, "159"), CatUnknown},
		{"stop error logged long after this boot", rec41(16*time.Minute, "159"), CatUnknown}, // past markerAfterBootWindow
		{
			"event 41 from another provider",
			winevent.Record{EventID: 41, Provider: providerKernelGeneral, Time: base.Add(10 * time.Second), Data: map[string]string{"BugcheckCode": "159"}},
			CatUnknown,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, ok := Latest(newestFirst(tc.rec, boot))
			if !ok {
				t.Fatal("expected a boot result")
			}
			if res.Category != tc.want {
				t.Fatalf("category = %q, want %q", res.Category, tc.want)
			}
		})
	}
}

// 6008 says "the previous shutdown was unexpected", and Windows writes it right
// after the boot it refers to. One from an earlier boot, or one that turns up
// long afterwards, belongs to a different cycle.
func TestLatest_UnexpectedMarkerWindow(t *testing.T) {
	boot := bootRec(301)
	cases := []struct {
		name   string
		offset time.Duration
		want   Category
	}{
		{"logged just after the boot", 30 * time.Second, CatUnexpected},
		// The literal offsets track markerAfterBootWindow (15 minutes) on purpose:
		// stating them here means widening that window trips this test.
		{"at the far edge of the window", 15 * time.Minute, CatUnexpected},
		{"past the window", 16 * time.Minute, CatUnknown},
		{"from an earlier boot", -time.Hour, CatUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			marker := winevent.Record{EventID: 6008, Provider: providerEventLog, Time: base.Add(tc.offset)}
			res, _ := Latest(newestFirst(marker, boot))
			if res.Category != tc.want {
				t.Fatalf("category = %q, want %q", res.Category, tc.want)
			}
		})
	}
}

// Records timestamped at or after the boot marker belong to the session that is
// only starting, not to the one that ended: a restart requested moments after
// booting must not be reported as the reason for that boot.
func TestLatest_RecordsAtOrAfterBootAreNotCorrelated(t *testing.T) {
	boot := bootRec(302)
	cases := []struct {
		name string
		rec  winevent.Record
	}{
		{
			"1074 for a shutdown that is starting now",
			rec1074(5*time.Minute, map[string]string{
				"param1": `C:\Windows\SystemApps\...\StartMenuExperienceHost.exe (PC)`,
				"param4": "0x0",
				"param5": "再起動",
				"param7": `PC\User`,
			}),
		},
		{
			"clean-shutdown 13 stamped exactly at the boot",
			winevent.Record{EventID: 13, Provider: providerKernelGeneral, Time: base},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, ok := Latest([]winevent.Record{tc.rec, boot})
			if !ok {
				t.Fatal("expected a boot result")
			}
			if res.Category != CatUnknown {
				t.Fatalf("category = %q, want unknown", res.Category)
			}
			if !res.PrevShutdown.IsZero() {
				t.Fatalf("PrevShutdown = %v, want zero", res.PrevShutdown)
			}
		})
	}
}

// An Event 13 from before the previous boot marker ended an older session;
// pairing it with this boot would report a downtime spanning a whole extra
// boot cycle.
func TestLatest_StaleCleanShutdownExcluded(t *testing.T) {
	records := []winevent.Record{
		bootRec(303),
		{EventID: 6005, Provider: providerEventLog, Time: base.Add(-2 * time.Minute)}, // previous boot marker
		{EventID: 13, Provider: providerKernelGeneral, Time: base.Add(-6 * time.Hour)},
	}
	res, _ := Latest(records)
	if res.Category != CatUnknown {
		t.Fatalf("category = %q, want unknown (stale 13 must be excluded)", res.Category)
	}
	if !res.PrevShutdown.IsZero() {
		t.Fatalf("PrevShutdown = %v, want zero", res.PrevShutdown)
	}
	if res.Downtime != 0 {
		t.Fatalf("downtime = %v, want 0", res.Downtime)
	}
}

// The Kernel-General 12 record can age out of the log while the EventLog
// service start (6005) is still there, so 6005 has to work as a boot marker on
// its own — including as the source of the reported boot time and record ID.
func TestLatest_EventLogStartAsBootMarker(t *testing.T) {
	records := []winevent.Record{
		{EventID: 6005, Provider: providerEventLog, Time: base, RecordID: 304, Computer: "PC"},
		{EventID: 6006, Provider: providerEventLog, Time: base.Add(-30 * time.Minute)},
	}
	res, ok := Latest(records)
	if !ok {
		t.Fatal("expected 6005 to serve as a boot marker")
	}
	if res.Category != CatShutdown {
		t.Fatalf("category = %q, want shutdown (clean cold boot)", res.Category)
	}
	if !res.BootTime.Equal(base) || res.RecordID != 304 || res.Computer != "PC" {
		t.Fatalf("boot = %v / %d / %q, want %v / 304 / \"PC\"", res.BootTime, res.RecordID, res.Computer, base)
	}
	if res.Downtime != 30*time.Minute {
		t.Fatalf("downtime = %v, want 30m", res.Downtime)
	}
}

// isPowerOff is evaluated before isUpdate. Each record below carries every
// update signal there is (servicing process, planned + Operating System reason,
// SYSTEM as the initiating user) yet describes a power-off, so calling it
// "update" would promise a boot that is not coming.
func TestLatest_PowerOffWinsOverUpdateSignals(t *testing.T) {
	for _, shutdownType := range []string{"シャットダウン", "電源を切る", "shutdown", "power off"} {
		t.Run(shutdownType, func(t *testing.T) {
			records := []winevent.Record{
				bootRec(305),
				rec1074(-1*time.Minute, map[string]string{
					"param1": `C:\windows\servicing\TrustedInstaller.exe (PC)`,
					"param3": "オペレーティング システム: アップグレード (計画済)",
					"param4": "0x80020003",
					"param5": shutdownType,
					"param7": `NT AUTHORITY\SYSTEM`,
				}),
			}
			res, _ := Latest(records)
			if res.Category != CatShutdown {
				t.Fatalf("category = %q, want shutdown", res.Category)
			}
		})
	}
}

func TestIsUpdate(t *testing.T) {
	cases := []struct {
		name string
		data map[string]string
		want bool
	}{
		{"servicing stack", map[string]string{"param1": `C:\windows\servicing\TrustedInstaller.exe (PC)`}, true},
		{"update orchestrator", map[string]string{"param1": `C:\Windows\UUS\amd64\MoUsoCoreWorker.exe (PC)`}, true},
		{"orchestrator client", map[string]string{"param1": `C:\Windows\System32\UsoClient.exe (PC)`}, true},
		{"legacy update client", map[string]string{"param1": `C:\Windows\System32\wuauclt.exe (PC)`}, true},
		{"process named for Windows Update", map[string]string{"param1": "Windows Update (PC)"}, true},
		{
			"planned + Operating System reason, unknown process",
			map[string]string{"param1": `C:\windows\system32\svchost.exe (PC)`, "param4": "0x80020010"},
			true,
		},
		{
			"planned restart initiated by SYSTEM, non-OS reason",
			map[string]string{"param1": `C:\windows\system32\svchost.exe (PC)`, "param4": "0x80040002", "param7": `NT AUTHORITY\SYSTEM`},
			true,
		},
		{
			"same, with the bare SYSTEM rendering",
			map[string]string{"param4": "0x80040002", "param7": " system "},
			true,
		},
		{
			"SYSTEM but unplanned",
			map[string]string{"param1": `C:\windows\system32\svchost.exe (PC)`, "param4": "0x40002", "param7": `NT AUTHORITY\SYSTEM`},
			false,
		},
		{
			"planned, but a user asked for it",
			map[string]string{"param1": `C:\Windows\explorer.exe (PC)`, "param4": "0x80040002", "param7": `PC\User`},
			false,
		},
		{"nothing to go on", map[string]string{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := winevent.Record{EventID: 1074, Provider: "User32", Time: base, Data: tc.data}
			if got := isUpdate(&r); got != tc.want {
				t.Fatalf("isUpdate = %v, want %v", got, tc.want)
			}
		})
	}
}

// PendingShutdown classifies the shutdown that is happening right now, so it
// runs the same power-off-before-update switch as Latest and must agree with it.
func TestPendingShutdown_Categories(t *testing.T) {
	cases := []struct {
		name string
		data map[string]string
		want Category
	}{
		{
			"user restart from the Start menu",
			map[string]string{
				"param1": `C:\Windows\SystemApps\...\StartMenuExperienceHost.exe (PC)`,
				"param4": "0x0",
				"param5": "再起動",
				"param7": `PC\User`,
			},
			CatManual,
		},
		{
			"japanese power off",
			map[string]string{"param1": `C:\Windows\explorer.exe (PC)`, "param4": "0x0", "param5": "電源を切る", "param7": `PC\User`},
			CatShutdown,
		},
		{
			"english power off",
			map[string]string{"param1": `C:\Windows\System32\shutdown.exe (PC)`, "param4": "0x80000000", "param5": "power off", "param7": `PC\User`},
			CatShutdown,
		},
		{
			"planned SYSTEM restart with no update process named",
			map[string]string{"param1": `C:\windows\system32\svchost.exe (PC)`, "param4": "0x80040002", "param5": "再起動", "param7": `NT AUTHORITY\SYSTEM`},
			CatUpdate,
		},
		{
			// Same invariant as TestLatest_PowerOffWinsOverUpdateSignals.
			"update signals plus a power off",
			map[string]string{
				"param1": `C:\windows\servicing\TrustedInstaller.exe (PC)`,
				"param4": "0x80020003",
				"param5": "シャットダウン",
				"param7": `NT AUTHORITY\SYSTEM`,
			},
			CatShutdown,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, ok := PendingShutdown([]winevent.Record{rec1074(-10*time.Second, tc.data)}, base, 5*time.Minute)
			if !ok {
				t.Fatal("expected a pending shutdown")
			}
			if res.Category != tc.want {
				t.Fatalf("category = %q, want %q", res.Category, tc.want)
			}
		})
	}
}

// Everything the pre-shutdown notice shows comes straight off the 1074.
func TestPendingShutdown_Fields(t *testing.T) {
	e := rec1074(-5*time.Second, map[string]string{
		"param1": `C:\Windows\System32\shutdown.exe (PC)`,
		"param3": "その他 (計画済)",
		"param4": "0x80000000",
		"param5": "再起動",
		"param7": `PC\User`,
	})
	e.Computer = "PC"
	res, ok := PendingShutdown([]winevent.Record{e}, base, 5*time.Minute)
	if !ok {
		t.Fatal("expected a pending shutdown")
	}
	if res.Computer != "PC" || res.Process != e.Data["param1"] || res.User != `PC\User` ||
		res.ShutdownType != "再起動" || res.ReasonText != "その他 (計画済)" || res.ReasonCode != 0x80000000 {
		t.Fatalf("fields not carried over: %+v", res)
	}
}

// With no 1074 at all the caller must fall back to a generic notice rather than
// invent a reason.
func TestPendingShutdown_NoInitiatingEvent(t *testing.T) {
	cases := map[string][]winevent.Record{
		"no records": nil,
		"only unrelated events": {
			bootRec(306),
			{EventID: 6006, Provider: providerEventLog, Time: base.Add(-time.Hour)},
		},
		// D5: 1074 from a provider that is not User32 is not ours to read.
		"1074 from another provider": {
			{
				EventID: 1074, Provider: "Microsoft-Windows-WindowsUpdateClient", Time: base.Add(-10 * time.Second),
				Data: map[string]string{"param4": "0x0", "param5": "再起動", "param7": `PC\User`},
			},
		},
	}
	for name, records := range cases {
		t.Run(name, func(t *testing.T) {
			if res, ok := PendingShutdown(records, base, 5*time.Minute); ok {
				t.Fatalf("ok = true (%q), want false without a 1074", res.Category)
			}
		})
	}
}

// The 1074 must plausibly describe the shutdown in progress: older than the
// recent window it is the previous shutdown's, and far in the future it is
// nonsense. A small forward skew is tolerated because the pre-shutdown notice
// is sent within seconds of the event being written.
func TestPendingShutdown_Window(t *testing.T) {
	const recent = 5 * time.Minute
	cases := []struct {
		name   string
		offset time.Duration
		wantOK bool
	}{
		{"just logged", -10 * time.Second, true},
		{"exactly at the recent boundary", -recent, true},
		{"one second past the boundary", -recent - time.Second, false},
		{"slight forward skew", time.Minute, true},
		{"implausibly future", 3 * time.Minute, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			records := []winevent.Record{rec1074(tc.offset, map[string]string{
				"param4": "0x0",
				"param5": "再起動",
				"param7": `PC\User`,
			})}
			if _, ok := PendingShutdown(records, base, recent); ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
		})
	}
}

// D2 regression. 6008 and the bug-check 41 are the only records paired with the
// boot by a time window instead of by cycle, and that window reaches a little
// way BEFORE the boot marker. A crash-and-restart-at-once sequence puts the
// previous boot's markers inside it: boot A comes up after the stop error, the
// user reboots straight away, and boot B — barely a minute and a half later —
// would be reported as a second stop error nobody experienced. The nearer of
// the two boot markers owns the record.
// How long the Event Log service takes to start after the OS start time stamped
// in Kernel-General 12 is a property of the machine. An earlier draft of the
// previous-boot scan capped that lag at two minutes; past the cap this boot's own
// 6005 was mistaken for the previous boot, the cycle's lower bound rose above its
// upper bound, and an ordinary restart was reported as 「不明」 with no process,
// user or reason. Direction settles it on its own, so no ceiling belongs here.
func TestLatest_SlowEventLogStartIsStillTheSameBoot(t *testing.T) {
	proc := `C:\Windows\explorer.exe`
	for _, lag := range []time.Duration{3 * time.Second, 2 * time.Minute, 10 * time.Minute, time.Hour} {
		t.Run(lag.String(), func(t *testing.T) {
			records := []winevent.Record{
				// The 6005 is committed before the 12 but stamped after it, so the
				// previous-boot scan walks past the anchor and reaches it.
				{EventID: 12, Provider: providerKernelGeneral, Time: base, RecordID: 200, Computer: "PC"},
				{EventID: 6005, Provider: providerEventLog, Time: base.Add(lag), RecordID: 199, Computer: "PC"},
				rec1074(-30*time.Second, map[string]string{
					"param1": proc,
					"param4": "0x0",
					"param5": "再起動",
					"param7": `PC\User`,
				}),
				{EventID: 12, Provider: providerKernelGeneral, Time: base.Add(-4 * time.Hour), RecordID: 100, Computer: "PC"},
			}
			res, ok := Latest(records)
			if !ok {
				t.Fatal("expected a boot result")
			}
			if res.Category != CatManual {
				t.Fatalf("category = %q, want manual: the 6005 lag must not detach this boot from its own 1074", res.Category)
			}
			if res.Process != proc {
				t.Fatalf("process = %q, want %q", res.Process, proc)
			}
		})
	}
}

func TestLatest_MarkersOfThePreviousBootNotReattributed(t *testing.T) {
	bootB := winevent.Record{EventID: 12, Provider: providerKernelGeneral, Time: base, RecordID: 403, Computer: "PC"}
	// newest-first, with the previous boot marker at the given offset.
	withPrevBoot := func(prevOffset time.Duration, marker winevent.Record) []winevent.Record {
		prev := winevent.Record{EventID: 12, Provider: providerKernelGeneral, Time: base.Add(prevOffset), RecordID: 401, Computer: "PC"}
		return []winevent.Record{bootB, marker, prev}
	}
	rec6008 := func(offset time.Duration) winevent.Record {
		return winevent.Record{EventID: 6008, Provider: providerEventLog, Time: base.Add(offset)}
	}
	cases := []struct {
		name    string
		records []winevent.Record
		want    Category
	}{
		{
			"bug check written moments after the previous boot",
			withPrevBoot(-100*time.Second, rec41(-90*time.Second, "159")),
			CatUnknown,
		},
		{
			"unexpected-shutdown marker written moments after the previous boot",
			withPrevBoot(-100*time.Second, rec6008(-95*time.Second)),
			CatUnknown,
		},
		{
			// The pre-boot slack still has to work: a marker stamped a moment
			// before this boot, with the previous boot hours away, is this boot's.
			"bug check just before this boot, previous boot hours away",
			withPrevBoot(-8*time.Hour, rec41(-3*time.Second, "159")),
			CatCrash,
		},
		{
			"unexpected-shutdown marker just before this boot, previous boot hours away",
			withPrevBoot(-8*time.Hour, rec6008(-3*time.Second)),
			CatUnexpected,
		},
		{
			// With the log truncated to a single boot there is nothing nearer to
			// compare against, so the slack applies unconditionally.
			"bug check just before this boot, no previous boot in the log",
			[]winevent.Record{bootB, rec41(-3*time.Second, "159")},
			CatCrash,
		},
		{
			// Discriminates the rule from a narrower fixed window. The rejection
			// cases above put the marker 90s before this boot and the previous boot
			// 100s before it; any implementation that simply shrank the pre-boot
			// slack to a constant under 90s would pass them for the wrong reason.
			// Here the same 80s offset must be ACCEPTED, because the previous boot
			// is hours away and nearest-boot-wins is what decides ownership — not
			// how close to this boot the marker happens to sit.
			"bug check 80 seconds before this boot, previous boot hours away",
			withPrevBoot(-8*time.Hour, rec41(-80*time.Second, "159")),
			CatCrash,
		},
		{
			"unexpected-shutdown marker 80 seconds before this boot, previous boot hours away",
			withPrevBoot(-8*time.Hour, rec6008(-80*time.Second)),
			CatUnexpected,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, ok := Latest(tc.records)
			if !ok {
				t.Fatal("expected a boot result")
			}
			if res.Category != tc.want {
				t.Fatalf("category = %q, want %q", res.Category, tc.want)
			}
		})
	}
}

// D3 regression. w32time steps the clock seconds into the boot, so a boot
// marker can carry a timestamp EARLIER than the boot before it. Picking the
// previous marker by timestamp then skips the real one and stretches the cycle
// across two boots, pulling the older boot's Windows Update 1074 onto a plain
// user restart. Log order survives the step, so the previous marker is the one
// that follows in the log.
func TestLatest_BackwardClockStepDoesNotWidenTheCycle(t *testing.T) {
	// Newest write first: boot C (clock stepped back to 12:25) / user restart
	// 12:40 / boot B 12:31 / Windows Update restart 12:10 / boot A 12:00.
	records := []winevent.Record{
		{EventID: 12, Provider: providerKernelGeneral, Time: base.Add(25 * time.Minute), RecordID: 404, Computer: "PC"},
		rec1074(40*time.Minute, map[string]string{
			"param1": `C:\Windows\SystemApps\...\StartMenuExperienceHost.exe (PC)`,
			"param4": "0x0",
			"param5": "再起動",
			"param7": `PC\User`,
		}),
		{EventID: 12, Provider: providerKernelGeneral, Time: base.Add(31 * time.Minute), RecordID: 402, Computer: "PC"},
		rec1074(10*time.Minute, map[string]string{
			"param1": `C:\windows\servicing\TrustedInstaller.exe (PC)`,
			"param4": "0x80020003",
			"param5": "再起動",
			"param7": `NT AUTHORITY\SYSTEM`,
		}),
		{EventID: 12, Provider: providerKernelGeneral, Time: base, RecordID: 400, Computer: "PC"},
	}
	res, ok := Latest(records)
	if !ok {
		t.Fatal("expected a boot result")
	}
	// Reporting "unknown" is the honest answer here: the 1074 that really
	// preceded this boot is stamped after it, so nothing in the cycle survives.
	if res.Category != CatUnknown {
		t.Fatalf("category = %q, want unknown (a stepped clock must not reach into an older cycle)", res.Category)
	}
	if res.Process != "" {
		t.Fatalf("process = %q, want empty", res.Process)
	}
}

// D5 regression. 1074 and 6008 are matched across every provider, unlike
// EventID 12, which is filtered because other providers emit it too. Nothing
// says another System-channel provider writes these ids, so this is defence in
// depth: the classifier must not act on a record it cannot attribute.
func TestLatest_ProviderFilters(t *testing.T) {
	boot := bootRec(500)
	restart1074 := map[string]string{
		"param1": `C:\Windows\SystemApps\...\StartMenuExperienceHost.exe (PC)`,
		"param4": "0x0",
		"param5": "再起動",
		"param7": `PC\User`,
	}
	foreign1074 := rec1074(-30*time.Second, restart1074)
	foreign1074.Provider = "Microsoft-Windows-WindowsUpdateClient"
	upper1074 := rec1074(-30*time.Second, restart1074)
	upper1074.Provider = "USER32"
	cases := []struct {
		name string
		rec  winevent.Record
		want Category
	}{
		{"1074 from User32", rec1074(-30*time.Second, restart1074), CatManual},
		// 1074 is a classic (non-manifest) source: its provider name is rendered
		// from the EventLog registry key rather than a manifest, and older
		// Windows rendered it upper-case. Same source, so it is still ours.
		{"1074 from USER32", upper1074, CatManual},
		{"1074 from another provider", foreign1074, CatUnknown},
		{
			"6008 from the EventLog service",
			winevent.Record{EventID: 6008, Provider: providerEventLog, Time: base.Add(30 * time.Second)},
			CatUnexpected,
		},
		{
			"6008 from another provider",
			winevent.Record{EventID: 6008, Provider: providerKernelPower, Time: base.Add(30 * time.Second)},
			CatUnknown,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, ok := Latest(newestFirst(tc.rec, boot))
			if !ok {
				t.Fatal("expected a boot result")
			}
			if res.Category != tc.want {
				t.Fatalf("category = %q, want %q", res.Category, tc.want)
			}
		})
	}
}

// D1 regression. PendingShutdown runs before there is a boot event for the
// shutdown in progress, so a boot marker NEWER than the 1074 means the machine
// already came back up after that 1074: it describes a shutdown that is over.
// The caller's recency window cannot tell the two apart, because a boot can
// easily land inside it.
func TestPendingShutdown_BootMarkerAfterThe1074(t *testing.T) {
	cases := []struct {
		name    string
		records []winevent.Record
		wantOK  bool
	}{
		{
			"Kernel-General 12 newer than the 1074",
			[]winevent.Record{
				{EventID: 12, Provider: providerKernelGeneral, Time: base.Add(-3 * time.Minute), RecordID: 501},
				rec1074(-4*time.Minute, map[string]string{
					"param1": `C:\Windows\UUS\amd64\MoUsoCoreWorker.exe (PC)`,
					"param4": "0x80020003",
					"param5": "再起動",
					"param7": `NT AUTHORITY\SYSTEM`,
				}),
			},
			false,
		},
		{
			"EventLog 6005 newer than the 1074",
			[]winevent.Record{
				{EventID: 6005, Provider: providerEventLog, Time: base.Add(-3 * time.Minute), RecordID: 502},
				rec1074(-4*time.Minute, map[string]string{"param4": "0x0", "param5": "再起動", "param7": `PC\User`}),
			},
			false,
		},
		{
			// A backward clock step larger than the uptime leaves this session's
			// OWN boot marker stamped ahead of the 1074 being written now. The
			// log's order is not fooled: the marker was written first, so the
			// machine has not booted since, and the reason must still be read.
			"boot marker stamped after the 1074 by a backward clock step",
			[]winevent.Record{
				rec1074(-10*time.Second, map[string]string{"param4": "0x0", "param5": "再起動", "param7": `PC\User`}),
				{EventID: 12, Provider: providerKernelGeneral, Time: base.Add(15 * time.Minute), RecordID: 504},
			},
			true,
		},
		{
			// Ties have to fall somewhere: a marker stamped on the same second
			// as the 1074 but written after it is a boot that already happened.
			"boot marker written after the 1074 and stamped identically",
			[]winevent.Record{
				{EventID: 12, Provider: providerKernelGeneral, Time: base.Add(-10 * time.Second), RecordID: 505},
				rec1074(-10*time.Second, map[string]string{"param4": "0x0", "param5": "再起動", "param7": `PC\User`}),
			},
			false,
		},
		{
			// The ordinary case: the newest boot marker is the one this session
			// started from, so it is older than the 1074 being logged now.
			"boot marker older than the 1074",
			[]winevent.Record{
				rec1074(-10*time.Second, map[string]string{"param4": "0x0", "param5": "再起動", "param7": `PC\User`}),
				{EventID: 12, Provider: providerKernelGeneral, Time: base.Add(-2 * time.Hour), RecordID: 503},
			},
			true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := PendingShutdown(tc.records, base, 5*time.Minute); ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
		})
	}
}

// D3 follow-up. One boot writes BOTH markers — Kernel-General 12, stamped at
// the OS start time, and EventLog 6005, stamped when the log service came up a
// few seconds later — and either may be committed to the log first. When the
// 6005 is committed first it sits immediately AFTER the 12 in the newest-first
// slice, exactly where the scan for the previous boot looks. Taking it would
// put the cycle's lower bound (the 6005, stamped after the 12) above its upper
// bound, discarding every record of the session that just ended and reporting
// 「不明」 with no process, user or reason for an ordinary restart.
func TestLatest_CompanionMarkerIsNotThePreviousBoot(t *testing.T) {
	const proc = `C:\Windows\SystemApps\...\StartMenuExperienceHost.exe (PC)`
	userRestart := rec1074(-30*time.Second, map[string]string{
		"param1": proc,
		"param4": "0x0",
		"param5": "再起動",
		"param7": `PC\User`,
	})
	prevBoot := winevent.Record{EventID: 12, Provider: providerKernelGeneral, Time: base.Add(-4 * time.Hour), RecordID: 100, Computer: "PC"}
	twelve := func(rid int64) winevent.Record {
		return winevent.Record{EventID: 12, Provider: providerKernelGeneral, Time: base, RecordID: rid, Computer: "PC"}
	}
	logStart := func(rid int64) winevent.Record {
		return winevent.Record{EventID: 6005, Provider: providerEventLog, Time: base.Add(3 * time.Second), RecordID: rid, Computer: "PC"}
	}
	// Newest write first, so the marker with the higher RecordID leads.
	cases := map[string][]winevent.Record{
		"6005 committed before the 12": {twelve(200), logStart(199), userRestart, prevBoot},
		"12 committed before the 6005": {logStart(200), twelve(199), userRestart, prevBoot},
	}
	for name, records := range cases {
		t.Run(name, func(t *testing.T) {
			res, ok := Latest(records)
			if !ok {
				t.Fatal("expected a boot result")
			}
			if !res.BootTime.Equal(base) {
				t.Fatalf("boot time = %v, want the Kernel-General 12 at %v", res.BootTime, base)
			}
			if res.Category != CatManual {
				t.Fatalf("category = %q, want manual", res.Category)
			}
			if res.Process != proc {
				t.Fatalf("process = %q, want %q", res.Process, proc)
			}
		})
	}
}

// D4 follow-up. The bug-check code is not only classified, it is rendered: it
// reaches the Discord embed's 詳細 field verbatim. EvtRender returns the value
// as XML chardata, which carries the surrounding indentation with it, so an
// untrimmed code posts a field with newlines embedded in it.
func TestLatest_CrashDetailIsTrimmed(t *testing.T) {
	res, ok := Latest(newestFirst(rec41(10*time.Second, "\n      159\n    "), bootRec(600)))
	if !ok {
		t.Fatal("expected a boot result")
	}
	if res.Category != CatCrash {
		t.Fatalf("category = %q, want crash", res.Category)
	}
	if res.Detail != "Bugcheck code 159" {
		t.Fatalf("detail = %q, want %q", res.Detail, "Bugcheck code 159")
	}
}
