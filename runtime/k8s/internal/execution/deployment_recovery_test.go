package execution

import (
	"context"
	"errors"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasing"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"testing"
	"time"
)

// Real PostgreSQL and reconstructed controller; API observations are synthetic.
// This cannot stand in for a real Kubernetes Pod-restart demonstration.
func TestDeploymentRecoveryUsesOnlyOriginalAcknowledgedEffects(t *testing.T) {
	for _, stage := range []string{"claimed", "intent", "sent", "advance-ack", "advance-frozen", "advance-policy-missing", "rollback-intent", "rollback-sent", "rollback-ack", "target-replaced", "generation-replaced", "expired"} {
		t.Run(stage, func(t *testing.T) {
			f := newDeploymentFixture(t)
			ctx := context.Background()
			claim := f.claim(t)
			r, transport := deploymentRuntime(f, t, "success")
			if stage == "intent" {
				if e := f.j.BeginDeploymentEffect(ctx, claim, f.before(), false); e != nil {
					t.Fatal(e)
				}
			}
			if stage != "claimed" && stage != "intent" {
				advance := f.effect(t, claim, f.before(), false)
				transport.current = advance.After
				transport.last = advance
				if stage != "sent" {
					if e := f.j.RecordDeploymentReceipt(ctx, claim, advance); e != nil {
						t.Fatal(e)
					}
				}
				if stage == "rollback-intent" {
					if e := f.j.BeginDeploymentEffect(ctx, claim, advance.After, true); e != nil {
						t.Fatal(e)
					}
				}
				if stage == "rollback-sent" || stage == "rollback-ack" {
					rolled := f.effect(t, claim, advance.After, true)
					transport.current = rolled.After
					transport.last = rolled
					if stage == "rollback-ack" {
						if e := f.j.RecordDeploymentReceipt(ctx, claim, rolled); e != nil {
							t.Fatal(e)
						}
					}
				}
			}
			switch stage {
			case "advance-frozen":
				f.p.cfg.Provenance.Build.Inner.Freeze = true
				f.p.cfg.Provenance.Build.Outer.Freeze = true
			case "advance-policy-missing":
				r.Load = func() (releasing.Configuration, error) {
					return releasing.Configuration{}, errors.New("missing current advance mount")
				}
			case "target-replaced":
				f.p.cfg.Target.ServiceUID = store.NewID()
			case "generation-replaced":
				transport.current.Generation++
			case "expired":
				if _, e := f.p.b.f.Admin.Pool.Exec(`UPDATE deployment_requests SET expires_at=clock_timestamp()-interval '1 second'`); e != nil {
					t.Fatal(e)
				}
			}
			state, e := r.PollOnce(ctx)
			success := stage == "advance-ack" || stage == "advance-frozen" || stage == "advance-policy-missing" || stage == "rollback-ack"
			if success {
				if e != nil || state != "deployment-rolled-back" {
					t.Fatal("known acknowledged recovery failed", state, e)
				}
			} else if stage != "expired" && e == nil {
				t.Fatal("uncertain recovery accepted", state)
			}
			if transport.applies != 0 {
				t.Fatal("recovery advanced")
			}
			expectedRollbacks := 0
			if success && stage != "rollback-ack" {
				expectedRollbacks = 1
			}
			if transport.rollbacks != expectedRollbacks {
				t.Fatal("replayed or missing rollback", transport.rollbacks)
			}
			var stored, token string
			var expires time.Time
			if e := f.p.b.f.Admin.Pool.QueryRow(`SELECT state,claim_token::text,expires_at FROM deployment_requests WHERE id=$1`, claim.ID).Scan(&stored, &token, &expires); e != nil {
				t.Fatal(e)
			}
			expected := "quarantined"
			if success {
				expected = "rolled-back"
			}
			if stored != expected || token != claim.Token || stage != "expired" && !expires.Equal(claim.Until) {
				t.Fatal("claim changed or outcome missing", stored)
			}
			if success {
				record, e := f.j.ReadDeploymentRecord(ctx, claim)
				if e != nil || record.Outcome != "rolled-back" {
					t.Fatal("recovered fact unavailable", e)
				}
			}
			_, _ = r.PollOnce(ctx)
			if transport.applies != 0 || transport.rollbacks != expectedRollbacks {
				t.Fatal("terminal recovery replayed")
			}
		})
	}
}
