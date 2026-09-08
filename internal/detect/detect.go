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

// SystemChannel and EventQuery select the relevant System-log events. The query
// matches on EventID alone and every provider check happens in code: an EventID
// is not an identity on its own (Wininit and UserModePowerService also emit
// EventID 12, which is why that one has always been filtered).
const (
	SystemChannel = "System"
	EventQuery    = "*[System[(EventID=1074 or EventID=6005 or EventID=6006 or EventID=6008 or EventID=41 or EventID=12 or EventID=13)]]"

	providerKernelGeneral = "Microsoft-Windows-Kernel-General"
	providerKernelPower   = "Microsoft-Windows-Kernel-Power"
	providerEventLog      = "EventLog"
	providerUser32        = "User32"
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

// isShutdownRequest reports whether r is the Event 1074 that records a
// shutdown/restart request, the classifier's primary signal.
//
// The provider clause is defence in depth: no other System-channel provider is
// known to write 1074, but this is the record every category below "unexpected"
// is derived from, so it may only be read when it is attributable. User32 is the
// name observed on Windows 10/11 (see the package doc and the verbatim record in
// the tests).
//
// The comparison folds case because 1074 comes from a classic (non-manifest)
// event source, whose name is rendered from its EventLog registry key rather
// than from a manifest — older Windows rendered it "USER32". The fold gives up
// nothing (no plausible foreign provider differs from "User32" by case alone)
// and the cost of a miss is total: no process, user or reason on any reboot,
// and a permanent fallback to the generic pre-shutdown notice. The
// manifest-based providers stay exact comparisons; their names are stable.
func isShutdownRequest(r *winevent.Record) bool {
	return r.EventID == 1074 && strings.EqualFold(r.Provider, providerUser32)
}

// Latest returns the classification of the most recent boot found in records
// (which must be ordered newest-first, as returned by winevent.Query). The
// second result is false when no boot marker is present.
func Latest(records []winevent.Record) (*Result, bool) {
	bootIdx := indexFirst(records, func(r *winevent.Record) bool {
		return r.EventID == 12 && r.Provider == providerKernelGeneral
	})
	if bootIdx < 0 {
		// Fall back to the Event Log service start as a boot marker.
		bootIdx = indexFirst(records, func(r *winevent.Record) bool {
			return r.EventID == 6005 && r.Provider == providerEventLog
		})
	}
	if bootIdx < 0 {
		return nil, false
	}
	boot := &records[bootIdx]

	bt := boot.Time
	res := &Result{
		Category: CatUnknown,
		BootTime: bt,
		RecordID: boot.RecordID,
		Computer: boot.Computer,
	}

	// Previous boot marker, so we can scope everything to the current boot cycle.
	// It is the next marker in LOG ORDER, not the newest one carrying an earlier
	// timestamp: w32time steps the clock seconds into a boot, so a marker can end
	// up stamped before the boot that preceded it. Selecting by timestamp then
	// skips the real previous marker and stretches inCycle across two cycles,
	// which drags an older boot's 1074 (a servicing reboot, say) onto the boot
	// being reported. The slice is newest-first as the log wrote it, and that
	// order survives a clock step. inCycle itself still compares timestamps:
	// moving the correlation onto record-ID ranges would rewrite the contract the
	// whole package rests on, for a rare fault.
	//
	// The scan has to step over this boot's OWN other marker first. A boot writes
	// both a Kernel-General 12 and an EventLog 6005 seconds apart, and the log may
	// commit the 6005 first — which lands it immediately after the 12, exactly
	// where this scan looks. Accepting it would put the cycle's lower bound above
	// its upper bound (the 6005 is stamped when the log service came up, after the
	// OS start time the 12 carries) and discard every record of the session that
	// just ended: an ordinary restart would report 「不明」 with no process, user
	// or reason.
	//
	// What identifies that companion is its DIRECTION, not merely its nearness:
	// this boot's 6005 is stamped at or after bt, whereas the previous boot's
	// 6005 — even one from a cycle that lasted a minute — is stamped before it.
	// Two markers of the same id are always two different boots. The check below
	// is written symmetrically, but only the 6005 case can actually arise:
	// whenever the log holds any Kernel-General 12 that is the anchor, so the
	// companion following it is always the 6005.
	//
	// Direction alone, with no upper bound on the lag. How long the Event Log
	// service takes to start after the OS start time the 12 carries is a property
	// of the machine, not of this tool: a 2-minute ceiling here was measured to
	// turn an ordinary restart into 「不明」 with no process, user or reason on any
	// host slower than that, which the timestamp-ordered scan this replaced never
	// did. A bound that rejects nothing direction does not already reject can only
	// ever misfire.
	var prevBoot *winevent.Record
	for i := bootIdx + 1; i < len(records); i++ {
		r := &records[i]
		if !isBootMarker(r) {
			continue
		}
		if r.EventID != boot.EventID && !r.Time.Before(bt) {
			continue // this boot's other marker, not the previous boot
		}
		prevBoot = r
		break
	}
	// inCycle reports whether t belongs to the session that just ended (after the
	// previous boot, before this one). With no previous boot known, only the
	// upper bound applies.
	inCycle := func(t time.Time) bool {
		if !t.Before(bt) {
			return false
		}
		return prevBoot == nil || t.After(prevBoot.Time)
	}

	// nearBoot pairs the two records that describe the session that ended but are
	// written during the one starting (6008 and the bug-check 41). They sit after
	// bt, so inCycle cannot hold them; a window around bt does.
	//
	// The window reaches a little before bt because such a marker can be stamped
	// just ahead of the boot marker it belongs to. That slack must not reach into
	// the previous session: after a stop error the machine is usually rebooted
	// within a minute or two, and the previous boot's 41/6008 would then fall
	// inside it and be reported as a second crash that never happened. So a
	// pre-bt record counts only while it is nearer to this boot than to the
	// previous one. (Requiring that no boot marker sit between the record and bt
	// would not work: 6008 is written by the EventLog service *after* 6005, so no
	// marker ever falls between them.)
	nearBoot := func(t time.Time) bool {
		if !within(t, bt, -2*time.Minute, markerAfterBootWindow) {
			return false
		}
		if !t.Before(bt) {
			return true
		}
		return prevBoot == nil || bt.Sub(t) < t.Sub(prevBoot.Time)
	}

	// Initiating restart/shutdown request for THIS boot (newest 1074 in the cycle).
	init1074 := findFirst(records, func(r *winevent.Record) bool {
		return isShutdownRequest(r) && inCycle(r.Time)
	})
	// Unexpected previous shutdown (logged shortly after the new boot). The
	// provider clause is defence in depth, like isShutdownRequest's.
	unexpected := findFirst(records, func(r *winevent.Record) bool {
		return r.EventID == 6008 && r.Provider == providerEventLog && nearBoot(r.Time)
	})
	// Stop error / BSOD: Kernel-Power 41 with a real bug-check code.
	//
	// The code is parsed rather than compared as text. BugcheckCode is a
	// manifest-typed integer that Kernel-Power renders in decimal (the record in
	// this package's tests reads "159"; only BugcheckParameter1-4 are
	// win:HexInt64), so no rendering of zero other than "0" has actually been
	// seen — this is hardening, not a fix for an observed failure. It earns its
	// place because of what it would cost: read as text, "0x0", "00" and
	// "0x00000000" are all non-empty and none of them equals "0", so on a build
	// rendering the field that way EVERY ordinary reboot would outrank its own
	// 1074 and post the loudest alert the tool has. parseHex normalises prefix,
	// padding and whitespace, and fails closed (unparsable -> 0 -> not a crash),
	// which is the right direction to be wrong in here. Decimal codes are read as
	// hex, which is harmless: only zero vs non-zero is used.
	crash := findFirst(records, func(r *winevent.Record) bool {
		if r.EventID != 41 || r.Provider != providerKernelPower {
			return false
		}
		return parseHex(r.Data["BugcheckCode"]) != 0 && nearBoot(r.Time)
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
		res.Detail = "Bugcheck code " + strings.TrimSpace(crash.Data["BugcheckCode"])
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
// It returns false when no 1074 describes the shutdown now under way — none
// logged within recent of now, or one the machine has already booted out of —
// so the caller can fall back to a generic "shutting down" notice rather than
// report a stale reason from an earlier shutdown.
func PendingShutdown(records []winevent.Record, now time.Time, recent time.Duration) (*Result, bool) {
	eIdx := indexFirst(records, isShutdownRequest)
	if eIdx < 0 {
		return nil, false
	}
	e := &records[eIdx]
	if now.Sub(e.Time) > recent || e.Time.After(now.Add(2*time.Minute)) {
		return nil, false // too old (previous shutdown) or implausibly future
	}
	// Scope to the current boot cycle, as Latest does. The shutdown in progress
	// has no boot event of its own yet, so a boot marker written AFTER the 1074
	// says the machine already came back up after that request: it describes a
	// shutdown that is over. Recency alone cannot tell the two apart — a whole
	// boot fits comfortably inside the caller's window.
	//
	// "After" is decided by LOG ORDER, for the same reason the previous-boot scan
	// above is: a backward clock step larger than the uptime leaves this very
	// session's marker stamped ahead of the 1074 being written right now, and
	// comparing stamps would then throw away the reason for the shutdown actually
	// under way. The slice is newest-first as written, so a marker at a LOWER
	// index than the 1074 was written after it. That also settles the tie a stamp
	// comparison leaves open, where a marker carries the 1074's own timestamp.
	// The markers are already in this slice; EventQuery selects 12 and 6005 and
	// the service passes the result straight through.
	if bIdx := indexFirst(records, isBootMarker); bIdx >= 0 && bIdx < eIdx {
		return nil, false
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

// isPowerOff reports whether Event 1074's param5 describes a full power-off
// rather than a restart.
//
// param5 is the localised shutdown-type string, and Event 1074 carries no
// numeric equivalent, so matching text is the only option here. The list below
// is therefore the known-good set rather than a complete one: a locale whose
// string is missing falls through to the restart path and is reported as
// "manual". Confirmed renderings:
//
//	en  "power off", "shutdown"
//	ja  "電源を切る"  (observed on Japanese Windows 11)
//	ja  "シャットダウン"
//
// Add a locale here only from an actual Event 1074, not from a translation of
// the word "shutdown" — Japanese renders the power-off case as 電源を切る, not
// as シャットダウン, and assuming otherwise is what let this case slip through.
func isPowerOff(e *winevent.Record) bool {
	st := strings.ToLower(e.Data["param5"])
	return strings.Contains(st, "power off") ||
		strings.Contains(st, "shutdown") ||
		strings.Contains(st, "シャットダウン") ||
		strings.Contains(st, "電源を切る")
}

func findFirst(records []winevent.Record, pred func(*winevent.Record) bool) *winevent.Record {
	if i := indexFirst(records, pred); i >= 0 {
		return &records[i]
	}
	return nil
}

// indexFirst returns the index of the first matching record, or -1. Callers that
// need the log's own ORDER, not just the record, go through it: the slice is
// newest-first as written, which stays meaningful when the timestamps do not.
func indexFirst(records []winevent.Record, pred func(*winevent.Record) bool) int {
	for i := range records {
		if pred(&records[i]) {
			return i
		}
	}
	return -1
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
	// Parse directly as 32-bit: reason codes are 32-bit, and this makes the
	// value provably fit in uint32 (ParseUint rejects anything larger), so the
	// conversion can't silently truncate (CodeQL go/incorrect-integer-conversion).
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return 0
	}
	return uint32(v)
}
