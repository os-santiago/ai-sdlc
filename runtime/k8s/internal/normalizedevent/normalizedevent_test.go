package normalizedevent

import "testing"

const sampleWorkItemJSON = `{
  "version": "1.0.0",
  "entity": {
    "kind": "work_item",
    "id": "revision-42",
    "number": 1573,
    "title": "fix: improve community search performance",
    "description": "## Problem\nSearch is slow.\n\n## Acceptance criteria\n- [ ] Search returns the same results\n- [ ] Latency is covered by a regression test\n\n## Context\nSee files.",
    "state": "open",
    "labels": ["ready-to-implement", "priority:P2"]
  },
  "transition": "labeled",
  "actor": {"id": "12345", "type": "user", "login": "maintainer"},
  "repository": {"id": "999", "name": "os-santiago/homedir"}
}`

func TestDecodeAcceptsWorkItem(t *testing.T) {
	event, err := Decode([]byte(sampleWorkItemJSON))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if event.Entity.Title != "fix: improve community search performance" {
		t.Fatalf("unexpected title: %q", event.Entity.Title)
	}
}

func TestDecodeRejectsNonWorkItem(t *testing.T) {
	_, err := Decode([]byte(`{"version":"1.0.0","entity":{"kind":"change_request","id":"1","title":"t","state":"open"},"transition":"opened","actor":{"id":"1","type":"bot"}}`))
	if err == nil {
		t.Fatal("expected error for non work_item entity kind")
	}
}

func TestExtractAcceptanceCriteriaFindsChecklist(t *testing.T) {
	body := "## Problem\nSearch is slow.\n\n## Acceptance criteria\n- [ ] Search returns the same results\n- [ ] Latency is covered by a regression test\n\n## Context\nSee files."
	got := ExtractAcceptanceCriteria(body)
	want := []string{"Search returns the same results", "Latency is covered by a regression test"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestExtractAcceptanceCriteriaMissingSectionReturnsNil(t *testing.T) {
	got := ExtractAcceptanceCriteria("## Problem\nNo AC section here.")
	if got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
}

func TestToIssueViewProjectsFields(t *testing.T) {
	event, err := Decode([]byte(sampleWorkItemJSON))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	view, err := ToIssueView(event, "maintainer")
	if err != nil {
		t.Fatalf("ToIssueView: %v", err)
	}
	if view.Repository != "os-santiago/homedir" || view.Number != 1573 {
		t.Fatalf("got %+v", view)
	}
	if len(view.AcceptanceCriteria) != 2 {
		t.Fatalf("acceptance criteria = %v", view.AcceptanceCriteria)
	}
	if view.AuthorizedBy != "maintainer" {
		t.Fatalf("authorizedBy = %q", view.AuthorizedBy)
	}
}

func TestToIssueViewRejectsMissingRepository(t *testing.T) {
	event := Event{Entity: Entity{Kind: "work_item", Number: intPtr(1)}}
	if _, err := ToIssueView(event, ""); err == nil {
		t.Fatal("expected error for missing repository")
	}
}

func intPtr(v int) *int { return &v }
