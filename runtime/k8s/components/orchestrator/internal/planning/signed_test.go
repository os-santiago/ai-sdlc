package planning

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/delivery"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/intake"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/testsupport"
)

func TestSignedShadowPlansAreDurableAndNeverDispatch(t *testing.T) {
	f := testsupport.New(t)
	component := f.Components["orchestrator"]
	in := delivery.Inbox{DB: component.DB, Target: "orchestrator"}
	effect := signedEffect(component, f.Config)
	apply := func(generation int64, eligible bool) error {
		decision := "accepted"
		if !eligible {
			decision = "rejected"
		}
		admission := intake.Admission{Snapshot: f.Snapshot(generation, eligible), Decision: decision, Risk: "low", Reasons: []string{"Fixture decision."}, RequiredGates: []string{"tests"}, PolicyVersion: "1.0.0"}
		envelope := f.Envelope(t, "admission-controller", admission.ID(), "plan-admission", "/internal/plan-admission", admission)
		_, _, err := in.Accept(context.Background(), f.Material.Policy, envelope, "plan-admission", http.MethodPost, "/internal/plan-admission", time.Now(), effect)
		return err
	}
	for _, step := range []struct {
		generation int64
		eligible   bool
	}{{2, true}, {1, true}, {2, true}, {3, false}, {4, true}} {
		if err := apply(step.generation, step.eligible); err != nil {
			t.Fatal(err)
		}
	}
	if f.Count(t, `SELECT count(*) FROM plans`) != 2 || f.Count(t, `SELECT count(*) FROM work_items WHERE status='blocked'`) != 2 || f.Count(t, `SELECT generation FROM planning_heads`) != 4 {
		t.Fatal("shadow plan reconciliation mismatch")
	}
	if f.Count(t, `SELECT count(*) FROM authorized_outbox WHERE producer='orchestrator'`) != 0 {
		t.Fatal("shadow dispatched implementation")
	}
}
