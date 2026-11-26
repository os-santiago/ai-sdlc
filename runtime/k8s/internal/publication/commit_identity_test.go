package publication

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
)

var configuredIdentity = project.CommitIdentity{Name: "Example Delivery Bot", Email: "delivery-bot@example.org"}

func TestCommitBodyUsesTheDefaultIdentity(t *testing.T) {
	if (project.Publication{}).Identity() != project.DefaultCommitIdentity || project.DefaultCommitIdentity.Validate() != nil {
		t.Fatal("default identity")
	}
	date := time.Date(2026, 11, 25, 1, 2, 3, 0, time.UTC)
	c := commitBody("subject", strings.Repeat("a", 40), strings.Repeat("b", 40), project.DefaultCommitIdentity, date)
	want := commitPerson{"ai-sdlc[bot]", "noreply@ai-sdlc.invalid", "2026-11-25T01:02:03Z"}
	if c.Author != want || c.Committer != want || c.Message != "subject\n\nSigned-off-by: ai-sdlc[bot] <noreply@ai-sdlc.invalid>\n" {
		t.Fatalf("default commit body %+v", c)
	}
}

func TestCommitBodyUsesTheConfiguredIdentity(t *testing.T) {
	date := time.Date(2026, 11, 25, 1, 2, 3, 0, time.UTC)
	c := commitBody("subject", strings.Repeat("a", 40), strings.Repeat("b", 40), configuredIdentity, date)
	want := commitPerson{configuredIdentity.Name, configuredIdentity.Email, "2026-11-25T01:02:03Z"}
	if c.Author != want || c.Committer != want || !strings.HasSuffix(c.Message, "\n\nSigned-off-by: Example Delivery Bot <delivery-bot@example.org>\n") {
		t.Fatalf("configured commit body %+v", c)
	}
	if commitSHA(c) == commitSHA(commitBody("subject", strings.Repeat("a", 40), strings.Repeat("b", 40), project.DefaultCommitIdentity, date)) {
		t.Fatal("identity not bound into the commit SHA")
	}
}

// The planned commit SHA must be Git's own object identity for the commit
// with the configured identity.
func TestCommitSHAMatchesGitCommitTreeForTheConfiguredIdentity(t *testing.T) {
	if _, e := exec.LookPath("git"); e != nil {
		t.Skip("git unavailable")
	}
	dir := t.TempDir()
	date := time.Date(2026, 11, 25, 1, 2, 3, 0, time.UTC)
	env := []string{"GIT_CONFIG_NOSYSTEM=1", "HOME=" + dir, "TZ=UTC",
		"GIT_AUTHOR_NAME=" + configuredIdentity.Name, "GIT_AUTHOR_EMAIL=" + configuredIdentity.Email,
		"GIT_COMMITTER_NAME=" + configuredIdentity.Name, "GIT_COMMITTER_EMAIL=" + configuredIdentity.Email,
		"GIT_AUTHOR_DATE=" + date.Format(time.RFC3339), "GIT_COMMITTER_DATE=" + date.Format(time.RFC3339)}
	run := func(stdin string, args ...string) string {
		cmd := exec.Command("git", append([]string{"-c", "commit.gpgsign=false", "-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), env...)
		cmd.Stdin = strings.NewReader(stdin)
		out, e := cmd.Output()
		if e != nil {
			t.Fatal("git", args[0], e)
		}
		return strings.TrimSpace(string(out))
	}
	run("", "init", "-q")
	blob := run("source\n", "hash-object", "-w", "--stdin")
	tree := run("100644 blob "+blob+"\tREADME.md\n", "mktree")
	parent := run("parent\n", "commit-tree", tree)
	for _, identity := range []project.CommitIdentity{configuredIdentity, project.DefaultCommitIdentity} {
		env[3], env[4], env[5], env[6] = "GIT_AUTHOR_NAME="+identity.Name, "GIT_AUTHOR_EMAIL="+identity.Email, "GIT_COMMITTER_NAME="+identity.Name, "GIT_COMMITTER_EMAIL="+identity.Email
		c := commitBody(writerPlan().CommitMessage, tree, parent, identity, date)
		// commit-tree -F - keeps the message bytes, including the trailer.
		if got := run(c.Message, "commit-tree", tree, "-p", parent, "-F", "-"); got != commitSHA(c) {
			t.Fatalf("%s: planned commit %s, git %s", identity.Name, commitSHA(c), got)
		}
	}
}

func TestWriterCommitsWithThePlannedIdentity(t *testing.T) {
	p := writerPlan()
	p.CommitIdentity = configuredIdentity
	source := strings.Repeat("d", 40)
	now := time.Now().UTC().Truncate(time.Second)
	planned := commitSHA(commitBody(p.CommitMessage, p.ExpectedTreeSHA, source, configuredIdentity, now))
	var forgeSHA atomic.Value
	forgeSHA.Store(planned)
	var calls atomic.Int32
	w, _ := writerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		var c commitRequest
		if json.Unmarshal(body, &c) != nil || c.Author.Name != configuredIdentity.Name || c.Author.Email != configuredIdentity.Email || c.Author != c.Committer ||
			!strings.HasSuffix(c.Message, "Signed-off-by: Example Delivery Bot <delivery-bot@example.org>\n") || strings.Contains(string(body), "ai-sdlc[bot]") {
			t.Error("commit does not carry the planned identity")
		}
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]any{"sha": forgeSHA.Load(), "message": c.Message, "tree": map[string]string{"sha": c.Tree}, "parents": []any{map[string]string{"sha": source}}, "author": c.Author, "committer": c.Committer})
	})
	fence := func(context.Context, string) error { return nil }
	if sha, e := w.CreateCommit(context.Background(), fence, p, source, now); e != nil || sha != planned {
		t.Fatal("planned commit refused", e)
	}
	// A forge commit that is not the planned object (for example one created
	// with another identity) yields no receipt.
	forgeSHA.Store(commitSHA(commitBody(p.CommitMessage, p.ExpectedTreeSHA, source, project.DefaultCommitIdentity, now)))
	if _, e := w.CreateCommit(context.Background(), fence, p, source, now); e != ErrPublication {
		t.Fatal("unplanned commit object accepted")
	}
	if calls.Load() != 2 {
		t.Fatal("write count")
	}
	// A plan without a valid identity never reaches the network.
	for _, identity := range []project.CommitIdentity{{}, {Name: "Bot", Email: "bot"}, {Name: "Bot <x@y.z>", Email: "bot@example.org"}, {Name: " Bot", Email: "bot@example.org"}} {
		q := writerPlan()
		q.CommitIdentity = identity
		if _, e := w.CreateCommit(context.Background(), fence, q, source, now); e != ErrPublication {
			t.Fatal("invalid identity accepted", identity)
		}
	}
	if calls.Load() != 2 {
		t.Fatal("invalid identity reached the network")
	}
}
