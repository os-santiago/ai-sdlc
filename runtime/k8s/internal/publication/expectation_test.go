package publication

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

// Every expectation must re-derive the exact digest the Writer journals
// before its single POST, or reconciliation could observe a different write.
func TestExpectationsMatchWriterJournalDigests(t *testing.T) {
	p := writerPlan()
	source, commit := strings.Repeat("d", 40), strings.Repeat("e", 40)
	date := time.Now().UTC().Truncate(time.Second)
	planDigest := strings.Repeat("f", 64)
	var fenced []string
	w, _ := writerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503) // the digest is journaled before the outcome
	})
	fence := func(_ context.Context, digest string) error { fenced = append(fenced, digest); return nil }
	ctx := context.Background()
	w.CreateTree(ctx, fence, p)
	w.CreateCommit(ctx, fence, p, source, date)
	w.CreateBranch(ctx, fence, p, commit)
	w.CreatePullRequest(ctx, fence, p, source, commit, planDigest)
	if len(fenced) != 4 {
		t.Fatal("writer did not journal four requests", len(fenced))
	}
	tree, e1 := ExpectTree(p, source)
	c, e2 := ExpectCommit(p, source, date)
	branch, e3 := ExpectBranch(p.Repository, p.Branch, commit, source)
	pr, e4 := ExpectPullRequest(p.Repository, 1581, p.Branch, commit, planDigest, source)
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil {
		t.Fatal(e1, e2, e3, e4)
	}
	for i, x := range []Expectation{tree, c, branch, pr} {
		if x.RequestDigest != fenced[i] || x.Source != source {
			t.Fatal("expectation digest differs from the journaled request", x.Kind)
		}
	}
	if tree.SHA != p.ExpectedTreeSHA || c.Tree != p.ExpectedTreeSHA || c.Identity != p.CommitIdentity || !workspacebundle.ValidSourceSHA(c.SHA) || branch.SHA != commit || branch.Branch != p.Branch || pr.SHA != commit || pr.PlanDigest != planDigest {
		t.Fatal("planned identities")
	}
	// A different commit time is a different planned commit.
	if other, _ := ExpectCommit(p, source, date.Add(time.Second)); other.SHA == c.SHA || other.RequestDigest == c.RequestDigest {
		t.Fatal("commit time not bound")
	}
	// A different recorded identity is a different planned commit.
	q := p
	q.CommitIdentity = configuredIdentity
	if other, e := ExpectCommit(q, source, date); e != nil || other.SHA == c.SHA || other.RequestDigest == c.RequestDigest || other.Identity != configuredIdentity {
		t.Fatal("commit identity not bound", e)
	}
	for _, bad := range []func() error{
		func() error { _, e := ExpectBranch(p.Repository, "main", commit, source); return e },
		func() error {
			_, e := ExpectCommit(p, source, date.Add(time.Millisecond))
			return e
		},
		func() error {
			// A plan without a recorded identity cannot plan a commit.
			q := p
			q.CommitIdentity = project.CommitIdentity{}
			_, e := ExpectCommit(q, source, date)
			return e
		},
		func() error {
			_, e := ExpectPullRequest(p.Repository, 1581, p.Branch, commit, "x", source)
			return e
		},
		func() error { _, e := ExpectTree(p, "HEAD"); return e },
		func() error { _, e := ExpectBranch("other/repository", p.Branch, commit, source); return e },
	} {
		if bad() == nil {
			t.Fatal("unplanned expectation accepted")
		}
	}
}

// The planned commit SHA must be Git's own object identity for that commit.
func TestPlannedCommitSHAIsGitObjectIdentity(t *testing.T) {
	if _, e := exec.LookPath("git"); e != nil {
		t.Skip("git unavailable")
	}
	dir := t.TempDir()
	run := func(stdin string, env []string, args ...string) string {
		cmd := exec.Command("git", append([]string{"-c", "commit.gpgsign=false", "-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), env...)
		cmd.Stdin = strings.NewReader(stdin)
		out, e := cmd.Output()
		if e != nil {
			t.Fatal("git", args[0], e)
		}
		return strings.TrimSpace(string(out))
	}
	run("", nil, "init", "-q", "--object-format=sha1")
	blob := run("source\n", nil, "hash-object", "-w", "--stdin")
	tree := run("100644 blob "+blob+"\tREADME.md\n", nil, "mktree")
	date := time.Date(2026, 11, 25, 1, 2, 3, 0, time.UTC)
	// A non-default recorded identity: the expectation must use the plan's.
	p := writerPlan()
	p.ExpectedTreeSHA, p.CommitIdentity = tree, configuredIdentity
	identity := []string{"GIT_AUTHOR_NAME=" + configuredIdentity.Name, "GIT_AUTHOR_EMAIL=" + configuredIdentity.Email, "GIT_COMMITTER_NAME=" + configuredIdentity.Name, "GIT_COMMITTER_EMAIL=" + configuredIdentity.Email,
		"GIT_AUTHOR_DATE=" + date.Format(time.RFC3339), "GIT_COMMITTER_DATE=" + date.Format(time.RFC3339), "TZ=UTC", "GIT_CONFIG_NOSYSTEM=1", "HOME=" + dir}
	parent := run("parent\n", identity, "commit-tree", tree)
	x, e := ExpectCommit(p, parent, date)
	if e != nil {
		t.Fatal(e)
	}
	// commit-tree -F - keeps the message bytes, including the trailer.
	cmd := exec.Command("git", "-c", "commit.gpgsign=false", "-C", dir, "commit-tree", tree, "-p", parent, "-F", "-")
	cmd.Env = append(os.Environ(), identity...)
	cmd.Stdin = strings.NewReader(x.Message)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != x.SHA {
		t.Fatalf("planned commit %s, git %s", x.SHA, got)
	}
	var body commitRequest
	raw, _ := json.Marshal(commitBody(p.CommitMessage, tree, parent, p.CommitIdentity, date))
	if json.Unmarshal(raw, &body) != nil || body.Message != x.Message {
		t.Fatal("expectation message differs from the request")
	}
}
