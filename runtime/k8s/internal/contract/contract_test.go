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
	if got.Triggers.MinBodyLength != 120 {
		t.Fatalf("triggers.min_body_length default = %d, want 120", got.Triggers.MinBodyLength)
	}
	if got.Triggers.AcceptanceCriteria.Min != 1 {
		t.Fatalf("triggers.acceptance_criteria.min default = %d, want 1", got.Triggers.AcceptanceCriteria.Min)
	}
	if got.Triggers.AcceptanceCriteria.Max != nil {
		t.Fatalf("triggers.acceptance_criteria.max default should be nil (unbounded), got %v", *got.Triggers.AcceptanceCriteria.Max)
	}
	if got.MaxAcceptanceCriteriaOrUnbounded() != 0 {
		t.Fatalf("MaxAcceptanceCriteriaOrUnbounded() = %d, want 0", got.MaxAcceptanceCriteriaOrUnbounded())
	}
	if got.Review.MaxChangedFiles != nil {
		t.Fatalf("review.max_changed_files default should be nil (unbounded), got %v", *got.Review.MaxChangedFiles)
	}
	if got.MaxChangedFilesOrUnbounded() != 0 {
		t.Fatalf("MaxChangedFilesOrUnbounded() = %d, want 0", got.MaxChangedFilesOrUnbounded())
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

func TestLoadOverridesNestedPartialAcceptanceCriteria(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".ai-sdlc.yaml")
	// Only sets acceptance_criteria.max — min must still come from the
	// schema default (1), confirming the nested-struct merge behaves like
	// the flat-field merge (absent keys keep Default()'s value, not zero).
	contents := []byte(`
spec: "1.0.0"
triggers:
  acceptance_criteria:
    max: 8
review:
  max_changed_files: 5
`)
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Triggers.AcceptanceCriteria.Min != 1 {
		t.Fatalf("acceptance_criteria.min should keep default 1 when only max is set, got %d", got.Triggers.AcceptanceCriteria.Min)
	}
	if got.MaxAcceptanceCriteriaOrUnbounded() != 8 {
		t.Fatalf("MaxAcceptanceCriteriaOrUnbounded() = %d, want 8", got.MaxAcceptanceCriteriaOrUnbounded())
	}
	if got.MaxChangedFilesOrUnbounded() != 5 {
		t.Fatalf("MaxChangedFilesOrUnbounded() = %d, want 5", got.MaxChangedFilesOrUnbounded())
	}
	if got.Triggers.MinBodyLength != 120 {
		t.Fatalf("min_body_length should keep default 120 when unset, got %d", got.Triggers.MinBodyLength)
	}
}

func TestLoadMissingFileFails(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Fatal("expected error for missing contract file")
	}
}
