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
	if kinds[DeliveryObligation] != 1 || kinds[BaselineComparison] != 1 || kinds[ScopeLimit] != 1 || kinds[OperationalSection] != 1 {
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
	} {
		if !strings.Contains(proposal, keep) {
			t.Fatalf("proposal lost %q:\n%s", keep, proposal)
		}
	}
	for _, gone := range []string{"Delivery readiness", "byte-for-byte", "every other file", "provenance", "approvals", "Limit the change"} {
		if strings.Contains(proposal, gone) {
			t.Fatalf("proposal kept %q:\n%s", gone, proposal)
		}
	}
	if len(revised.AcceptanceCriteria) != len(issue.AcceptanceCriteria)-2 {
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

// A change-scope restriction needs the original source to verify, so the
// read-only reviewer can only request changes; admission proposes removing it.
func TestScopeLimitCriteriaAreNotAdmittedAndRemovedFromTheProposal(t *testing.T) {
	for _, criterion := range []string{
		"Limit the change to `web/static/ttl.js`, `tests/ttl.test.mjs`, and that single presentation assertion in `tools/verify_browser.py`.",
		"Restrict the changes to `internal/parser`.",
		"Only modify files under web/static.",
		"Change only `web/static/ttl.js`.",
		"Edits must be confined to `web/static`.",
		"Keep the change scoped to the `web` directory.",
		"Only touch client.go.",
	} {
		body := "Improve the token lifetime display so people read durations immediately in the console.\n\n## Acceptance criteria\n\n- [ ] Show `1 minute` for 60 seconds.\n- [ ] " + criterion + "\n"
		issue := issueFromBody(t, body)
		if got := Evaluate(issue, DefaultPolicy()); got.Decision != NeedsClarification {
			t.Fatalf("scope limit admitted %q: %#v", criterion, got)
		}
		proposal, unresolved := ProposeRevision(issue)
		revised := issueFromBody(t, proposal)
		if len(unresolved) != 0 || strings.Contains(proposal, criterion) || Evaluate(revised, DefaultPolicy()).Decision != Accepted ||
			len(revised.AcceptanceCriteria) != 1 {
			t.Fatalf("scope limit not removed %q: %#v\n%s", criterion, unresolved, proposal)
		}
	}
	for _, reviewable := range []string{"Limit the label to 20 characters.", "Display only the remaining minutes when hours are zero.",
		"Users can only edit their own comments.", "Admins can only change roles from the settings page.",
		"Only edit mode is allowed for admins.", "Restrict edits to the document to its owner.",
		"Limit changes to the password to once per day.", "Limit modifications to admin users.",
		"Confine diffs to 500 lines in the viewer.", "Restrict uploads to 5 MB.",
		"Restrict the edits to the document to its owner.", "Limit the changes to the password to once per day.",
		"Restrict your changes to settings to admins.", "Scope the changes to the tenant to the current organization.",
		"Changes must be limited to 5 per minute per user.", "Edits should be restricted to the owner.",
		"Upload rejects files other than PNG and JPEG.", "The CLI accepts paths other than the working directory.",
		"The export contains no other files than report.csv.", "The diff viewer only highlights changed lines.",
		"Only touch the config.yaml when migration is enabled.", "Only change the file permissions when the user is an owner.",
		"Only edit the file name in the rename dialog."} {
		issue := issueFromBody(t, "Improve the token lifetime display so people read durations immediately in the console.\n\n## Acceptance criteria\n\n- [ ] "+reviewable+"\n")
		if findings := ReviewabilityFindings(issue); len(findings) != 0 {
			t.Fatalf("product property flagged %q: %#v", reviewable, findings)
		}
	}
}

// Mixed criteria keep their behavior: only the scope (and baseline) clauses are
// cut, and a scope clause that states behavior is left for the author.
func TestMixedScopeCriteriaKeepBehaviorOrStayUnresolved(t *testing.T) {
	head := "Improve the token lifetime display so people read durations immediately in the console.\n\n## Acceptance criteria\n\n- [ ] "
	issue := issueFromBody(t, head+"Update `#ttl` to `1 minute`, all other lines otherwise unchanged; only edit `verify_browser.py`.\n")
	proposal, unresolved := ProposeRevision(issue)
	if len(unresolved) != 0 || !strings.Contains(proposal, "- [ ] Update `#ttl` to `1 minute`.") || strings.Contains(proposal, "only edit") {
		t.Fatalf("mixed criterion lost behavior: %#v\n%s", unresolved, proposal)
	}
	issue = issueFromBody(t, head+"Only modify `ttl.js` so that 60 seconds renders as `1 minute`.\n")
	proposal, unresolved = ProposeRevision(issue)
	if len(unresolved) != 1 || !strings.Contains(proposal, "so that 60 seconds renders") {
		t.Fatalf("behavioral scope clause silently dropped: %#v\n%s", unresolved, proposal)
	}
}
