//go:build windows

package main

import (
	"testing"

	"github.com/223n/restart-message/internal/config"
	"github.com/223n/restart-message/internal/detect"
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
