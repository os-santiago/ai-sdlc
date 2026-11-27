package mergewriter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/forge"
)

func readerFixture(t *testing.T, h http.HandlerFunc) (*Reader, context.Context, *atomic.Int32) {
	t.Helper()
	var writes atomic.Int32
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes.Add(1)
			http.Error(w, "write", http.StatusMethodNotAllowed)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("x", 40) || !r.Close {
			t.Error("credential or non-replayable connection")
		}
		h(w, r)
	}))
	t.Cleanup(s.Close)
	reader, e := NewReader(forge.Credential{Version: 1, Mode: "gh-cli", Host: "github.com", Repository: repository, ActorID: 1, ActorLogin: "fixture", Token: strings.Repeat("x", 40)})
	if e != nil {
		t.Fatal(e)
	}
	transport := reader.http.Transport.(*http.Transport)
	transport.TLSClientConfig.RootCAs = s.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	reader.base = s.URL
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return reader, ctx, &writes
}

// pullResponse is the GitHub pull request the runtime published, merged
// exactly as planned unless mutated.
func pullResponse() map[string]any {
	e := expected()
	repo := map[string]string{"full_name": repository}
	return map[string]any{"number": 42, "state": "closed", "draft": false, "merged": true, "merge_commit_sha": strings.Repeat("e", 40),
		"head": map[string]any{"ref": e.Branch, "sha": e.HeadSHA, "repo": repo},
		"base": map[string]any{"ref": "main", "sha": e.BaseSHA, "repo": repo}}
}

func openPull() map[string]any {
	p := pullResponse()
	p["state"], p["merged"], p["merge_commit_sha"] = "open", false, nil
	return p
}

func mainRef(sha string) map[string]any {
	return map[string]any{"ref": "refs/heads/main", "object": map[string]string{"type": "commit", "sha": sha}}
}

type forgeView struct {
	pull, commit, ref, compare map[string]any
	status                     map[string]int
}

const movedMain = "6666666666666666666666666666666666666666"

func compareResponse(status string, ahead int) map[string]any {
	return map[string]any{"status": status, "ahead_by": ahead, "behind_by": 3, "base_commit": map[string]string{"sha": movedMain}}
}

func (v forgeView) serve(t *testing.T, paths *[]string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		*paths = append(*paths, r.URL.Path)
		var body map[string]any
		var kind string
		switch r.URL.Path {
		case "/repos/os-santiago/homedir/pulls/42":
			body, kind = v.pull, "pull"
		case "/repos/os-santiago/homedir/git/commits/" + strings.Repeat("e", 40):
			body, kind = v.commit, "commit"
		case "/repos/os-santiago/homedir/git/ref/heads/main":
			body, kind = v.ref, "ref"
		case "/repos/os-santiago/homedir/compare/" + movedMain + "..." + expected().HeadSHA:
			if r.URL.RawQuery != "per_page=1" {
				t.Errorf("unbounded comparison %q", r.URL.RawQuery)
			}
			body, kind = v.compare, "compare"
		default:
			t.Errorf("unbounded read %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if code := v.status[kind]; code != 0 {
			http.Error(w, `{"message":"no"}`, code)
			return
		}
		_ = json.NewEncoder(w).Encode(body)
	}
}

func plan(t *testing.T) Plan {
	t.Helper()
	p, e := PlanMerge(expected(), 3)
	if e != nil {
		t.Fatal(e)
	}
	return p
}

// The plan is the request the transport sends: the same digest reaches the
// pre-send fence, so a reconciler re-derives exactly what was journaled.
func TestPlanIsTheRequestTheTransportSends(t *testing.T) {
	var fenced string
	w, ctx := fixture(t, func(out http.ResponseWriter, r *http.Request) { http.Error(out, "", http.StatusConflict) })
	_, _ = w.Merge(ctx, expected(), 3, func(_ context.Context, d string) error { fenced = d; return nil })
	if p := plan(t); fenced == "" || p.RequestDigest != fenced || p.Title != "fix(ai-sdlc): integrate issue #3 [skip release]" || p.Message != "Refs #3" {
		t.Fatal("plan is not the sent request", p.RequestDigest, fenced)
	}
	for _, bad := range []func(*Plan){
		func(p *Plan) { p.Issue = 4 },
		func(p *Plan) { p.Expected.Branch = "feature/x" },
		func(p *Plan) { p.Expected.HeadSHA = p.Expected.BaseSHA },
		func(p *Plan) { p.Expected.TreeSHA = "not-a-sha" },
		func(p *Plan) { p.Expected.PullRequest = 0 },
		func(p *Plan) { p.Expected.Repository = "../escape" },
	} {
		p := plan(t)
		bad(&p)
		if _, e := PlanMerge(p.Expected, p.Issue); e == nil {
			t.Fatal("invalid plan accepted", p.Expected, p.Issue)
		}
	}
}

func TestReaderObservesOnlyThePlannedMerge(t *testing.T) {
	e := expected()
	cases := map[string]struct {
		view  func(*forgeView)
		state string
		err   bool
	}{
		"merged-exactly": {func(*forgeView) {}, ObservedMerged, false},
		"merged-trailing-lines": {func(v *forgeView) {
			v.commit["message"] = "fix(ai-sdlc): integrate issue #3 [skip release]\n\nRefs #3\n"
		}, ObservedMerged, false},
		"wrong-tree": {func(v *forgeView) { v.commit["tree"] = map[string]string{"sha": strings.Repeat("f", 40)} }, ObservedDifferent, false},
		"swapped-parents": {func(v *forgeView) {
			v.commit["parents"] = []map[string]string{{"sha": e.HeadSHA}, {"sha": e.BaseSHA}}
		}, ObservedDifferent, false},
		"other-base-parent": {func(v *forgeView) {
			v.commit["parents"] = []map[string]string{{"sha": strings.Repeat("9", 40)}, {"sha": e.HeadSHA}}
		}, ObservedDifferent, false},
		"squash-single-parent": {func(v *forgeView) { v.commit["parents"] = []map[string]string{{"sha": e.BaseSHA}} }, ObservedDifferent, false},
		"other-title":          {func(v *forgeView) { v.commit["message"] = "Merge pull request #42\n\nRefs #3" }, ObservedDifferent, false},
		"missing-refs":         {func(v *forgeView) { v.commit["message"] = "fix(ai-sdlc): integrate issue #3 [skip release]\n" }, ObservedDifferent, false},
		"other-refs":           {func(v *forgeView) { v.commit["message"] = "fix(ai-sdlc): integrate issue #3 [skip release]\n\nRefs #4" }, ObservedDifferent, false},
		"commit-sha-differs":   {func(v *forgeView) { v.commit["sha"] = strings.Repeat("8", 40) }, ObservedDifferent, false},
		"head-moved": {func(v *forgeView) {
			v.pull["head"] = map[string]any{"ref": e.Branch, "sha": strings.Repeat("7", 40), "repo": map[string]string{"full_name": repository}}
		}, ObservedDifferent, false},
		"fork-head": {func(v *forgeView) {
			v.pull["head"] = map[string]any{"ref": e.Branch, "sha": e.HeadSHA, "repo": map[string]string{"full_name": "someone/homedir"}}
		}, ObservedDifferent, false},
		"retargeted": {func(v *forgeView) {
			v.pull["base"] = map[string]any{"ref": "release", "sha": e.BaseSHA, "repo": map[string]string{"full_name": repository}}
		}, ObservedDifferent, false},
		"merged-but-open":     {func(v *forgeView) { v.pull["state"] = "open" }, ObservedDifferent, false},
		"merged-no-commit":    {func(v *forgeView) { v.pull["merge_commit_sha"] = nil }, ObservedDifferent, false},
		"merged-field-absent": {func(v *forgeView) { delete(v.pull, "merged") }, "", true},
		"open-unmerged":       {func(v *forgeView) { v.pull = openPull() }, ObservedUnmerged, false},
		// main moved: only a comparison that shows the head outside main proves
		// that nothing was merged.
		"open-base-moved-ahead": {func(v *forgeView) {
			v.pull, v.ref, v.compare = openPull(), mainRef(movedMain), compareResponse("ahead", 1)
		}, ObservedUnmerged, false},
		"open-base-moved-diverged": {func(v *forgeView) {
			v.pull, v.ref, v.compare = openPull(), mainRef(movedMain), compareResponse("diverged", 2)
		}, ObservedUnmerged, false},
		"open-base-moved-behind": {func(v *forgeView) {
			v.pull, v.ref, v.compare = openPull(), mainRef(movedMain), compareResponse("behind", 1)
		}, ObservedDifferent, false},
		"open-base-moved-identical": {func(v *forgeView) {
			v.pull, v.ref, v.compare = openPull(), mainRef(movedMain), compareResponse("identical", 1)
		}, ObservedDifferent, false},
		"open-base-moved-ahead-by-zero": {func(v *forgeView) {
			v.pull, v.ref, v.compare = openPull(), mainRef(movedMain), compareResponse("ahead", 0)
		}, ObservedDifferent, false},
		"open-base-moved-other-base-commit": {func(v *forgeView) {
			v.pull, v.ref, v.compare = openPull(), mainRef(movedMain), compareResponse("ahead", 1)
			v.compare["base_commit"] = map[string]string{"sha": e.BaseSHA}
		}, ObservedDifferent, false},
		"open-base-moved-compare-missing": {func(v *forgeView) {
			v.pull, v.ref, v.compare = openPull(), mainRef(movedMain), compareResponse("ahead", 1)
			v.status = map[string]int{"compare": 404}
		}, "", true},
		"open-base-moved-closed-pull": {func(v *forgeView) {
			v.pull, v.ref, v.compare = openPull(), mainRef(movedMain), compareResponse("ahead", 1)
			v.pull["state"] = "closed"
		}, ObservedDifferent, false},
		"open-head-moved-base-moved": {func(v *forgeView) {
			v.pull, v.ref, v.compare = openPull(), mainRef(movedMain), compareResponse("ahead", 1)
			v.pull["head"] = map[string]any{"ref": e.Branch, "sha": strings.Repeat("7", 40), "repo": map[string]string{"full_name": repository}}
		}, ObservedDifferent, false},
		"base-other-repo": {func(v *forgeView) {
			v.pull["base"] = map[string]any{"ref": "main", "sha": e.BaseSHA, "repo": map[string]string{"full_name": "someone/homedir"}}
		}, ObservedDifferent, false},
		"base-repo-absent": {func(v *forgeView) {
			v.pull["base"] = map[string]any{"ref": "main", "sha": e.BaseSHA}
		}, ObservedDifferent, false},
		"head-other-branch": {func(v *forgeView) {
			v.pull["head"] = map[string]any{"ref": "ai-sdlc/issue-3-" + strings.Repeat("0", 32), "sha": e.HeadSHA, "repo": map[string]string{"full_name": repository}}
		}, ObservedDifferent, false},
		"open-base-other-repo": {func(v *forgeView) {
			v.pull = openPull()
			v.pull["base"] = map[string]any{"ref": "main", "sha": e.BaseSHA, "repo": map[string]string{"full_name": "someone/homedir"}}
		}, ObservedDifferent, false},
		"open-draft":       {func(v *forgeView) { v.pull = openPull(); v.pull["draft"] = true }, ObservedDifferent, false},
		"closed-unmerged":  {func(v *forgeView) { v.pull = openPull(); v.pull["state"] = "closed" }, ObservedDifferent, false},
		"pull-missing":     {func(v *forgeView) { v.status = map[string]int{"pull": 404} }, "", true},
		"pull-forbidden":   {func(v *forgeView) { v.status = map[string]int{"pull": 403} }, "", true},
		"pull-unavailable": {func(v *forgeView) { v.status = map[string]int{"pull": 502} }, "", true},
		"commit-missing":   {func(v *forgeView) { v.status = map[string]int{"commit": 404} }, "", true},
		"main-missing":     {func(v *forgeView) { v.pull = openPull(); v.status = map[string]int{"ref": 404} }, "", true},
		"main-forbidden":   {func(v *forgeView) { v.pull = openPull(); v.status = map[string]int{"ref": 403} }, "", true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			v := forgeView{pull: pullResponse(), commit: commitResponse(), ref: mainRef(e.BaseSHA)}
			c.view(&v)
			var paths []string
			reader, ctx, writes := readerFixture(t, v.serve(t, &paths))
			got, err := reader.Observe(ctx, plan(t))
			if writes.Load() != 0 {
				t.Fatal("reader wrote")
			}
			if c.err {
				if err == nil || got != (Observation{}) {
					t.Fatal("doubtful read accepted", got)
				}
				return
			}
			if err != nil || got.State != c.state {
				t.Fatal(got, err)
			}
			if (got.State == ObservedMerged) != (got.CommitSHA == strings.Repeat("e", 40)) {
				t.Fatal("merge commit", got)
			}
		})
	}
}

func TestReaderRefusesAnotherRepositoryOrAHandMadePlan(t *testing.T) {
	var paths []string
	v := forgeView{pull: pullResponse(), commit: commitResponse(), ref: mainRef(expected().BaseSHA)}
	reader, ctx, _ := readerFixture(t, v.serve(t, &paths))
	other := expected()
	other.Repository = "os-santiago/saferus"
	p, e := PlanMerge(other, 3)
	if e != nil {
		t.Fatal(e)
	}
	if _, e := reader.Observe(ctx, p); e == nil {
		t.Fatal("another repository observed")
	}
	tampered := plan(t)
	tampered.RequestDigest = strings.Repeat("0", 64)
	if _, e := reader.Observe(ctx, tampered); e == nil {
		t.Fatal("hand-made plan observed")
	}
	tampered = plan(t)
	tampered.Title = "anything"
	if _, e := reader.Observe(ctx, tampered); e == nil {
		t.Fatal("hand-made title observed")
	}
	if len(paths) != 0 {
		t.Fatal("refused plan reached the forge", paths)
	}
	if _, e := NewReader(forge.Credential{Version: 1, Mode: "gh-cli", Host: "github.com", Repository: "example/unsupported", ActorID: 1, ActorLogin: "fixture", Token: strings.Repeat("x", 40)}); e == nil {
		t.Fatal("unsupported repository reader")
	}
}
