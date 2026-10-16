package contract

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultMatchesSchemaDocumentedDefaults(t *testing.T) {
	got := Default()
	if got.Autonomy.Level != "suggest" {
		t.Fatalf("autonomy.level default = %q, want suggest", got.Autonomy.Level)
	}
	if got.Triggers.IssueLabel != "ready-to-implement" {
		t.Fatalf("triggers.issue_label default = %q", got.Triggers.IssueLabel)
	}
	if got.MaxRepairsOrDefault() != 3 {
		t.Fatalf("merge.max_repairs default = %d, want 3", got.MaxRepairsOrDefault())
	}
	if got.MaxFixIterationsOrDefault() != 3 {
		t.Fatalf("review.max_fix_iterations default = %d, want 3", got.MaxFixIterationsOrDefault())
	}
	if !got.CriticalRequiresAcceptanceOrDefault() {
		t.Fatal("review.critical_requires_acceptance default should be true")
	}
	if got.Engine.MaxSteps != 400 || got.Engine.MaxSeconds != 1500 {
		t.Fatalf("engine defaults = %+v", got.Engine)
	}
}

func TestLoadOverridesDefaultsFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".ai-sdlc.yaml")
	contents := []byte(`
spec: "1.0.0"
autonomy:
  level: auto-merge-low
merge:
  max_repairs: 5
review:
  max_fix_iterations: 2
  critical_requires_acceptance: false
triggers:
  issue_label: ready-to-implement
  skip_labels: [epic, wontfix, needs-human]
`)
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Autonomy.Level != "auto-merge-low" {
		t.Fatalf("autonomy.level = %q", got.Autonomy.Level)
	}
	if got.MaxRepairsOrDefault() != 5 {
		t.Fatalf("merge.max_repairs = %d, want 5", got.MaxRepairsOrDefault())
	}
	if got.MaxFixIterationsOrDefault() != 2 {
		t.Fatalf("review.max_fix_iterations = %d, want 2", got.MaxFixIterationsOrDefault())
	}
	if got.CriticalRequiresAcceptanceOrDefault() {
		t.Fatal("review.critical_requires_acceptance should be false after override")
	}
	if len(got.Triggers.SkipLabels) != 3 {
		t.Fatalf("triggers.skip_labels = %v", got.Triggers.SkipLabels)
	}
}

func TestLoadMissingFileFails(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Fatal("expected error for missing contract file")
	}
}
