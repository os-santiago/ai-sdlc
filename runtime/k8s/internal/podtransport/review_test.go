package podtransport

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelmailbox"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/reviewresult"
	core "k8s.io/api/core/v1"
)

func TestReviewExportBindsRoleTerminationIdentityAndFixedHelper(t *testing.T) {
	for _, kind := range []string{"valid", "generator", "running", "zero-exit", "failed-exit", "replaced-after", "changed-exit", "bad-summary", "oversized", "no-status"} {
		t.Run(kind, func(t *testing.T) {
			b, original, _ := workspaceBinding(t)
			var p executor.Profile
			json.Unmarshal(b.Profile, &p)
			if kind != "generator" {
				p.Version = executor.ReviewProfileVersion
			}
			p.MaxSteps, p.MaxSeconds = 4, 90
			b.Profile, _ = json.Marshal(p)
			a, e := executor.ParseAssignment(b.Assignment, modelmailbox.Digest(b.Assignment), time.Now())
			if e != nil {
				t.Fatal(e)
			}
			a.ProfileSHA256 = modelmailbox.Digest(b.Profile)
			b.Assignment, _ = json.Marshal(a)
			w, e := executor.Compile(b.Namespace, b.WorkerImage, b.MailboxImage, b.Assignment, b.Profile, b.ADev, time.Now())
			if e != nil {
				t.Fatal(e)
			}
			raw, _ := json.Marshal(w.Pod)
			var pod core.Pod
			json.Unmarshal(raw, &pod)
			applyDefaults(&pod.Spec)
			pod.Spec.NodeName, pod.UID, pod.Status = original.Spec.NodeName, original.UID, original.Status
			summary, _ := json.Marshal(map[string]any{"v": 1, "success": false, "model": p.Model, "exit_reason": "no_changes", "iterations": 2, "tool_calls_total": 1, "tool_calls": map[string]int{"read_file": 1}, "phases": []any{map[string]any{"role": "reviewer", "provider": "custom", "model": p.Model, "iterations": 2}}, "review": map[string]any{"verdict": "approve", "explicit": true, "fix_rounds": 0, "max_fixes": 0}, "final_message": "Inspected source.\nVERDICT: approve", "error": nil})
			session, calls := workspaceSession(t, b, pod, func(p *core.Pod, n int32) {
				if n >= 2 && kind != "running" {
					code := int32(10)
					if kind == "zero-exit" {
						code = 0
					}
					if kind == "failed-exit" || n == 3 && kind == "changed-exit" {
						code = 1
					}
					p.Status.ContainerStatuses[0].State = core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: code}}
				}
				if n == 3 && kind == "replaced-after" {
					p.UID = "33333333-3333-4333-8333-333333333333"
				}
			}, func(r *http.Request, conn *websocket.Conn) {
				want := []string{"/workspace-transfer", "review", "--pod-uid", b.PodUID, "--source-sha", strings.Repeat("a", 40)}
				if !reflect.DeepEqual(r.URL.Query()["command"], want) || r.URL.Query().Get("stdin") != "" {
					t.Error("arbitrary review helper")
				}
				out := summary
				if kind == "bad-summary" {
					out = bytes.Replace(out, []byte(`"explicit":true`), []byte(`"explicit":false`), 1)
				}
				if kind == "oversized" {
					out = bytes.Repeat([]byte("x"), reviewresult.MaxBytes+1)
				}
				workspaceReply(conn, out, kind != "no-status")
			})
			got, e := session.ExportReview(context.Background())
			if (e == nil) != (kind == "valid") {
				t.Fatalf("unexpected export %v", e)
			}
			if e == nil {
				if !bytes.Equal(got.Bytes(), summary) || got.Result().Verdict != "approve" {
					t.Fatal("export differs")
				}
				copy := got.Bytes()
				copy[0] = 'x'
				if bytes.Equal(copy, got.Bytes()) {
					t.Fatal("mutable export")
				}
			}
			want := int32(1)
			if kind == "generator" || kind == "running" || kind == "zero-exit" || kind == "failed-exit" {
				want = 0
			}
			if calls.Load() != want {
				t.Fatal("unexpected helper invocation/replay")
			}
		})
	}
}
