package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var (
	// A boot time with a non-zero sub-second part, so a round trip that silently
	// truncated the timestamp would change the value AlreadyNotified compares.
	bootA = time.Date(2026, 6, 16, 12, 0, 0, 123456789, time.UTC)
	bootB = time.Date(2026, 6, 17, 8, 30, 0, 0, time.UTC)

	// The offset Windows reports for a Japanese machine; used to prove the
	// comparison is instant-based, not representation-based.
	jst = time.FixedZone("JST", 9*60*60)
)

// statePath returns a path inside a fresh temp dir. Nested so callers can also
// exercise the parent-directory creation in Save.
func statePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "state.json")
}

// A missing state file is the normal first-run condition and must read as
// "nothing was ever notified", never as an error: erroring here would abort the
// run command and the very first boot would go unreported.
func TestLoad_MissingFileIsEmptyState(t *testing.T) {
	s := Load(filepath.Join(t.TempDir(), "does-not-exist", "state.json"))
	if s == nil {
		t.Fatal("Load returned nil; callers dereference the result unconditionally")
	}
	if *s != (State{}) {
		t.Fatalf("state = %+v, want zero value", *s)
	}
	if s.AlreadyNotified(100, bootA) {
		t.Fatal("a missing state file must not report a real boot as already notified")
	}
}

// Same contract for a damaged file: half-written or garbage state must degrade
// to "never notified" (one duplicate notification) rather than to a hard error
// that silences the tool until someone deletes the file by hand.
func TestLoad_CorruptFileIsEmptyState(t *testing.T) {
	cases := map[string]string{
		"empty file":      "",
		"truncated json":  `{"last_boot_time":"2026-06-16T12:00:00Z","last_rec`,
		"not json at all": "not json\n",
		"wrong shape":     `["last_boot_time"]`,
		// Field order matters here. encoding/json stops decoding an object at the
		// first field whose UnmarshalJSON fails, so with the bad timestamp first
		// the struct is left zero whatever Load does with the error. Putting good
		// fields ahead of it is the only way to tell "Load discards the partial
		// result" apart from "Load returns whatever was half-decoded" — and a
		// half-decoded record (a plausible LastRecordID next to a zero boot time)
		// is exactly what a torn state.json would hand back to AlreadyNotified.
		"unparsable time, bad field first": `{"last_boot_time":"yesterday","last_record_id":100}`,
		"unparsable time, id decoded":      `{"last_record_id":100,"last_boot_time":"yesterday"}`,
		"unparsable time, two fields decoded": `{"last_record_id":100,` +
			`"last_notified":"2026-06-16T12:00:00Z","last_boot_time":"yesterday"}`,
		"nul-padded record": "\x00\x00\x00\x00",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := statePath(t)
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatalf("seed file: %v", err)
			}
			s := Load(path)
			if *s != (State{}) {
				t.Fatalf("state = %+v, want zero value", *s)
			}
			if s.AlreadyNotified(100, bootA) {
				t.Fatal("a corrupt state file must not suppress the next notification")
			}
		})
	}
}

// The on-disk JSON keys are a compatibility surface: every existing install has
// a state.json written by an older build. Renaming a struct tag would make those
// files parse into a zero State and re-notify a boot that was already reported,
// so the literal keys are pinned here rather than round-tripped through Save.
func TestLoad_JSONKeysArePinned(t *testing.T) {
	path := statePath(t)
	const content = `{
  "last_boot_time": "2026-06-16T12:00:00.123456789Z",
  "last_record_id": 4242,
  "last_notified": "2026-06-16T12:01:00Z"
}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	s := Load(path)
	if !s.LastBootTime.Equal(bootA) {
		t.Fatalf("LastBootTime = %v, want %v", s.LastBootTime, bootA)
	}
	if s.LastRecordID != 4242 {
		t.Fatalf("LastRecordID = %d, want 4242", s.LastRecordID)
	}
	if want := time.Date(2026, 6, 16, 12, 1, 0, 0, time.UTC); !s.LastNotified.Equal(want) {
		t.Fatalf("LastNotified = %v, want %v", s.LastNotified, want)
	}
	if !s.AlreadyNotified(4242, bootA) {
		t.Fatal("a state file from an earlier build must still suppress its own boot")
	}
}

// Timestamps must survive Save+Load unchanged down to the nanosecond and keep
// their instant across zones: AlreadyNotified compares the boot time, so any
// drift introduced by the encoding would re-notify every boot.
func TestSaveLoad_RoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   State
	}{
		{
			"utc with nanoseconds",
			State{LastBootTime: bootA, LastRecordID: 4242, LastNotified: bootA.Add(time.Minute)},
		},
		{
			"fixed +09:00 offset",
			State{LastBootTime: bootA.In(jst), LastRecordID: 7, LastNotified: bootB.In(jst)},
		},
		{
			"zero value",
			State{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := statePath(t)
			if err := Save(path, &tc.in); err != nil {
				t.Fatalf("Save: %v", err)
			}
			got := Load(path)
			if !got.LastBootTime.Equal(tc.in.LastBootTime) {
				t.Fatalf("LastBootTime = %v, want %v", got.LastBootTime, tc.in.LastBootTime)
			}
			if !got.LastNotified.Equal(tc.in.LastNotified) {
				t.Fatalf("LastNotified = %v, want %v", got.LastNotified, tc.in.LastNotified)
			}
			if got.LastRecordID != tc.in.LastRecordID {
				t.Fatalf("LastRecordID = %d, want %d", got.LastRecordID, tc.in.LastRecordID)
			}
			// Only asserted for a state that recorded a real boot. A zero state
			// also "matches" the zero boot, but that falls out of comparing two
			// zero values; it is not a promise this package makes, and pinning it
			// would forbid ever hardening AlreadyNotified against it.
			if tc.in != (State{}) && !got.AlreadyNotified(tc.in.LastRecordID, tc.in.LastBootTime) {
				t.Fatal("the reloaded state no longer matches the boot it recorded")
			}
		})
	}
}

// The state file lives under %ProgramData%\restart-message\, which does not
// exist before the first notification, so Save has to create it.
func TestSave_CreatesParentDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restart-message", "nested", "state.json")
	if err := Save(path, &State{LastBootTime: bootA, LastRecordID: 1}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("state file not written: %v", err)
	}
	if !Load(path).AlreadyNotified(1, bootA) {
		t.Fatal("state written into a freshly created directory did not read back")
	}
}

// Every notification overwrites the previous record; leftovers from the longer
// old document would make the file invalid JSON and lose the suppression.
func TestSave_OverwritesExistingFile(t *testing.T) {
	path := statePath(t)
	first := State{LastBootTime: bootA, LastRecordID: 123456789, LastNotified: bootA}
	if err := Save(path, &first); err != nil {
		t.Fatalf("Save first: %v", err)
	}
	second := State{LastBootTime: bootB, LastRecordID: 7, LastNotified: bootB}
	if err := Save(path, &second); err != nil {
		t.Fatalf("Save second: %v", err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var decoded State
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("file is not valid JSON after overwrite: %v (%q)", err, b)
	}
	if decoded.LastRecordID != 7 || !decoded.LastBootTime.Equal(bootB) {
		t.Fatalf("state = %+v, want the second save", decoded)
	}
	if Load(path).AlreadyNotified(123456789, bootA) {
		t.Fatal("the superseded boot is still reported as notified")
	}
}

// The temp file + rename is what keeps the live state.json intact when a save
// cannot complete — the realistic case here, since the service saves while the
// machine is already shutting down. Blocking the temp path (a directory sitting
// where the temp file goes) makes the write fail after the destination would
// otherwise have been truncated, so a plain in-place write fails this test.
func TestSave_FailedWriteKeepsPreviousState(t *testing.T) {
	path := statePath(t)
	if err := Save(path, &State{LastBootTime: bootA, LastRecordID: 100}); err != nil {
		t.Fatalf("Save first: %v", err)
	}
	// EISDIR on Linux, ERROR_ACCESS_DENIED on Windows; an error either way.
	if err := os.Mkdir(path+".tmp", 0o755); err != nil {
		t.Fatalf("block temp path: %v", err)
	}

	if err := Save(path, &State{LastBootTime: bootB, LastRecordID: 101}); err == nil {
		t.Fatal("Save = nil, want an error when the temp file cannot be written")
	}
	if !Load(path).AlreadyNotified(100, bootA) {
		t.Fatal("a failed Save destroyed the previously recorded boot")
	}
}

// A successful save must leave nothing behind: the state directory is also where
// the service log lives and a stale .tmp would be mistaken for a half-finished
// write. (That the temp file is on the write path at all is pinned above.)
func TestSave_LeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	for i := 0; i < 2; i++ { // second pass covers the overwrite path too
		if err := Save(path, &State{LastBootTime: bootA, LastRecordID: int64(i)}); err != nil {
			t.Fatalf("Save #%d: %v", i, err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 1 || names[0] != "state.json" {
		t.Fatalf("directory contains %v, want only [state.json]", names)
	}
}

// The suppression key is the pair (record id, boot time). Matching on only one
// half would either re-notify a boot already reported or, worse, swallow a real
// restart whose record id happened to repeat after a log wrap.
func TestAlreadyNotified(t *testing.T) {
	saved := State{LastBootTime: bootA, LastRecordID: 4242, LastNotified: bootA.Add(time.Minute)}
	cases := []struct {
		name     string
		recordID int64
		boot     time.Time
		want     bool
	}{
		{"same record id and boot time", 4242, bootA, true},
		{"different record id", 4243, bootA, false},
		{"different boot time", 4242, bootB, false},
		{"boot time off by a nanosecond", 4242, bootA.Add(time.Nanosecond), false},
		{"both differ", 1, bootB, false},
		{"nothing recorded yet", 0, time.Time{}, false},
		// Load rebuilds the boot time from RFC3339 text, so it can come back in a
		// different location than the one detect produced. time.Time == compares
		// the wall clock, monotonic reading AND the *Location pointer, so a
		// "simplified" == here would report every boot as new forever, notifying
		// on each scheduled-task run. Equal compares the instant.
		{"same instant in +09:00", 4242, bootA.In(jst), true},
		{"same instant in a zero-offset zone that is not UTC", 4242, bootA.In(time.FixedZone("GMT", 0)), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := saved.AlreadyNotified(tc.recordID, tc.boot); got != tc.want {
				t.Fatalf("AlreadyNotified(%d, %v) = %v, want %v", tc.recordID, tc.boot, got, tc.want)
			}
		})
	}
}

// End-to-end over the sequence the run command performs: first boot (no file),
// a second scheduled run inside the same boot, then a genuinely new boot. This
// is the whole point of the package — exactly one notification per boot.
func TestSequence_TwoBoots(t *testing.T) {
	path := statePath(t)

	// First run after boot A: nothing on disk, so the boot is reported.
	if st := Load(path); st.AlreadyNotified(100, bootA) {
		t.Fatal("boot A suppressed before anything was saved")
	}
	if err := Save(path, &State{LastBootTime: bootA, LastRecordID: 100, LastNotified: bootA.Add(time.Minute)}); err != nil {
		t.Fatalf("Save after boot A: %v", err)
	}

	// A second run inside the same boot (a retry, or a manual `run`) must stay
	// silent — this is the duplicate the state file exists to prevent.
	if !Load(path).AlreadyNotified(100, bootA) {
		t.Fatal("boot A not suppressed on the second run")
	}

	// Boot B: different record id and boot time, so it is reported and recorded.
	st := Load(path)
	if st.AlreadyNotified(101, bootB) {
		t.Fatal("boot B suppressed by boot A's state")
	}
	if err := Save(path, &State{LastBootTime: bootB, LastRecordID: 101, LastNotified: bootB.Add(time.Minute)}); err != nil {
		t.Fatalf("Save after boot B: %v", err)
	}

	final := Load(path)
	if !final.AlreadyNotified(101, bootB) {
		t.Fatal("boot B not suppressed after being saved")
	}
	if final.AlreadyNotified(100, bootA) {
		t.Fatal("boot A still suppressed after boot B overwrote the state")
	}
}

// A save that cannot happen has to surface as an error: the run command reports
// it, and the next run then re-notifies. Silently swallowing it would leave the
// caller believing the boot was recorded when nothing reached the disk.
func TestSave_ReportsUnusableDirectory(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "restart-message")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}

	// The parent of the state file is an existing regular file, so MkdirAll
	// cannot create the directory (ENOTDIR on both Linux and Windows).
	if err := Save(filepath.Join(blocker, "state.json"), &State{LastBootTime: bootA, LastRecordID: 1}); err == nil {
		t.Fatal("Save = nil, want an error when the parent directory cannot be created")
	}
}
