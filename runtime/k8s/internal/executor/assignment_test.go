package executor

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

func profileFixture() Profile {
	return Profile{ProfileVersion, "fixture-model", 8, 60, 2048, strings.Repeat("a", 64), 0}
}
func assignmentFixture(now time.Time) Assignment {
	return Assignment{Version: AssignmentVersion, WorkItemID: "11111111-1111-4111-8111-111111111111", Attempt: 1, Repository: "fixture/product", SourceSHA: strings.Repeat("b", 40), TreeSHA256: strings.Repeat("c", 64), ProfileSHA256: strings.Repeat("d", 64), AuthorityRevision: strings.Repeat("e", 64), IssueNumbers: []int{1}, Title: "Bounded fixture", Body: "Synthetic input, not execution authority.", AcceptanceCriteria: []string{"Preserve the source fixture"}, ExpiresAt: now.Add(time.Minute).UnixMilli()}
}
func TestBoundAssignmentAndProfile(t *testing.T) {
	now := time.Now()
	a := assignmentFixture(now)
	raw, _ := json.Marshal(a)
	got, e := ParseAssignment(raw, workspacebundle.Digest(raw), now)
	if e != nil || got.WorkItemID != a.WorkItemID {
		t.Fatal(e)
	}
	p := profileFixture()
	raw, _ = json.Marshal(p)
	if _, e = ParseProfile(raw, workspacebundle.Digest(raw)); e != nil {
		t.Fatal(e)
	}
	if _, e = ParseProfile(raw, strings.Repeat("f", 64)); e == nil {
		t.Fatal("profile digest mismatch accepted")
	}
}
func TestAssignmentRejectsAmbiguityAndExpiredState(t *testing.T) {
	now := time.Now()
	a := assignmentFixture(now)
	raw, _ := json.Marshal(a)
	variants := []string{strings.Replace(string(raw), `"version":`, `"Version":`, 1), strings.Replace(string(raw), `"attempt":1`, `"attempt":1,"attempt":2`, 1), strings.Replace(string(raw), `"issueNumbers":[1]`, `"issueNumbers":null`, 1), string(raw) + `{}`, strings.Replace(string(raw), `"attempt":1`, `"attempt":1,"command":"shell"`, 1)}
	for _, v := range variants {
		if _, e := ParseAssignment([]byte(v), workspacebundle.Digest([]byte(v)), now); e == nil {
			t.Fatal("ambiguous assignment accepted")
		}
	}
	mutations := []func(*Assignment){func(a *Assignment) { a.Attempt = 17 }, func(a *Assignment) { a.ExpiresAt = now.UnixMilli() }, func(a *Assignment) { a.ExpiresAt = now.Add(2 * time.Hour).UnixMilli() }, func(a *Assignment) { a.Repository = "../product" }, func(a *Assignment) { a.IssueNumbers = []int{1, 1} }, func(a *Assignment) { a.AcceptanceCriteria = []string{""} }, func(a *Assignment) { a.TreeSHA256 = "unbound" }, func(a *Assignment) { a.Body = strings.Repeat("x", 32*1024+1) }}
	for _, mutate := range mutations {
		a := assignmentFixture(now)
		mutate(&a)
		raw, _ := json.Marshal(a)
		if _, e := ParseAssignment(raw, workspacebundle.Digest(raw), now); e == nil {
			t.Fatal("invalid bound accepted")
		}
	}
	if _, e := ParseAssignment(raw, strings.Repeat("f", 64), now); e == nil {
		t.Fatal("assignment digest mismatch accepted")
	}
}
func TestProfileRejectsUnboundedOrExecutableConfiguration(t *testing.T) {
	mutations := []func(*Profile){func(p *Profile) { p.MaxSteps = 33 }, func(p *Profile) { p.MaxSeconds = 0 }, func(p *Profile) { p.MaxSeconds = 901 }, func(p *Profile) { p.MaxTokens = 8193 }, func(p *Profile) { p.Model = "with whitespace" }, func(p *Profile) { p.ADevSHA256 = "unpinned" }}
	for _, mutate := range mutations {
		p := profileFixture()
		mutate(&p)
		raw, _ := json.Marshal(p)
		if _, e := ParseProfile(raw, workspacebundle.Digest(raw)); e == nil {
			t.Fatal("invalid profile accepted")
		}
	}
	raw, _ := json.Marshal(profileFixture())
	raw = append(raw[:len(raw)-1], []byte(`,"providerURL":"http://attacker"}`)...)
	if _, e := ParseProfile(raw, workspacebundle.Digest(raw)); e == nil {
		t.Fatal("provider override accepted")
	}
}
