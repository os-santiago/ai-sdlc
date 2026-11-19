package admission

import (
	"os"
	"strings"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/normalizedevent"
)

func issueFromBody(t *testing.T, body string) Issue {
	t.Helper()
	return Issue{Repository: "os-santiago/saferus", Number: 4, Revision: "r1",
		Title: "Show token lifetime in readable units", Body: body,
		AcceptanceCriteria: normalizedevent.ExtractAcceptanceCriteria(body),
		RequestedBy:        "author", AuthorizedBy: "maintainer"}
}

// The issue body that reached an independent request_changes in the live
// installation: it mixes delivery obligations, baseline comparisons and an
// operational section with reviewable software criteria.
func TestUnreviewableIssueIsNotAdmittedAndGetsAReviewableProposal(t *testing.T) {
	raw, err := os.ReadFile("testdata/saferus-issue-4.md")
	if err != nil {
		t.Fatal(err)
	}
	issue := issueFromBody(t, string(raw))
	got := Evaluate(issue, DefaultPolicy())
	if got.Decision != NeedsClarification || got.Risk != "low" {
		t.Fatalf("unreviewable issue admitted: %#v", got)
	}
	findings := ReviewabilityFindings(issue)
	kinds := map[FindingKind]int{}
	for _, f := range findings {
		kinds[f.Kind]++
	}
	if kinds[DeliveryObligation] != 1 || kinds[BaselineComparison] != 2 || kinds[OperationalSection] != 1 {
		t.Fatalf("unexpected findings %#v", findings)
	}

	proposal, unresolved := ProposeRevision(issue)
	if len(unresolved) != 0 {
		t.Fatalf("unresolved findings %#v", unresolved)
	}
	revised := issueFromBody(t, proposal)
	if again := Evaluate(revised, DefaultPolicy()); again.Decision != Accepted {
		t.Fatalf("proposal not admissible: %#v\n%s", again, proposal)
	}
	if len(ReviewabilityFindings(revised)) != 0 {
		t.Fatal("proposal still unreviewable")
	}
	// The software request, recipe and every reviewable criterion survive.
	for _, keep := range []string{
		"Improve the demo's token-lifetime presentation",
		"AI-SDLC validation recipe: saferus-token-lifetime/v1",
		"Format nonnegative safe integer seconds",
		"Cover `1 second`",
		"Preserve `Unavailable`",
		"Update the browser expectation for `#ttl` from `60 seconds` to `1 minute`.",
		"Limit the change to `web/static/ttl.js`, `tests/ttl.test.mjs`, and that single presentation assertion in `tools/verify_browser.py`.",
	} {
		if !strings.Contains(proposal, keep) {
			t.Fatalf("proposal lost %q:\n%s", keep, proposal)
		}
	}
	for _, gone := range []string{"Delivery readiness", "byte-for-byte", "every other file", "provenance", "approvals"} {
		if strings.Contains(proposal, gone) {
			t.Fatalf("proposal kept %q:\n%s", gone, proposal)
		}
	}
	if len(revised.AcceptanceCriteria) != len(issue.AcceptanceCriteria)-1 {
		t.Fatalf("criteria %d -> %d", len(issue.AcceptanceCriteria), len(revised.AcceptanceCriteria))
	}
}

func TestReviewableIssueIsUnchanged(t *testing.T) {
	issue := validIssue()
	if findings := ReviewabilityFindings(issue); len(findings) != 0 {
		t.Fatalf("false positive %#v", findings)
	}
	if body, unresolved := ProposeRevision(issue); body != issue.Body || unresolved != nil {
		t.Fatal("reviewable issue rewritten")
	}
	if got := Evaluate(issue, DefaultPolicy()); got.Decision != Accepted {
		t.Fatalf("reviewable issue not admitted %#v", got)
	}
}

func TestBaselineOnlyCriterionIsReportedNotRewritten(t *testing.T) {
	body := "Refactor the parser for clarity while keeping its public behavior identical for callers.\n\n## Acceptance criteria\n\n- [ ] All other files byte-for-byte\n- [ ] Parser tests cover empty input\n"
	issue := issueFromBody(t, body)
	got := Evaluate(issue, DefaultPolicy())
	if got.Decision != NeedsClarification {
		t.Fatalf("got %#v", got)
	}
	proposal, unresolved := ProposeRevision(issue)
	if len(unresolved) != 1 || unresolved[0].Criterion != 1 || !strings.Contains(proposal, "All other files byte-for-byte") {
		t.Fatalf("baseline-only criterion silently rewritten: %#v\n%s", unresolved, proposal)
	}
}

func TestReviewabilityKeepsStrongerDecisions(t *testing.T) {
	issue := validIssue()
	issue.Body += "\n\n## Acceptance criteria\n\n- [ ] Deploy to production after independent review\n"
	issue.AcceptanceCriteria = append(issue.AcceptanceCriteria, "Deploy to production after independent review")
	issue.AuthorizedBy = ""
	got := Evaluate(issue, DefaultPolicy())
	if got.Decision != NeedsHuman {
		t.Fatalf("reviewability overrode a stronger decision: %#v", got)
	}
	if len(got.Reasons) < 2 {
		t.Fatalf("missing reviewability reason %#v", got)
	}
}

// Review feedback: product features that mention deployment or expiry are
// reviewable, headings without a space are recognized, later reviewable
// clauses survive, and fragments are not declared resolved.
func TestReviewabilityAvoidsFalsePositivesAndLossyRewrites(t *testing.T) {
	product := "Show release information to operators so they can see what is live right now.\n\n## Problem\n\nDisplay the token expiry in minutes.\n\nAI-SDLC validation recipe: example/v1\n\n## Acceptance criteria\n\n- [ ] Display deployment status beside each release.\n- [ ] Display the token expiry in minutes.\n"
	issue := issueFromBody(t, product)
	if findings := ReviewabilityFindings(issue); len(findings) != 0 {
		t.Fatalf("product properties flagged %#v", findings)
	}
	if got := Evaluate(issue, DefaultPolicy()); got.Decision != Accepted {
		t.Fatalf("reviewable product issue blocked %#v", got)
	}

	noSpace := "Improve the status page so a person can read the current state quickly and clearly.\n\n##Acceptance criteria\n\n- [ ] Obtain independent review and deploy to the production environment.\n- [ ] Show the state label.\n"
	issue = issueFromBody(t, noSpace)
	proposal, unresolved := ProposeRevision(issue)
	if len(unresolved) != 0 || strings.Contains(proposal, "independent review") || !strings.Contains(proposal, "Show the state label.") {
		t.Fatalf("heading without space not handled: %#v\n%s", unresolved, proposal)
	}

	later := "Improve the empty list experience so people understand there is nothing to show yet.\n\n## Acceptance criteria\n\n- [ ] Render an empty state; all other files remain unchanged; show an error for invalid input.\n"
	issue = issueFromBody(t, later)
	proposal, unresolved = ProposeRevision(issue)
	if len(unresolved) != 0 || !strings.Contains(proposal, "Render an empty state; show an error for invalid input.") {
		t.Fatalf("later reviewable clause lost: %#v\n%s", unresolved, proposal)
	}

	fragment := "Improve the input handling so every case is treated consistently by the parser module.\n\n## Acceptance criteria\n\n- [ ] For each input; all other files remain unchanged.\n"
	issue = issueFromBody(t, fragment)
	proposal, unresolved = ProposeRevision(issue)
	if len(unresolved) != 1 || !strings.Contains(proposal, "For each input; all other files remain unchanged.") {
		t.Fatalf("fragment declared resolved: %#v\n%s", unresolved, proposal)
	}
	if got := Evaluate(issue, DefaultPolicy()); got.Decision != NeedsClarification {
		t.Fatalf("unresolved baseline criterion admitted %#v", got)
	}
}
