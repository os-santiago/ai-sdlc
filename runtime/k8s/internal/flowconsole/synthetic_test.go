package flowconsole

import (
	"testing"
	"time"
)

func TestSyntheticLoadIsValidDeterministicAndLarge(t *testing.T) {
	end := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	events := Synthetic(500, end)
	if len(events) < 15000 {
		t.Fatal("synthetic load too small", len(events))
	}
	seen := map[string]bool{}
	kinds := map[string]bool{}
	issues := map[int]bool{}
	for _, e := range events {
		if err := e.Validate(); err != nil {
			t.Fatal(err)
		}
		if seen[e.ID] {
			t.Fatal("duplicate id")
		}
		if e.Time.After(end) {
			t.Fatal("event after end")
		}
		seen[e.ID], kinds[e.Kind], issues[e.Issue] = true, true, true
	}
	if len(issues) != 500 || len(kinds) < 9 {
		t.Fatal("coverage", len(issues), len(kinds))
	}
	again := Synthetic(500, end)
	if len(again) != len(events) || again[len(again)-1].ID != events[len(events)-1].ID {
		t.Fatal("not deterministic")
	}
}
