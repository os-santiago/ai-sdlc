package publication

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/forge"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/verification"
)

func writerPlan() TreePlan {
	return TreePlan{Request: TreeRequest{BaseTree: strings.Repeat("a", 40), Tree: []TreeEntry{{verification.LocaleSource, "100644", "blob", "source"}, {verification.LocaleNewTest, "100644", "blob", "test"}}}, ExpectedTreeSHA: strings.Repeat("b", 40), Branch: "ai-sdlc/issue-1581-" + strings.Repeat("c", 32), CommitMessage: "fix(ai-sdlc): address issue #1581 [skip release]"}
}
func writerFixture(t *testing.T, handler http.HandlerFunc) (*Writer, *httptest.Server) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	w, e := NewWriter(forge.Credential{Version: 1, Mode: "gh-cli", Host: "github.com", Repository: "os-santiago/homedir", ActorID: 1, ActorLogin: "fixture", Token: strings.Repeat("x", 40)})
	if e != nil {
		t.Fatal(e)
	}
	w.base = server.URL
	w.http.Transport.(*http.Transport).TLSClientConfig.RootCAs = server.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	return w, server
}
func TestPublisherFinitePlanThroughActualTLS(t *testing.T) {
	p := writerPlan()
	source, commit := strings.Repeat("d", 40), strings.Repeat("e", 40)
	now := time.Now().UTC().Truncate(time.Second)
	var calls atomic.Int32
	fences := 0
	var expectedDigest atomic.Value
	w, _ := writerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer "+strings.Repeat("x", 40) || r.GetBody != nil {
			t.Error("wrong write boundary")
		}
		var raw map[string]json.RawMessage
		body, _ := io.ReadAll(r.Body)
		if expectedDigest.Load() != authority.Digest(body) {
			t.Error("journal fence digest differs from exact HTTP request")
		}
		if json.Unmarshal(body, &raw) != nil {
			t.Error("invalid request")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		switch r.URL.Path {
		case "/repos/os-santiago/homedir/git/trees":
			if len(raw) != 2 {
				t.Error("arbitrary tree fields")
			}
			json.NewEncoder(w).Encode(map[string]any{"sha": p.ExpectedTreeSHA, "truncated": false})
		case "/repos/os-santiago/homedir/git/commits":
			b, _ := json.Marshal(raw)
			var c commitRequest
			json.Unmarshal(b, &c)
			if c.Tree != p.ExpectedTreeSHA || len(c.Parents) != 1 || c.Parents[0] != source || !strings.Contains(c.Message, "[skip release]") || !strings.Contains(c.Message, "Signed-off-by: Sergio Canales <sergio.canales.e@gmail.com>") || c.Author.Name != "Sergio Canales" || c.Author.Email != "sergio.canales.e@gmail.com" || c.Author != c.Committer {
				t.Error("commit identity or scope changed")
			}
			json.NewEncoder(w).Encode(map[string]any{"sha": commit, "message": c.Message, "tree": map[string]string{"sha": c.Tree}, "parents": []any{map[string]string{"sha": source}}, "author": c.Author, "committer": c.Committer})
		case "/repos/os-santiago/homedir/git/refs":
			var ref, sha string
			json.Unmarshal(raw["ref"], &ref)
			json.Unmarshal(raw["sha"], &sha)
			if ref != "refs/heads/"+p.Branch || sha != commit || len(raw) != 2 {
				t.Error("mutable or unrelated ref")
			}
			json.NewEncoder(w).Encode(map[string]any{"ref": ref, "object": map[string]string{"type": "commit", "sha": sha}})
		case "/repos/os-santiago/homedir/pulls":
			var title, body, head, base string
			json.Unmarshal(raw["title"], &title)
			json.Unmarshal(raw["body"], &body)
			json.Unmarshal(raw["head"], &head)
			json.Unmarshal(raw["base"], &base)
			if title != p.CommitMessage || head != p.Branch || base != "main" || !strings.Contains(body, "Refs #1581") || strings.Contains(body, "Closes") {
				t.Error("PR changed derived scope")
			}
			repo := map[string]string{"full_name": "os-santiago/homedir"}
			json.NewEncoder(w).Encode(map[string]any{"number": 1600, "state": "open", "draft": false, "html_url": "https://github.com/os-santiago/homedir/pull/1600", "head": map[string]any{"ref": head, "sha": commit, "repo": repo}, "base": map[string]any{"ref": base, "sha": source, "repo": repo}})
		default:
			t.Error("unsupported endpoint")
		}
	})
	fence := func(_ context.Context, digest string) error { expectedDigest.Store(digest); fences++; return nil }
	ctx := context.Background()
	if sha, e := w.CreateTree(ctx, fence, p); e != nil || sha != p.ExpectedTreeSHA {
		t.Fatal("tree receipt", e)
	}
	if sha, e := w.CreateCommit(ctx, fence, p, source, now); e != nil || sha != commit {
		t.Fatal("commit receipt", e)
	}
	if e := w.CreateBranch(ctx, fence, p, commit); e != nil {
		t.Fatal("branch receipt", e)
	}
	if pr, e := w.CreatePullRequest(ctx, fence, p, source, commit, strings.Repeat("f", 64)); e != nil || pr.Number != 1600 {
		t.Fatal("PR receipt", e)
	}
	if calls.Load() != 4 || fences != 8 {
		t.Fatal("write count or current-authority fences differ")
	}
}
func TestPublisherAmbiguousResponsesNeverRetryOrLeak(t *testing.T) {
	for _, scenario := range []string{"redirect", "server", "duplicate", "wrong-tree", "oversized", "lost-reply", "revoked-after"} {
		t.Run(scenario, func(t *testing.T) {
			var calls, target atomic.Int32
			other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { target.Add(1) }))
			defer other.Close()
			w, _ := writerFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				switch scenario {
				case "redirect":
					w.Header().Set("Location", other.URL)
					w.WriteHeader(307)
				case "server":
					w.WriteHeader(503)
					w.Write([]byte("secret-provider-response"))
				case "duplicate":
					w.WriteHeader(201)
					w.Write([]byte(`{"sha":"` + strings.Repeat("b", 40) + `","sha":"` + strings.Repeat("a", 40) + `"}`))
				case "wrong-tree":
					w.WriteHeader(201)
					w.Write([]byte(`{"sha":"` + strings.Repeat("a", 40) + `"}`))
				case "oversized":
					w.Header().Set("Content-Length", "1048577")
					w.WriteHeader(201)
				case "lost-reply":
					conn, _, e := w.(http.Hijacker).Hijack()
					if e != nil {
						t.Error(e)
					}
					conn.Close()
				default:
					w.WriteHeader(201)
					w.Write([]byte(`{"sha":"` + strings.Repeat("b", 40) + `"}`))
				}
			})
			fences := 0
			fence := func(context.Context, string) error {
				fences++
				if scenario == "revoked-after" && fences == 2 {
					return errors.New("revoked")
				}
				return nil
			}
			if _, e := w.CreateTree(context.Background(), fence, writerPlan()); e != ErrPublication {
				t.Fatal("unsafe response yielded receipt", e)
			}
			if calls.Load() != 1 || target.Load() != 0 {
				t.Fatal("write replayed or credential followed redirect")
			}
		})
	}
}
func TestPublisherDeniesUnreviewedPlanBeforeNetwork(t *testing.T) {
	var calls atomic.Int32
	w, _ := writerFixture(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })
	for _, mutate := range []func(*TreePlan){func(p *TreePlan) { p.Branch = "main" }, func(p *TreePlan) { p.CommitMessage = "release production" }, func(p *TreePlan) { p.Request.Tree[0].Path = ".github/workflows/release.yml" }, func(p *TreePlan) { p.Request.Tree[0].Mode = "120000" }, func(p *TreePlan) { p.Request.Tree[1] = p.Request.Tree[0] }, func(p *TreePlan) { p.Request.BaseTree = "main" }} {
		p := writerPlan()
		mutate(&p)
		if _, e := w.CreateTree(context.Background(), func(context.Context, string) error { return nil }, p); e == nil {
			t.Fatal("unsafe plan reached writer")
		}
	}
	if _, e := w.CreateTree(context.Background(), nil, writerPlan()); e == nil {
		t.Fatal("missing authority fence")
	}
	if _, e := w.CreateTree(context.Background(), func(context.Context, string) error { return errors.New("revoked") }, writerPlan()); e == nil {
		t.Fatal("revoked fence")
	}
	if calls.Load() != 0 {
		t.Fatal("invalid write reached network")
	}
}
