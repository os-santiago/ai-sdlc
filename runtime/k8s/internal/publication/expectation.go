package publication

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

// Effect kinds, in the only order the journal admits them.
const (
	EffectTree        = "tree"
	EffectCommit      = "commit"
	EffectBranch      = "branch"
	EffectPullRequest = "pull-request"
)

// Expectation is one planned write effect, re-derived only from journaled
// facts: the signed command, the claim's plan digest and commit time, and the
// receipts of the effects that preceded it. RequestDigest is the SHA-256 of
// the exact body the Writer sends, so a reconciler can prove that it observes
// the write its journal recorded before the POST. An Expectation authorizes
// no write; it only describes what a read-only observation must find.
type Expectation struct {
	Kind, Repository, RequestDigest string
	// Source is the publication's parent commit. The forge must show it before
	// any absence it reports is trusted.
	Source string
	// SHA is the tree or commit object; for a branch or pull request, the head commit.
	SHA string
	// Commit fields.
	Tree, Message string
	Date          time.Time
	// Branch and pull request fields.
	Branch, PlanDigest string
}

func expectation(kind, repository, source string, body any) (Expectation, error) {
	raw, e := json.Marshal(body)
	if e != nil || !project.Supported(repository) || !workspacebundle.ValidSourceSHA(source) {
		return Expectation{}, ErrPublication
	}
	return Expectation{Kind: kind, Repository: repository, Source: source, RequestDigest: authority.Digest(raw)}, nil
}

// ExpectTree describes the tree a reconstructed plan creates. Its identity is
// content-addressed: the planned SHA is computed from the planned files.
func ExpectTree(p TreePlan, source string) (Expectation, error) {
	if _, e := validatePlan(p.Repository, p); e != nil {
		return Expectation{}, e
	}
	x, e := expectation(EffectTree, p.Repository, source, p.Request)
	x.SHA = p.ExpectedTreeSHA
	return x, e
}

// ExpectCommit describes the commit created on tree with the claim's commit
// time. Its SHA is the Git object identity of exactly that commit content.
func ExpectCommit(repository string, issue int, tree, source string, date time.Time) (Expectation, error) {
	if issue < 1 || issue > 2147483647 || !workspacebundle.ValidSourceSHA(tree) || date.IsZero() || date.Nanosecond() != 0 {
		return Expectation{}, ErrPublication
	}
	body := commitBody(CommitMessage(issue), tree, source, date)
	x, e := expectation(EffectCommit, repository, source, body)
	x.Tree, x.Message, x.Date = tree, body.Message, date.UTC()
	x.SHA = commitSHA(body)
	return x, e
}

// ExpectBranch describes the ref creation of branch at commit.
func ExpectBranch(repository, branch, commit, source string) (Expectation, error) {
	if !pilotBranch.MatchString(branch) || !workspacebundle.ValidSourceSHA(commit) {
		return Expectation{}, ErrPublication
	}
	x, e := expectation(EffectBranch, repository, source, branchBody(branch, commit))
	x.SHA, x.Branch = commit, branch
	return x, e
}

// ExpectPullRequest describes the pull request from branch at commit whose
// body carries the plan digest.
func ExpectPullRequest(repository string, issue int, branch, commit, planDigest, source string) (Expectation, error) {
	if issue < 1 || issue > 2147483647 || !pilotBranch.MatchString(branch) || !workspacebundle.ValidSourceSHA(commit) || !workspacebundle.ValidDigest(planDigest) {
		return Expectation{}, ErrPublication
	}
	x, e := expectation(EffectPullRequest, repository, source, pullRequestBody(repository, issue, CommitMessage(issue), branch, planDigest))
	x.SHA, x.Branch, x.PlanDigest = commit, branch, planDigest
	return x, e
}

// commitSHA is the canonical Git commit object identity GitHub assigns to the
// commit the Writer requests: one parent, one identity, UTC timestamps.
func commitSHA(c commitRequest) string {
	date, e := time.Parse(time.RFC3339, c.Author.Date)
	if e != nil || len(c.Parents) != 1 {
		return ""
	}
	person := func(p commitPerson) string {
		return fmt.Sprintf("%s <%s> %d +0000", p.Name, p.Email, date.Unix())
	}
	raw := fmt.Sprintf("tree %s\nparent %s\nauthor %s\ncommitter %s\n\n%s", c.Tree, c.Parents[0], person(c.Author), person(c.Committer), c.Message)
	return hex.EncodeToString(objectID("commit", []byte(raw)))
}
