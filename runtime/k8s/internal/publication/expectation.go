package publication

import (
	"encoding/json"
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
	// Commit fields. Identity is the plan's recorded commit identity.
	Tree, Message string
	Date          time.Time
	Identity      project.CommitIdentity
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

// ExpectCommit describes the commit the plan creates on its tree with the
// claim's commit time: the plan's message and recorded commit identity, built
// by the writer's own commitBody. Its SHA is the Git object identity of exactly
// that commit, the one CreateCommit requires the forge to return.
func ExpectCommit(p TreePlan, source string, date time.Time) (Expectation, error) {
	if _, e := validatePlan(p.Repository, p); e != nil || date.IsZero() || date.Nanosecond() != 0 || p.CommitIdentity.Name == "" || p.CommitIdentity.Email == "" {
		return Expectation{}, ErrPublication
	}
	body := commitBody(p.CommitMessage, p.ExpectedTreeSHA, source, p.CommitIdentity, date)
	x, e := expectation(EffectCommit, p.Repository, source, body)
	x.Tree, x.Message, x.Date, x.Identity = p.ExpectedTreeSHA, body.Message, date.UTC(), p.CommitIdentity
	x.SHA = commitSHA(body)
	if x.SHA == "" {
		return Expectation{}, ErrPublication
	}
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
