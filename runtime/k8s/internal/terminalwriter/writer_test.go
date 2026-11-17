package terminalwriter

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/forge"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasecompletion"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func plan() releasecompletion.Plan {
	p := releasecompletion.Plan{Version: "ai-sdlc.terminal-release-plan/v1", CommandID: "complete-" + strings.Repeat("a", 64), Repository: "os-santiago/saferus", IssueNumber: 4, Generation: 1, IssueRevision: strings.Repeat("b", 64), SourceSHA: strings.Repeat("c", 40), TreeSHA: strings.Repeat("d", 40), Image: "registry.example/saferus@sha256:" + strings.Repeat("e", 64), TargetSHA256: strings.Repeat("f", 64), DeploymentRecordSHA256: strings.Repeat("1", 64), ProvenanceRecordSHA256: strings.Repeat("2", 64), BuildRecordSHA256: strings.Repeat("3", 64), Title: "AI-SDLC kind test production: issue #4", ExpiresAt: time.Now().Add(2 * time.Minute).UnixMilli()}
	p.Tag = "ai-sdlc/" + p.CommandID
	p.Body = p.BodyText()
	return p
}
func fixture(t *testing.T, h http.HandlerFunc) (*Writer, context.Context) {
	t.Helper()
	s := httptest.NewTLSServer(h)
	t.Cleanup(s.Close)
	w, e := New(forge.Credential{Version: 1, Mode: "gh-cli", Host: "github.com", Repository: "os-santiago/saferus", ActorID: 1, ActorLogin: "fixture", Token: strings.Repeat("x", 40)})
	if e != nil {
		t.Fatal(e)
	}
	w.http.Transport.(*http.Transport).TLSClientConfig.RootCAs = s.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	w.base = s.URL
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return w, ctx
}
func refResponse(p releasecompletion.Plan) any {
	return map[string]any{"ref": "refs/tags/" + p.Tag, "object": map[string]string{"type": "commit", "sha": p.SourceSHA}}
}
func releaseResponse(p releasecompletion.Plan, draft bool) map[string]any {
	r := map[string]any{"id": int64(37), "tag_name": p.Tag, "target_commitish": p.SourceSHA, "name": p.Title, "body": p.Body, "draft": draft, "prerelease": false, "html_url": "https://github.com/" + p.Repository + "/releases/tag/" + p.Tag, "published_at": time.Now().UTC()}
	if draft {
		r["published_at"] = nil
		r["html_url"] = "https://github.com/" + p.Repository + "/releases/untagged-fixture"
	}
	return r
}
func TestTerminalSingleSendFourStepsBoundToExactSource(t *testing.T) {
	for _, step := range []string{"tag", "draft", "publish", "close"} {
		t.Run(step, func(t *testing.T) {
			p := plan()
			id := int64(0)
			if step == "publish" || step == "close" {
				id = 37
			}
			var writes, fences atomic.Int32
			w, ctx := fixture(t, func(out http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("x", 40) || !r.Close {
					t.Error("transport")
				}
				var value any
				if r.Method != "GET" {
					writes.Add(1)
					if fences.Load() != 1 {
						t.Error("effect before durable fence")
					}
					expected, e := releasecompletion.Request(p, step, id, time.Now())
					if e != nil || r.Method != expected.Method || r.URL.Path != expected.Path {
						t.Error("effect binding")
					}
					var raw json.RawMessage
					if json.NewDecoder(r.Body).Decode(&raw) != nil || string(raw) != string(expected.Body) {
						t.Error("body")
					}
					switch step {
					case "tag":
						value = refResponse(p)
						out.WriteHeader(201)
					case "draft":
						value = releaseResponse(p, true)
						out.WriteHeader(201)
					case "publish":
						value = releaseResponse(p, false)
					case "close":
						value = map[string]any{"id": 51, "number": 4, "state": "closed", "state_reason": "completed"}
					}
				} else if strings.Contains(r.URL.Path, "/git/ref/") {
					value = refResponse(p)
				} else if strings.Contains(r.URL.Path, "/releases/") {
					value = releaseResponse(p, step == "publish")
				} else if strings.HasSuffix(r.URL.Path, "/issues/4") {
					value = map[string]any{"id": 51, "number": 4, "state": "open"}
				} else {
					t.Error("unexpected read")
				}
				_ = json.NewEncoder(out).Encode(value)
			})
			sent := time.Now()
			r, e := w.Execute(ctx, p, step, id, func(_ context.Context, q releasecompletion.EffectRequest) error {
				fences.Add(1)
				if writes.Load() != 0 {
					t.Error("late fence")
				}
				expected, _ := releasecompletion.Request(p, step, id, time.Now())
				if q.Digest() != expected.Digest() {
					t.Error("request digest")
				}
				return nil
			})
			if e != nil || writes.Load() != 1 || fences.Load() != 1 || releasecompletion.ValidateReceipt(p, mustRequest(t, p, step, id, sent), id, r, sent, time.Now()) != nil {
				t.Fatalf("receipt denied: %v writes=%d", e, writes.Load())
			}
		})
	}
}
func mustRequest(t *testing.T, p releasecompletion.Plan, step string, id int64, now time.Time) releasecompletion.EffectRequest {
	t.Helper()
	q, e := releasecompletion.Request(p, step, id, now)
	if e != nil {
		t.Fatal(e)
	}
	return q
}
func TestTerminalAmbiguousACKNeverRetriesOrAdopts(t *testing.T) {
	for _, kind := range []string{"disconnect", "existing", "redirect", "duplicate-json", "wrong-sha"} {
		t.Run(kind, func(t *testing.T) {
			p := plan()
			var writes atomic.Int32
			w, ctx := fixture(t, func(out http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" {
					t.Error("adoption read")
					http.Error(out, "denied", 500)
					return
				}
				writes.Add(1)
				switch kind {
				case "disconnect":
					c, _, _ := out.(http.Hijacker).Hijack()
					_ = c.Close()
				case "existing":
					http.Error(out, "exists", 422)
				case "redirect":
					out.Header().Set("Location", "/unexpected")
					out.WriteHeader(307)
				case "duplicate-json":
					out.WriteHeader(201)
					_, _ = out.Write([]byte(`{"ref":"wrong","ref":"refs/tags/` + p.Tag + `","object":{"type":"commit","sha":"` + p.SourceSHA + `"}}`))
				case "wrong-sha":
					out.WriteHeader(201)
					v := p
					v.SourceSHA = strings.Repeat("9", 40)
					_ = json.NewEncoder(out).Encode(refResponse(v))
				}
			})
			if _, e := w.Execute(ctx, p, "tag", 0, func(context.Context, releasecompletion.EffectRequest) error { return nil }); e == nil || writes.Load() != 1 {
				t.Fatal("ambiguous effect accepted/retried", e, writes.Load())
			}
		})
	}
}
func TestTerminalRejectsFenceAndChangedTagBeforePublish(t *testing.T) {
	for _, mode := range []string{"fence", "tag", "draft", "closed-issue"} {
		t.Run(mode, func(t *testing.T) {
			p := plan()
			var writes atomic.Int32
			step := "publish"
			if mode == "closed-issue" {
				step = "close"
			}
			w, ctx := fixture(t, func(out http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					writes.Add(1)
					http.Error(out, "unexpected", 500)
					return
				}
				var v any
				if strings.Contains(r.URL.Path, "/git/ref/") {
					q := p
					if mode == "tag" {
						q.SourceSHA = strings.Repeat("8", 40)
					}
					v = refResponse(q)
				} else if strings.Contains(r.URL.Path, "/releases/") {
					q := releaseResponse(p, step == "publish")
					if mode == "draft" {
						q["body"] = "tampered"
					}
					v = q
				} else {
					v = map[string]any{"id": 51, "number": 4, "state": "closed", "state_reason": "completed"}
				}
				_ = json.NewEncoder(out).Encode(v)
			})
			if _, e := w.Execute(ctx, p, step, 37, func(context.Context, releasecompletion.EffectRequest) error {
				if mode == "fence" {
					return errors.New("withdrawn")
				}
				t.Error("invalid preflight reached send fence")
				return nil
			}); e == nil || writes.Load() != 0 {
				t.Fatal("unauthorized mutation", e)
			}
		})
	}
}
func TestTerminalRejectsUnboundedOrExpiredPlans(t *testing.T) {
	p := plan()
	var requests atomic.Int32
	w, ctx := fixture(t, func(http.ResponseWriter, *http.Request) { requests.Add(1) })
	f := func(context.Context, releasecompletion.EffectRequest) error { return nil }
	if _, e := w.Execute(context.Background(), p, "tag", 0, f); e == nil {
		t.Fatal("unbounded")
	}
	p.ExpiresAt = time.Now().Add(-time.Second).UnixMilli()
	if _, e := w.Execute(ctx, p, "tag", 0, f); e == nil || requests.Load() != 0 {
		t.Fatal("expired")
	}
}

func TestTerminalPublishAndCloseLostACKAreNotRetried(t *testing.T) {
	for _, step := range []string{"publish", "close"} {
		t.Run(step, func(t *testing.T) {
			p := plan()
			var writes atomic.Int32
			w, ctx := fixture(t, func(out http.ResponseWriter, r *http.Request) {
				if r.Method == "PATCH" {
					writes.Add(1)
					c, _, _ := out.(http.Hijacker).Hijack()
					_ = c.Close()
					return
				}
				var value any
				if strings.Contains(r.URL.Path, "/git/ref/") {
					value = refResponse(p)
				} else if strings.Contains(r.URL.Path, "/releases/") {
					value = releaseResponse(p, step == "publish")
				} else {
					value = map[string]any{"id": 51, "number": 4, "state": "open"}
				}
				_ = json.NewEncoder(out).Encode(value)
			})
			if _, e := w.Execute(ctx, p, step, 37, func(context.Context, releasecompletion.EffectRequest) error { return nil }); e == nil || writes.Load() != 1 {
				t.Fatal("lost ACK accepted/retried", e, writes.Load())
			}
		})
	}
}
