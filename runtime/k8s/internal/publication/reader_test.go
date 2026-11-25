package publication

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

// fakeForge serves a closed set of read endpoints. Any method other than GET
// is a forge write and fails the test.
type fakeForge struct {
	t              *testing.T
	reads, writes  atomic.Int32
	objects        map[string]any // path (with query) -> 200 JSON body
	status         map[string]int // path -> forced status
	controlMissing bool
	lastQuery      string
	lastAuthorized bool
}

func (f *fakeForge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		f.writes.Add(1)
		f.t.Error("forge write during reconciliation", r.Method)
		w.WriteHeader(500)
		return
	}
	f.reads.Add(1)
	f.lastAuthorized = r.Header.Get("Authorization") == "Bearer "+strings.Repeat("x", 40)
	path := strings.TrimPrefix(r.URL.Path, "/repos/os-santiago/homedir")
	if r.URL.Path == path {
		w.WriteHeader(403)
		return
	}
	if path == "/pulls" {
		f.lastQuery = r.URL.RawQuery
	}
	if path == "/git/commits/"+strings.Repeat("d", 40) && !f.controlMissing {
		json.NewEncoder(w).Encode(map[string]string{"sha": strings.Repeat("d", 40)})
		return
	}
	if status, ok := f.status[path]; ok {
		w.WriteHeader(status)
		w.Write([]byte(`{"message":"provider detail"}`))
		return
	}
	if body, ok := f.objects[path]; ok {
		json.NewEncoder(w).Encode(body)
		return
	}
	w.WriteHeader(404)
	w.Write([]byte(`{"message":"Not Found"}`))
}

func readerFixture(t *testing.T, forge_ *fakeForge) *Reader {
	t.Helper()
	forge_.t = t
	server := httptest.NewTLSServer(forge_)
	t.Cleanup(server.Close)
	r, e := NewReader(forge.Credential{Version: 1, Mode: "gh-cli", Host: "github.com", Repository: "os-santiago/homedir", ActorID: 1, ActorLogin: "fixture", Token: strings.Repeat("x", 40)})
	if e != nil {
		t.Fatal(e)
	}
	r.base = server.URL
	r.http.Transport.(*http.Transport).TLSClientConfig.RootCAs = server.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	return r
}

func readerExpectations(t *testing.T) map[string]Expectation {
	t.Helper()
	p := writerPlan()
	source, commit := strings.Repeat("d", 40), strings.Repeat("e", 40)
	tree, _ := ExpectTree(p, source)
	c, _ := ExpectCommit(p.Repository, 1581, p.ExpectedTreeSHA, source, time.Date(2026, 11, 25, 1, 2, 3, 0, time.UTC))
	branch, _ := ExpectBranch(p.Repository, p.Branch, commit, source)
	pr, _ := ExpectPullRequest(p.Repository, 1581, p.Branch, commit, strings.Repeat("f", 64), source)
	return map[string]Expectation{EffectTree: tree, EffectCommit: c, EffectBranch: branch, EffectPullRequest: pr}
}

// exactObjects returns, per kind, the read path and the forge body showing the
// planned effect exactly.
func exactObjects(x map[string]Expectation) map[string]map[string]any {
	repo := map[string]string{"full_name": "os-santiago/homedir"}
	c := x[EffectCommit]
	person := map[string]string{"name": "Sergio Canales", "email": "sergio.canales.e@gmail.com", "date": c.Date.Format(time.RFC3339)}
	b, pr := x[EffectBranch], x[EffectPullRequest]
	body := "Refs #1581\n\nAI-SDLC verified candidate.\n" + proofMarker(pr.PlanDigest) + "\n"
	return map[string]map[string]any{
		EffectTree:   {"/git/trees/" + x[EffectTree].SHA: map[string]any{"sha": x[EffectTree].SHA, "truncated": false}},
		EffectCommit: {"/git/commits/" + c.SHA: map[string]any{"sha": c.SHA, "message": c.Message, "tree": map[string]string{"sha": c.Tree}, "parents": []any{map[string]string{"sha": c.Source}}, "author": person, "committer": person}},
		EffectBranch: {"/git/ref/heads/" + b.Branch: map[string]any{"ref": "refs/heads/" + b.Branch, "object": map[string]string{"type": "commit", "sha": b.SHA}}},
		EffectPullRequest: {"/pulls": []any{map[string]any{"number": 1600, "state": "open", "draft": false, "body": body,
			"head": map[string]any{"ref": pr.Branch, "sha": pr.SHA, "repo": repo}, "base": map[string]any{"ref": "main", "sha": strings.Repeat("9", 40), "repo": repo}}}},
	}
}

func TestReaderObservesEachEffectReadOnly(t *testing.T) {
	expectations := readerExpectations(t)
	for _, kind := range []string{EffectTree, EffectCommit, EffectBranch, EffectPullRequest} {
		x := expectations[kind]
		exact := exactObjects(expectations)[kind]
		var path string
		for p := range exact {
			path = p
		}
		t.Run(kind+"/exact", func(t *testing.T) {
			f := &fakeForge{objects: map[string]any{path: exact[path]}}
			got, e := readerFixture(t, f).Observe(context.Background(), x)
			if e != nil || got.State != ObservedExact || (kind == EffectPullRequest) != (got.PullRequest == 1600) || !f.lastAuthorized {
				t.Fatal("exact effect", got, e)
			}
			if kind == EffectPullRequest && f.lastQuery != "head=os-santiago%3A"+strings.ReplaceAll(x.Branch, "/", "%2F")+"&per_page=100&state=all" {
				t.Fatal("pull request query", f.lastQuery)
			}
		})
		t.Run(kind+"/absent", func(t *testing.T) {
			f := &fakeForge{objects: map[string]any{}}
			if kind == EffectPullRequest {
				f.objects[path] = []any{}
			}
			got, e := readerFixture(t, f).Observe(context.Background(), x)
			if e != nil || got.State != ObservedAbsent {
				t.Fatal("absent effect", got, e)
			}
		})
		t.Run(kind+"/different", func(t *testing.T) {
			var other any
			raw, _ := json.Marshal(exact[path])
			switch kind {
			case EffectTree:
				other = map[string]any{"sha": x.SHA, "truncated": true}
			case EffectCommit:
				var v map[string]any
				json.Unmarshal(raw, &v)
				v["parents"] = []any{map[string]string{"sha": strings.Repeat("7", 40)}}
				other = v
			case EffectBranch:
				other = map[string]any{"ref": "refs/heads/" + x.Branch, "object": map[string]string{"type": "commit", "sha": strings.Repeat("7", 40)}}
			case EffectPullRequest:
				var v []map[string]any
				json.Unmarshal(raw, &v)
				v[0]["body"] = "Refs #1581\n\nedited"
				other = v
			}
			f := &fakeForge{objects: map[string]any{path: other}}
			got, e := readerFixture(t, f).Observe(context.Background(), x)
			if e != nil || got.State != ObservedDifferent {
				t.Fatal("different effect", got, e)
			}
		})
		t.Run(kind+"/error", func(t *testing.T) {
			for _, status := range []int{500, 502, 403, 301} {
				f := &fakeForge{objects: map[string]any{path: exact[path]}, status: map[string]int{path: status}}
				if got, e := readerFixture(t, f).Observe(context.Background(), x); e == nil {
					t.Fatal("observation error became an outcome", status, got)
				}
			}
		})
		t.Run(kind+"/unreadable-repository", func(t *testing.T) {
			// Without a visible parent commit, a 404 proves nothing.
			f := &fakeForge{objects: map[string]any{}, controlMissing: true}
			if got, e := readerFixture(t, f).Observe(context.Background(), x); e == nil {
				t.Fatal("absence trusted without read access", got)
			}
		})
	}
}

func TestReaderPullRequestAmbiguityStaysQuarantined(t *testing.T) {
	x := readerExpectations(t)[EffectPullRequest]
	exact := exactObjects(readerExpectations(t))[EffectPullRequest]["/pulls"].([]any)
	closed := map[string]any{}
	raw, _ := json.Marshal(exact[0])
	json.Unmarshal(raw, &closed)
	closed["state"], closed["number"] = "closed", 1599
	for name, list := range map[string]any{"two": append([]any{closed}, exact...), "closed": []any{closed}, "null": nil, "object": map[string]any{"number": 1}} {
		f := &fakeForge{objects: map[string]any{"/pulls": list}}
		got, e := readerFixture(t, f).Observe(context.Background(), x)
		if e == nil && got.State != ObservedDifferent {
			t.Fatal("ambiguous pull request resolved", name, got)
		}
	}
}

func TestReaderRejectsUnplannedExpectationsBeforeNetwork(t *testing.T) {
	f := &fakeForge{objects: map[string]any{}}
	r := readerFixture(t, f)
	x := readerExpectations(t)
	for _, mutate := range []func(*Expectation){
		func(e *Expectation) { e.Repository = "os-santiago/saferus" },
		func(e *Expectation) { e.Kind = "merge" },
		func(e *Expectation) { e.Branch = "../../pulls" },
		func(e *Expectation) { e.SHA = "main" },
		func(e *Expectation) { e.Source = "" },
	} {
		v := x[EffectBranch]
		mutate(&v)
		if _, e := r.Observe(context.Background(), v); e == nil {
			t.Fatal("unplanned expectation observed")
		}
	}
	if f.reads.Load() != 0 || f.writes.Load() != 0 {
		t.Fatal("unplanned expectation reached the forge")
	}
}
