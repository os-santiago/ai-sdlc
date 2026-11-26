package mergewriter

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/forge"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/mergegate"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const repository = "os-santiago/homedir"

func expected() mergegate.Expected {
	return mergegate.Expected{Repository: repository, PullRequest: 42, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), TreeSHA: strings.Repeat("c", 40), Branch: "ai-sdlc/issue-3-" + strings.Repeat("d", 32)}
}
func fixture(t *testing.T, h http.HandlerFunc) (*Writer, context.Context) {
	return fixtureFor(t, h, repository)
}
func fixtureFor(t *testing.T, h http.HandlerFunc, repository string) (*Writer, context.Context) {
	t.Helper()
	s := httptest.NewTLSServer(h)
	t.Cleanup(s.Close)
	w, e := New(forge.Credential{Version: 1, Mode: "gh-cli", Host: "github.com", Repository: repository, ActorID: 1, ActorLogin: "fixture", Token: strings.Repeat("x", 40)})
	if e != nil {
		t.Fatal(e)
	}
	transport := w.http.Transport.(*http.Transport)
	transport.TLSClientConfig.RootCAs = s.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	w.base = s.URL
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	t.Cleanup(cancel)
	return w, ctx
}
func commitResponse() map[string]any {
	e := expected()
	return map[string]any{"sha": strings.Repeat("e", 40), "tree": map[string]string{"sha": e.TreeSHA}, "parents": []map[string]string{{"sha": e.BaseSHA}, {"sha": e.HeadSHA}}, "message": "fix(ai-sdlc): integrate issue #3 [skip release]\n\nRefs #3"}
}
func TestMergeSingleHeadConditionedWriteAndVerifiedCommit(t *testing.T) {
	var puts, gets atomic.Int32
	w, ctx := fixture(t, func(out http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("x", 40) || !r.Close {
			t.Error("credential or non-replayable connection")
		}
		if r.Method == http.MethodPut {
			puts.Add(1)
			if r.URL.Path != "/repos/os-santiago/homedir/pulls/42/merge" {
				t.Error("path")
			}
			var body map[string]string
			if json.NewDecoder(r.Body).Decode(&body) != nil || len(body) != 4 || body["sha"] != expected().HeadSHA || body["merge_method"] != "merge" || !strings.HasSuffix(body["commit_title"], "[skip release]") {
				t.Error("unbounded write")
			}
			// No personal or other identity is attested in the merge commit.
			if body["commit_message"] != "Refs #3" || strings.Contains(body["commit_message"], "Signed-off-by") {
				t.Errorf("merge commit message %q", body["commit_message"])
			}
			_ = json.NewEncoder(out).Encode(map[string]any{"merged": true, "sha": strings.Repeat("e", 40)})
		} else {
			gets.Add(1)
			if r.Method != "GET" || r.URL.Path != "/repos/os-santiago/homedir/git/commits/"+strings.Repeat("e", 40) {
				t.Error("unbounded read")
			}
			_ = json.NewEncoder(out).Encode(commitResponse())
		}
	})
	calls := 0
	digest := ""
	receipt, e := w.Merge(ctx, expected(), 3, func(_ context.Context, d string) error {
		calls++
		if calls == 1 {
			digest = d
			if puts.Load() != 0 {
				t.Error("effect before intent")
			}
		} else if d != digest || puts.Load() != 1 || gets.Load() != 1 {
			t.Error("post-effect fence")
		}
		return nil
	})
	if e != nil || calls != 2 || puts.Load() != 1 || gets.Load() != 1 || receipt.CommitSHA != strings.Repeat("e", 40) || receipt.HeadSHA != expected().HeadSHA {
		t.Fatal("merge receipt", e)
	}
	// The journaled intent binds exactly the identity-free request body.
	body, _ := json.Marshal(struct {
		SHA     string `json:"sha"`
		Method  string `json:"merge_method"`
		Title   string `json:"commit_title"`
		Message string `json:"commit_message"`
	}{expected().HeadSHA, "merge", "fix(ai-sdlc): integrate issue #3 [skip release]", "Refs #3"})
	binding, _ := json.Marshal(struct {
		Method, Path string
		Body         json.RawMessage
	}{http.MethodPut, "/repos/os-santiago/homedir/pulls/42/merge", body})
	if digest != authority.Digest(binding) {
		t.Fatal("fence digest does not bind the exact merge request")
	}
}
func TestMergeRejectsInvalidOrUnboundedAuthorityBeforeSend(t *testing.T) {
	var requests atomic.Int32
	w, ctx := fixture(t, func(http.ResponseWriter, *http.Request) { requests.Add(1) })
	fence := func(context.Context, string) error { return nil }
	for _, change := range []func(*mergegate.Expected){func(e *mergegate.Expected) { e.Repository = "os-santiago/saferus" }, func(e *mergegate.Expected) { e.HeadSHA = "main" }, func(e *mergegate.Expected) { e.HeadSHA = e.BaseSHA }, func(e *mergegate.Expected) { e.Branch = "main" }, func(e *mergegate.Expected) { e.Branch = "ai-sdlc/issue-4-" + strings.Repeat("d", 32) }, func(e *mergegate.Expected) { e.TreeSHA = "" }, func(e *mergegate.Expected) { e.PullRequest = 0 }} {
		e := expected()
		change(&e)
		if _, err := w.Merge(ctx, e, 3, fence); err == nil {
			t.Fatal("invalid expected accepted")
		}
	}
	if _, e := w.Merge(context.Background(), expected(), 3, fence); e == nil {
		t.Fatal("unbounded context")
	}
	if _, e := w.Merge(ctx, expected(), 3, nil); e == nil {
		t.Fatal("missing fence")
	}
	if _, e := w.Merge(ctx, expected(), 3, func(context.Context, string) error { return errors.New("revoked") }); e == nil {
		t.Fatal("revoked fence")
	}
	if requests.Load() != 0 {
		t.Fatal("invalid authority sent request")
	}
}
func TestMergeUnknownResponsesNeverRetryOrRedirect(t *testing.T) {
	for _, scenario := range []string{"redirect", "lost-response", "conflict", "accepted-async", "false", "missing", "duplicate", "oversize", "wrong-tree", "wrong-parent", "wrong-message", "post-fence"} {
		t.Run(scenario, func(t *testing.T) {
			var puts atomic.Int32
			w, ctx := fixture(t, func(out http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if r.Method == "PUT" {
					puts.Add(1)
					switch scenario {
					case "redirect":
						out.Header().Set("Location", "/redirect-target")
						out.WriteHeader(307)
						return
					case "lost-response":
						conn, _, e := out.(http.Hijacker).Hijack()
						if e != nil {
							t.Error(e)
							return
						}
						_ = conn.Close()
						return
					case "conflict":
						out.WriteHeader(409)
						return
					case "accepted-async":
						out.WriteHeader(202)
						return
					case "false":
						_, _ = out.Write([]byte(`{"merged":false,"sha":"` + strings.Repeat("e", 40) + `"}`))
						return
					case "missing":
						_, _ = out.Write([]byte(`{"sha":"` + strings.Repeat("e", 40) + `"}`))
						return
					case "duplicate":
						_, _ = out.Write([]byte(`{"merged":false,"merged":true,"sha":"` + strings.Repeat("e", 40) + `"}`))
						return
					case "oversize":
						_, _ = out.Write([]byte(strings.Repeat(" ", 65537)))
						return
					}
					_ = json.NewEncoder(out).Encode(map[string]any{"merged": true, "sha": strings.Repeat("e", 40)})
					return
				}
				commit := commitResponse()
				switch scenario {
				case "wrong-tree":
					commit["tree"] = map[string]string{"sha": strings.Repeat("f", 40)}
				case "wrong-parent":
					commit["parents"] = []map[string]string{{"sha": expected().HeadSHA}, {"sha": expected().BaseSHA}}
				case "wrong-message":
					commit["message"] = "unexpected release"
				}
				_ = json.NewEncoder(out).Encode(commit)
			})
			calls := 0
			receipt, e := w.Merge(ctx, expected(), 3, func(context.Context, string) error {
				calls++
				if scenario == "post-fence" && calls == 2 {
					return ErrMerge
				}
				return nil
			})
			if e == nil || receipt.CommitSHA != "" || puts.Load() != 1 {
				t.Fatal("uncertain write accepted or replayed", e, puts.Load())
			}
			if strings.Contains(e.Error(), strings.Repeat("x", 40)) {
				t.Fatal("credential leaked")
			}
		})
	}
}

func TestSaferusMergeUsesOnlyCredentialRepository(t *testing.T) {
	var calls atomic.Int32
	w, ctx := fixtureFor(t, func(out http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if !strings.HasPrefix(r.URL.Path, "/repos/os-santiago/saferus/") {
			t.Error("cross-repository request")
		}
		if r.Method == http.MethodPut {
			if r.URL.Path != "/repos/os-santiago/saferus/pulls/42/merge" {
				t.Error("merge path")
			}
			json.NewEncoder(out).Encode(map[string]any{"merged": true, "sha": strings.Repeat("e", 40)})
		} else {
			json.NewEncoder(out).Encode(commitResponse())
		}
	}, "os-santiago/saferus")
	want := expected()
	want.Repository = "os-santiago/saferus"
	if _, err := w.Merge(ctx, want, 3, func(context.Context, string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Merge(ctx, expected(), 3, func(context.Context, string) error { return nil }); err != ErrMerge {
		t.Fatal("cross-repo evidence accepted")
	}
	if calls.Load() != 2 {
		t.Fatal("write or observation count")
	}
}
