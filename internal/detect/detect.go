// Package detect classifies the most recent Windows boot from System event-log
// records into a restart category (update / manual / unexpected / crash / ...).
//
// Classification relies on locale-independent signals so it works regardless of
// the machine's display language:
//   - Event 1074 (User32): param4 shutdown reason code, param1 initiating process.
//   - Event 6008 (EventLog): the previous shutdown was unexpected.
//   - Event 41 (Kernel-Power) with BugcheckCode != 0: stop error (BSOD).
//
// Event 41 on its own is deliberately NOT treated as "unexpected": modern
// Windows logs it after ordinary reboots (BugcheckCode == 0) as well.
//
// All pairing is scoped to the *current boot cycle* — i.e. records between the
// previous boot marker and this boot — so a stale 1074/shutdown record from an
// earlier reboot is never mis-attributed to the latest boot, and a slow update
// (whose 1074 may precede the boot by well over half an hour) is still paired.
package detect

import (
	"strconv"
	"strings"
	"time"

	"github.com/223n/restart-message/internal/winevent"
)

// SystemChannel and EventQuery select the relevant System-log events. EventID 12
// is intentionally matched across all providers; we filter to Kernel-General in
// code (Wininit and UserModePowerService also emit EventID 12).
const (
	SystemChannel = "System"
	EventQuery    = "*[System[(EventID=1074 or EventID=6005 or EventID=6006 or EventID=6008 or EventID=41 or EventID=12 or EventID=13)]]"

	providerKernelGeneral = "Microsoft-Windows-Kernel-General"
	providerKernelPower   = "Microsoft-Windows-Kernel-Power"
	providerEventLog      = "EventLog"
)

// Category is the classified restart reason.
type Category string

const (
	CatUpdate     Category = "update"
	CatManual     Category = "manual"
	CatShutdown   Category = "shutdown"
	CatUnexpected Category = "unexpected"
	CatCrash      Category = "crash"
	CatUnknown    Category = "unknown"
)

// Result describes the latest boot and why it happened.
type Result struct {
	Category     Category      `json:"category"`
	BootTime     time.Time     `json:"boot_time"`
	RecordID     int64         `json:"record_id"`
	Computer     string        `json:"computer"`
	ReasonText   string        `json:"reason_text"`             // Windows' own reason string (param3) or a derived description
	Process      string        `json:"process,omitempty"`       // initiating process (param1)
	User         string        `json:"user,omitempty"`          // initiating user (param7)
	ShutdownType string        `json:"shutdown_type,omitempty"` // param5 (e.g. 再起動 / restart)
	ReasonCode   uint32        `json:"reason_code,omitempty"`   // param4
	PrevShutdown time.Time     `json:"prev_shutdown,omitempty"`
	Downtime     time.Duration `json:"downtime,omitempty"`
	Detail       string        `json:"detail,omitempty"`
}

// Shutdown reason-code bit layout (winreason.h: SHTDN_REASON_*).
const (
	reasonFlagPlanned = 0x80000000
	reasonMajorMask   = 0x00FF0000
	reasonMajorOS     = 0x00020000 // SHTDN_REASON_MAJOR_OPERATINGSYSTEM
)

// markerAfterBootWindow bounds how long after a boot the "unexpected shutdown"
// (6008) and bug-check (41) records may appear and still belong to that boot.
const markerAfterBootWindow = 15 * time.Minute

func isBootMarker(r *winevent.Record) bool {
	return (r.EventID == 12 && r.Provider == providerKernelGeneral) ||
		(r.EventID == 6005 && r.Provider == providerEventLog)
}

// Latest returns the classification of the most recent boot found in records
// (which must be ordered newest-first, as returned by winevent.Query). The
// second result is false when no boot marker is present.
func Latest(records []winevent.Record) (*Result, bool) {
	boot := findFirst(records, func(r *winevent.Record) bool {
		return r.EventID == 12 && r.Provider == providerKernelGeneral
	})
	if boot == nil {
		// Fall back to the Event Log service start as a boot marker.
		boot = findFirst(records, func(r *winevent.Record) bool {
			return r.EventID == 6005 && r.Provider == providerEventLog
		})
	}
	if boot == nil {
		return nil, false
	}

	bt := boot.Time
	res := &Result{
		Category: CatUnknown,
		BootTime: bt,
		RecordID: boot.RecordID,
		Computer: boot.Computer,
	}

	// Previous boot marker, so we can scope everything to the current boot cycle.
	prevBoot := findFirst(records, func(r *winevent.Record) bool {
		return isBootMarker(r) && r.Time.Before(bt)
	})
	// inCycle reports whether t belongs to the session that just ended (after the
	// previous boot, before this one). With no previous boot known, only the
	// upper bound applies.
	inCycle := func(t time.Time) bool {
		if !t.Before(bt) {
			return false
		}
		return prevBoot == nil || t.After(prevBoot.Time)
	}

	// Initiating restart/shutdown request for THIS boot (newest 1074 in the cycle).
	init1074 := findFirst(records, func(r *winevent.Record) bool {
		return r.EventID == 1074 && inCycle(r.Time)
	})
	// Unexpected previous shutdown (logged shortly after the new boot).
	unexpected := findFirst(records, func(r *winevent.Record) bool {
		return r.EventID == 6008 && within(r.Time, bt, -2*time.Minute, markerAfterBootWindow)
	})
	// Stop error / BSOD: Kernel-Power 41 with a real bug-check code.
	crash := findFirst(records, func(r *winevent.Record) bool {
		if r.EventID != 41 || r.Provider != providerKernelPower {
			return false
		}
		bc := strings.TrimSpace(r.Data["BugcheckCode"])
		return bc != "" && bc != "0" && within(r.Time, bt, -2*time.Minute, markerAfterBootWindow)
	})

	// Previous clean shutdown (within the cycle) for downtime and the clean
	// shutdown -> cold boot fallback.
	prev := findFirst(records, func(r *winevent.Record) bool {
		return r.EventID == 13 && r.Provider == providerKernelGeneral && inCycle(r.Time)
	})
	if prev == nil {
		prev = findFirst(records, func(r *winevent.Record) bool {
			return r.EventID == 6006 && r.Provider == providerEventLog && inCycle(r.Time)
		})
	}
	if prev != nil {
		res.PrevShutdown = prev.Time
		if d := bt.Sub(prev.Time); d > 0 {
			res.Downtime = d
		}
	}

	switch {
	case crash != nil:
		res.Category = CatCrash
		res.ReasonText = "予期しないシャットダウン（ストップエラー/BSOD）"
		res.Detail = "Bugcheck code " + crash.Data["BugcheckCode"]
	case unexpected != nil:
		res.Category = CatUnexpected
		res.ReasonText = "予期しないシャットダウン（電源喪失・ハングなど）"
	case init1074 != nil:
		res.Process = init1074.Data["param1"]
		res.User = init1074.Data["param7"]
		res.ShutdownType = init1074.Data["param5"]
		res.ReasonText = init1074.Data["param3"]
		res.ReasonCode = parseHex(init1074.Data["param4"])
		// Order matters: a power-off must win over the update heuristic so a
		// planned SYSTEM-initiated full shutdown is never labelled "update".
		switch {
		case isPowerOff(init1074):
			res.Category = CatShutdown
		case isUpdate(init1074):
			res.Category = CatUpdate
		default:
			res.Category = CatManual
		}
	case !res.PrevShutdown.IsZero():
		// No initiating 1074 survived, but the previous session ended cleanly
		// (Event 13 / 6006): a normal shutdown followed by a cold boot.
		res.Category = CatShutdown
		res.ReasonText = "クリーンシャットダウン後の起動"
	default:
		res.Category = CatUnknown
		res.ReasonText = "再起動の理由を特定できませんでした"
	}

	return res, true
}

// PendingShutdown classifies an in-progress shutdown/restart from the most
// recent Event 1074, for use at pre-shutdown time (there is no boot event yet).
// It returns false when no 1074 was logged within recent of now, so the caller
// can fall back to a generic "shutting down" notice rather than report a stale
// reason from an earlier shutdown.
func PendingShutdown(records []winevent.Record, now time.Time, recent time.Duration) (*Result, bool) {
	e := findFirst(records, func(r *winevent.Record) bool {
		return r.EventID == 1074
	})
	if e == nil {
		return nil, false
	}
	if now.Sub(e.Time) > recent || e.Time.After(now.Add(2*time.Minute)) {
		return nil, false // too old (previous shutdown) or implausibly future
	}
	res := &Result{
		Computer:     e.Computer,
		Process:      e.Data["param1"],
		User:         e.Data["param7"],
		ShutdownType: e.Data["param5"],
		ReasonText:   e.Data["param3"],
		ReasonCode:   parseHex(e.Data["param4"]),
	}
	switch {
	case isPowerOff(e):
		res.Category = CatShutdown
	case isUpdate(e):
		res.Category = CatUpdate
	default:
		res.Category = CatManual
	}
	return res, true
}

func isUpdate(e *winevent.Record) bool {
	proc := strings.ToLower(e.Data["param1"])
	for _, marker := range []string{
		"trustedinstaller.exe", // servicing stack (Windows Update / feature update)
		"mousocoreworker.exe",  // Update Session Orchestrator (Win10/11)
		"usoclient.exe",        // Update Session Orchestrator client
		"wuauclt.exe",          // legacy Windows Update client
		"windows update",
	} {
		if strings.Contains(proc, marker) {
			return true
		}
	}

	code := parseHex(e.Data["param4"])
	// Planned + "Operating System" major reason == servicing/update reboot.
	if code&reasonFlagPlanned != 0 && code&reasonMajorMask == reasonMajorOS {
		return true
	}
	// A planned restart initiated by SYSTEM is, on a workstation, practically
	// always servicing. (Power-offs are handled earlier, so this only sees
	// restarts.)
	user := strings.ToUpper(strings.TrimSpace(e.Data["param7"]))
	if code&reasonFlagPlanned != 0 && (user == `NT AUTHORITY\SYSTEM` || user == "SYSTEM") {
		return true
	}
	return false
}

func isPowerOff(e *winevent.Record) bool {
	st := strings.ToLower(e.Data["param5"])
	return strings.Contains(st, "power off") ||
		strings.Contains(st, "shutdown") ||
		strings.Contains(st, "シャットダウン")
}

func findFirst(records []winevent.Record, pred func(*winevent.Record) bool) *winevent.Record {
	for i := range records {
		if pred(&records[i]) {
			return &records[i]
		}
	}
	return nil
}

func within(t, ref time.Time, lo, hi time.Duration) bool {
	d := t.Sub(ref)
	return d >= lo && d <= hi
}

func parseHex(s string) uint32 {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "0x")
	s = strings.TrimPrefix(s, "0X")
	if s == "" {
		return 0
	}
	v, err := strconv.ParseUint(s, 16, 64)
	if err != nil {
		return 0
	}
	return uint32(v)
}
