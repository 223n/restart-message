// This file must stay free of build tags and of anything declared in
// winevent.go (//go:build windows), so that parseRecord — the boundary where
// raw event XML becomes the Record the detection logic consumes — keeps being
// exercised on Linux and macOS as well as on Windows. Nothing in here has to
// assert that: the file compiles into the package on every GOOS, so a
// Windows-only symbol creeping in would fail the build outright.

package winevent

import (
	"reflect"
	"testing"
	"time"
)

// The XML EvtRender actually returns for a User32 1074, down to the details the
// decoder has to tolerate: the default namespace, the Qualifiers attribute on
// EventID, and the System children (Version, Keywords, Correlation, ...) that
// xmlEvent does not declare.
const event1074XML = `<Event xmlns="http://schemas.microsoft.com/win/2004/08/events/event">
  <System>
    <Provider Name="User32" Guid="{b0aa8734-56f7-41cc-b2f4-de228e98b946}" EventSourceName="User32" />
    <EventID Qualifiers="32768">1074</EventID>
    <Version>0</Version>
    <Level>4</Level>
    <Task>0</Task>
    <Opcode>0</Opcode>
    <Keywords>0x8080000000000000</Keywords>
    <TimeCreated SystemTime="2026-06-16T11:58:30.1234567Z" />
    <EventRecordID>482913</EventRecordID>
    <Correlation />
    <Execution ProcessID="1234" ThreadID="5678" />
    <Channel>System</Channel>
    <Computer>PC</Computer>
    <Security UserID="S-1-5-18" />
  </System>
  <EventData>
    <Data Name="param1">C:\Windows\servicing\TrustedInstaller.exe (PC)</Data>
    <Data Name="param2">PC</Data>
    <Data Name="param3">オペレーティング システム: 回復 (計画済)</Data>
    <Data Name="param4">0x80020002</Data>
    <Data Name="param5">再起動</Data>
    <Data Name="param6"></Data>
    <Data Name="param7">NT AUTHORITY\SYSTEM</Data>
  </EventData>
</Event>`

// withEventData wraps an EventData block in the smallest System block Windows
// still renders, so the EventData-focused tests below only have to state the
// part they are about.
func withEventData(eventData string) string {
	return `<Event xmlns="http://schemas.microsoft.com/win/2004/08/events/event">
  <System>
    <Provider Name="User32" />
    <EventID Qualifiers="32768">1074</EventID>
    <TimeCreated SystemTime="2026-06-16T11:58:30.0000000Z" />
    <EventRecordID>1</EventRecordID>
    <Channel>System</Channel>
    <Computer>PC</Computer>
  </System>
` + eventData + `
</Event>`
}

func TestParseRecord_Event1074(t *testing.T) {
	rec, err := parseRecord(event1074XML)
	if err != nil {
		t.Fatalf("parseRecord returned %v", err)
	}
	if rec.Provider != "User32" {
		t.Errorf("Provider = %q, want User32", rec.Provider)
	}
	if rec.EventID != 1074 {
		t.Errorf("EventID = %d, want 1074", rec.EventID)
	}
	if rec.RecordID != 482913 {
		t.Errorf("RecordID = %d, want 482913", rec.RecordID)
	}
	if rec.Computer != "PC" {
		t.Errorf("Computer = %q, want PC", rec.Computer)
	}
	want := time.Date(2026, 6, 16, 11, 58, 30, 123456700, time.UTC)
	if !rec.Time.Equal(want) {
		t.Errorf("Time = %v, want %v", rec.Time, want)
	}
	wantData := map[string]string{
		"param1": `C:\Windows\servicing\TrustedInstaller.exe (PC)`,
		"param2": "PC",
		"param3": "オペレーティング システム: 回復 (計画済)",
		"param4": "0x80020002",
		"param5": "再起動",
		// An empty element still gets a key: detect distinguishes "present and
		// empty" from "absent" via the comma-ok on this map.
		"param6": "",
		"param7": `NT AUTHORITY\SYSTEM`,
	}
	if !reflect.DeepEqual(rec.Data, wantData) {
		t.Errorf("Data = %#v, want %#v", rec.Data, wantData)
	}
}

// Classic (non-manifest) events such as 1074 are frequently rendered with bare
// <Data> elements, which is why parseRecord synthesises param1, param2, ...
// The number is the element's 1-based position in EventData as a whole, not a
// counter over the unnamed elements only — that is what keeps the synthesised
// keys lined up with the %1..%n insertion strings of the message template, and
// therefore with the param4/param5/param7 keys detect reads.
func TestParseRecord_DataNaming(t *testing.T) {
	tests := []struct {
		name      string
		eventData string
		want      map[string]string
	}{
		{
			name: "all positional, keyed in document order",
			eventData: `  <EventData>
    <Data>C:\Windows\Explorer.EXE (PC)</Data>
    <Data>PC</Data>
    <Data>その他 (計画外)</Data>
    <Data>0x500ff</Data>
    <Data>再起動</Data>
  </EventData>`,
			want: map[string]string{
				"param1": `C:\Windows\Explorer.EXE (PC)`,
				"param2": "PC",
				"param3": "その他 (計画外)",
				"param4": "0x500ff",
				"param5": "再起動",
			},
		},
		{
			// The named elements still consume positions 1 and 3, so the bare
			// one between them is param2 and the trailing one is param4.
			name: "mixed named and positional: the index counts every element",
			eventData: `  <EventData>
    <Data Name="param1">C:\Windows\System32\shutdown.exe (PC)</Data>
    <Data>PC</Data>
    <Data Name="param3">その他 (計画外)</Data>
    <Data>0x0</Data>
  </EventData>`,
			want: map[string]string{
				"param1": `C:\Windows\System32\shutdown.exe (PC)`,
				"param2": "PC",
				"param3": "その他 (計画外)",
				"param4": "0x0",
			},
		},
		{
			// Pinning current behaviour, not endorsing it: an explicit name that
			// collides with a synthesised one silently overwrites, and the later
			// element in document order is the survivor.
			name: "an explicit name colliding with a positional key: last one wins",
			eventData: `  <EventData>
    <Data>first</Data>
    <Data Name="param1">second</Data>
  </EventData>`,
			want: map[string]string{"param1": "second"},
		},
		{
			// EvtRender emits values inline, but pretty-printed XML pads them
			// with the surrounding indentation. parseRecord stores the chardata
			// verbatim; trimming is detect's job.
			name: "chardata whitespace is kept verbatim",
			eventData: `  <EventData>
    <Data Name="param4">
      0x0
    </Data>
  </EventData>`,
			want: map[string]string{"param4": "\n      0x0\n    "},
		},
		{
			// Callers index Data without a nil check, so an event that carries
			// no EventData at all must still come back with a usable map.
			name:      "no EventData at all yields a non-nil empty map",
			eventData: "",
			want:      map[string]string{},
		},
		{
			name:      "an empty EventData element yields a non-nil empty map",
			eventData: "  <EventData></EventData>",
			want:      map[string]string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, err := parseRecord(withEventData(tt.eventData))
			if err != nil {
				t.Fatalf("parseRecord returned %v", err)
			}
			if rec.Data == nil {
				t.Fatal("Data is nil; callers index it without a nil check")
			}
			if !reflect.DeepEqual(rec.Data, tt.want) {
				t.Fatalf("Data = %#v, want %#v", rec.Data, tt.want)
			}
		})
	}
}

// 電源を切る is the shutdown-type value of a 1074 on a Japanese install, and
// mishandling it caused a misclassification in 0.4.1. EvtRender hands us UTF-16
// that we decode ourselves precisely so this survives; assert it byte for byte
// rather than trusting the pipeline.
func TestParseRecord_NonASCIIValuesSurvive(t *testing.T) {
	rec, err := parseRecord(withEventData(`  <EventData>
    <Data>C:\Windows\System32\RuntimeBroker.exe (PC)</Data>
    <Data>PC</Data>
    <Data>その他 (計画外)</Data>
    <Data>0x500ff</Data>
    <Data>電源を切る</Data>
    <Data></Data>
    <Data>PC\User</Data>
  </EventData>`))
	if err != nil {
		t.Fatalf("parseRecord returned %v", err)
	}
	if got := rec.Data["param5"]; got != "電源を切る" {
		t.Errorf("param5 = %q, want 電源を切る", got)
	}
	if got := rec.Data["param3"]; got != "その他 (計画外)" {
		t.Errorf("param3 = %q, want その他 (計画外)", got)
	}
	if got := rec.Data["param7"]; got != `PC\User` {
		t.Errorf(`param7 = %q, want PC\User`, got)
	}
}

// Kernel-Power 41 is the record detect reads BugcheckCode from, and only a
// non-zero code there may become "crash", so the key has to arrive spelled
// exactly as the manifest names it.
func TestParseRecord_KernelPower41(t *testing.T) {
	rec, err := parseRecord(`<Event xmlns="http://schemas.microsoft.com/win/2004/08/events/event">
  <System>
    <Provider Name="Microsoft-Windows-Kernel-Power" Guid="{331c3b3a-2005-44c2-ac5e-77220c37d6b4}" />
    <EventID>41</EventID>
    <Version>8</Version>
    <Level>1</Level>
    <TimeCreated SystemTime="2026-06-16T12:00:05.0000000Z" />
    <EventRecordID>482920</EventRecordID>
    <Channel>System</Channel>
    <Computer>PC</Computer>
    <Security UserID="S-1-5-18" />
  </System>
  <EventData>
    <Data Name="BugcheckCode">159</Data>
    <Data Name="BugcheckParameter1">0x3</Data>
    <Data Name="BugcheckParameter2">0xffff8d0e6b4c1060</Data>
    <Data Name="SleepInProgress">0</Data>
    <Data Name="PowerButtonTimestamp">0</Data>
  </EventData>
</Event>`)
	if err != nil {
		t.Fatalf("parseRecord returned %v", err)
	}
	if rec.Provider != "Microsoft-Windows-Kernel-Power" {
		t.Errorf("Provider = %q, want Microsoft-Windows-Kernel-Power", rec.Provider)
	}
	if rec.EventID != 41 {
		t.Errorf("EventID = %d, want 41", rec.EventID)
	}
	if got := rec.Data["BugcheckCode"]; got != "159" {
		t.Errorf("BugcheckCode = %q, want 159", got)
	}
	if got := rec.Data["SleepInProgress"]; got != "0" {
		t.Errorf("SleepInProgress = %q, want 0", got)
	}
}

// parseRecord discards the time.Parse error on purpose: a record whose
// SystemTime it cannot read is still worth returning, because Provider, EventID
// and the Data map carry the classification signal. The zero time it produces
// instead is what detect's boot-cycle windows then reject. These cases pin that
// choice — a future decision to surface the error must break them deliberately.
func TestParseRecord_TimeCreated(t *testing.T) {
	tests := []struct {
		name        string
		timeElem    string
		wantZero    bool
		wantInstant time.Time
	}{
		{
			name:        "RFC3339Nano with Z, as EvtRender writes it",
			timeElem:    `<TimeCreated SystemTime="2026-06-16T11:58:30.1234567Z" />`,
			wantInstant: time.Date(2026, 6, 16, 11, 58, 30, 123456700, time.UTC),
		},
		{
			// A local-offset stamp has to be converted, not merely accepted:
			// detect compares records against each other, so mixed zones would
			// silently skew every window.
			name:        "an offset timestamp is converted to UTC",
			timeElem:    `<TimeCreated SystemTime="2026-06-16T20:58:30.1234567+09:00" />`,
			wantInstant: time.Date(2026, 6, 16, 11, 58, 30, 123456700, time.UTC),
		},
		{
			name:        "whole seconds",
			timeElem:    `<TimeCreated SystemTime="2026-06-16T11:58:30Z" />`,
			wantInstant: time.Date(2026, 6, 16, 11, 58, 30, 0, time.UTC),
		},
		{
			name:     "an unparseable SystemTime yields the zero time, not an error",
			timeElem: `<TimeCreated SystemTime="16/06/2026 11:58:30" />`,
			wantZero: true,
		},
		{
			name:     "a TimeCreated element without the attribute",
			timeElem: `<TimeCreated />`,
			wantZero: true,
		},
		{
			name:     "no TimeCreated element at all",
			timeElem: "",
			wantZero: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, err := parseRecord(`<Event xmlns="http://schemas.microsoft.com/win/2004/08/events/event">
  <System>
    <Provider Name="User32" />
    <EventID Qualifiers="32768">1074</EventID>
    ` + tt.timeElem + `
    <EventRecordID>7</EventRecordID>
    <Computer>PC</Computer>
  </System>
</Event>`)
			if err != nil {
				t.Fatalf("parseRecord returned %v", err)
			}
			// Location, not just the instant: two times can compare equal while
			// formatting differently, and the Record contract says UTC.
			if loc := rec.Time.Location(); loc != time.UTC {
				t.Errorf("Time location = %v, want UTC", loc)
			}
			if tt.wantZero {
				if !rec.Time.IsZero() {
					t.Errorf("Time = %v, want the zero time", rec.Time)
				}
				// The rest of the record must survive the discarded error.
				if rec.EventID != 1074 || rec.RecordID != 7 {
					t.Errorf("EventID/RecordID = %d/%d, want 1074/7", rec.EventID, rec.RecordID)
				}
				return
			}
			if !rec.Time.Equal(tt.wantInstant) {
				t.Errorf("Time = %v, want %v", rec.Time, tt.wantInstant)
			}
		})
	}
}

// Malformed input is the one thing parseRecord does report, and it must report
// it rather than hand back a half-filled Record that detect would classify.
func TestParseRecord_MalformedXML(t *testing.T) {
	tests := []struct {
		name string
		xml  string
	}{
		{"unclosed element", `<Event><System><EventID>1074</EventID></Event>`},
		{"not XML at all", "EvtRender returned nothing useful"},
		{"empty string", ""},
		{"truncated mid-tag", `<Event><System><Provider Name="User3`},
		{"a non-numeric EventID cannot fill the int field", `<Event><System><EventID>oops</EventID></System></Event>`},
		{"a non-numeric EventRecordID cannot fill the int64 field", `<Event><System><EventRecordID>x</EventRecordID></System></Event>`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, err := parseRecord(tt.xml)
			if err == nil {
				t.Fatalf("parseRecord(%q) returned no error, got %#v", tt.xml, rec)
			}
			// The zero Record is returned alongside the error, so a caller that
			// ignores err at least gets nothing that looks like a real event.
			if !reflect.DeepEqual(rec, Record{}) {
				t.Errorf("record on error = %#v, want the zero Record", rec)
			}
		})
	}
}
