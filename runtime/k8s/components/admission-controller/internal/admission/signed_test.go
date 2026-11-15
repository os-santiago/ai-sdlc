package admission

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/contract"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/delivery"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/intake"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/testsupport"
)

func TestSignedAdmissionIgnoresOldGenerationsAndRejectsWrongBindings(t *testing.T) {
	f := testsupport.New(t)
	component := f.Components["admission-controller"]
	trusted := contract.Default()
	max := 8
	trusted.Triggers.AcceptanceCriteria.Max = &max
	in := delivery.Inbox{DB: component.DB, Target: "admission-controller"}
	effect := signedEffect(component, f.Config, trusted)
	apply := func(snapshot intake.Snapshot) error {
		envelope := f.Envelope(t, "intake", snapshot.ID(), "admit-issue", "/internal/admit-issue", snapshot)
		_, _, err := in.Accept(context.Background(), f.Material.Policy, envelope, "admit-issue", http.MethodPost, "/internal/admit-issue", time.Now(), effect)
		return err
	}
	for _, snapshot := range []intake.Snapshot{f.Snapshot(2, true), f.Snapshot(1, true), f.Snapshot(2, true), f.Snapshot(3, false)} {
		if err := apply(snapshot); err != nil {
			t.Fatal(err)
		}
	}
	if f.Count(t, `SELECT count(*) FROM admissions`) != 2 || f.Count(t, `SELECT count(*) FROM authorized_outbox WHERE producer='admission-controller'`) != 2 || f.Count(t, `SELECT generation FROM admission_heads`) != 3 {
		t.Fatal("stale/repeated receipt changed admission state")
	}
	wrong := f.Snapshot(4, true)
	wrong.Repository = "other/project"
	if err := apply(wrong); err == nil {
		t.Fatal("wrong payload scope accepted")
	}
	if f.Count(t, `SELECT generation FROM admission_heads`) != 3 {
		t.Fatal("rejected receipt advanced generation")
	}
}
