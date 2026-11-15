package planning

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/delivery"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/execution"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/intake"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/testsupport"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

func TestCandidatePlanningEmitsOnlyBoundSignedExecution(t *testing.T) {
	for _, risk := range []string{"low", "medium"} {
		t.Run(risk, func(t *testing.T) {
			f := testsupport.New(t)
			t.Setenv("AI_SDLC_RUNTIME_MODE", "candidate")
			f.Material.Policy.Autonomy = "auto-pr"
			var grants []authority.Grant
			for _, g := range f.Material.Policy.Grants {
				if g.Operation == "admit-issue" || g.Operation == "plan-admission" || g.Operation == "execute-work" {
					grants = append(grants, g)
				}
			}
			f.Material.Policy.Grants = grants
			component := f.Components["orchestrator"]
			raw, _ := json.Marshal(f.Material.Policy)
			if e := os.WriteFile(component.PolicyPath, raw, 0600); e != nil {
				t.Fatal(e)
			}
			c := execution.RuntimeConfig{Version: "ai-sdlc.candidate-runtime/v1", ADev: "doctrine", Profile: executor.Profile{Version: executor.ProfileVersion, Model: "codex-cli", MaxSteps: 4, MaxSeconds: 60, MaxTokens: 1024, ADevSHA256: workspacebundle.Digest([]byte("doctrine"))}, Placement: execution.Placement{Namespace: "ai-sdlc-executors", WorkerImage: "example/worker@sha256:" + strings.Repeat("a", 64), MailboxImage: "example/mailbox@sha256:" + strings.Repeat("b", 64)}, Model: execution.ModelPolicy{Limits: store.ModelLimits{Calls: 4, InputBytes: 131072, OutputBytes: 1048576}, ResponseLimit: 262144}}
			path := filepath.Join(t.TempDir(), "runtime.json")
			raw, _ = json.Marshal(c)
			if e := os.WriteFile(path, raw, 0600); e != nil {
				t.Fatal(e)
			}
			t.Setenv("AI_SDLC_EXECUTION_CONFIG_PATH", path)
			admission := intake.Admission{Snapshot: f.Snapshot(1, true), Decision: "accepted", Risk: risk, Reasons: []string{"fixture"}, RequiredGates: []string{"tests"}, PolicyVersion: "1.0.0"}
			if risk != "low" {
				admission.Decision = "needs-human"
			}
			in := delivery.Inbox{DB: component.DB, Target: "orchestrator"}
			for n := 0; n < 2; n++ {
				envelope := f.Envelope(t, "admission-controller", admission.ID(), "plan-admission", "/internal/plan-admission", admission)
				_, duplicate, e := in.Accept(context.Background(), f.Material.Policy, envelope, "plan-admission", http.MethodPost, "/internal/plan-admission", time.Now(), signedEffect(component, f.Config))
				if e != nil || duplicate != (n == 1) {
					t.Fatalf("planning replay %v %v", duplicate, e)
				}
			}
			count := f.Count(t, `SELECT count(*) FROM authorized_outbox WHERE operation='execute-work'`)
			if risk != "low" {
				if count != 0 || f.Count(t, `SELECT count(*) FROM work_items`) != 0 {
					t.Fatal("non-low admission dispatched")
				}
				return
			}
			if count != 1 || f.Count(t, `SELECT count(*) FROM work_items WHERE status='ready'`) != 1 {
				t.Fatal("missing unique dispatch")
			}
			var payload, receipt []byte
			if e := f.Admin.Pool.QueryRow(`SELECT payload,receipt FROM authorized_outbox WHERE operation='execute-work'`).Scan(&payload, &receipt); e != nil {
				t.Fatal(e)
			}
			var r authority.Receipt
			if authority.DecodeStrict(receipt, &r) != nil {
				t.Fatal("receipt")
			}
			command, e := execution.Verify(f.Material.Policy, r, payload, time.Now())
			if e != nil || command.SourceSHA != admission.Snapshot.BaseSHA || command.Generation != 1 || command.Attempt != 1 || command.AdmissionID != admission.ID() {
				t.Fatal("dispatch lost signed bindings", e)
			}
		})
	}
}
