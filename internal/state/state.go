// Package state persists which boot was last notified so each restart is
// reported exactly once across scheduled-task runs.
package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// State is the small persisted record of the last notification.
type State struct {
	LastBootTime time.Time `json:"last_boot_time"`
	LastRecordID int64     `json:"last_record_id"`
	LastNotified time.Time `json:"last_notified"`
}

// Load reads state from path. A missing or corrupt file yields an empty State
// (so the next boot is treated as new) rather than an error.
func Load(path string) *State {
	b, err := os.ReadFile(path)
	if err != nil {
		return &State{}
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return &State{}
	}
	return &s
}

// Save writes state atomically (temp file + rename).
func Save(path string, s *State) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// AlreadyNotified reports whether the given boot (record id + boot time) matches
// what was last notified.
func (s *State) AlreadyNotified(recordID int64, bootTime time.Time) bool {
	return s.LastRecordID == recordID && s.LastBootTime.Equal(bootTime)
}
