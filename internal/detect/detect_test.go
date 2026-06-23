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
			"param7": `PC\Keita`,
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
			"param7": `PC\Keita`,
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

// A 1074 from a previous boot cycle (older than the previous boot marker) must
// not be paired with the current boot.
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

func TestLatest_Stray1074FromPrevCycleExcluded(t *testing.T) {
	records := []winevent.Record{
		bootRec(203), // current boot at base
		{EventID: 6005, Provider: providerEventLog, Time: base.Add(-1 * time.Minute)}, // previous boot marker
		rec1074(-3*time.Minute, map[string]string{ // 1074 from before the previous boot
			"param1": `C:\...\StartMenuExperienceHost.exe (PC)`,
			"param4": "0x0",
			"param5": "再起動",
			"param7": `PC\Keita`,
		}),
	}
	res, _ := Latest(records)
	if res.Category != CatUnknown {
		t.Fatalf("category = %q, want unknown (stray 1074 must be excluded)", res.Category)
	}
}
