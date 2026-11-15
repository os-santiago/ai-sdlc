package execution

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

// This fixture enters through the signed inbox and never manufactures a claim.
func queuedPreparationFixture(t *testing.T) *preparationFixture {
	f := newFixture(t)
	adev := []byte("bounded source availability fixture\n")
	profile, _ := json.Marshal(executor.Profile{Version: executor.ProfileVersion, Model: "fixture", MaxSteps: 4, MaxSeconds: 30, MaxTokens: 1024, ADevSHA256: workspacebundle.Digest(adev)})
	f.command.ProfileSHA256 = workspacebundle.Digest(profile)
	if _, e := f.accept(f.envelope(t, f.command)); e != nil {
		t.Fatal(e)
	}
	return &preparationFixture{fixture: f, inputs: PreparationInputs{Policy: f.Material.Policy, Intake: f.Config, Profile: profile, ADev: adev}}
}
func TestSourcePreflightUnavailableNeverClaimsOrExtendsRequest(t *testing.T) {
	f := queuedPreparationFixture(t)
	ctx := context.Background()
	before, e := f.db.ReadExecution(ctx, f.command.ID())
	if e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 3; i++ {
		_, e = preflightSource(ctx, f.db, f.load, f.command.ID(), func(context.Context, string, string) (workspacebundle.Bundle, error) {
			return workspacebundle.Bundle{}, errors.New("unavailable")
		})
		if DiagnosticStage(e) != "source-unavailable" {
			t.Fatalf("unexpected diagnostic %v", e)
		}
	}
	after, e := f.db.ReadExecution(ctx, f.command.ID())
	if e != nil || !after.ExpiresAt.Equal(before.ExpiresAt) {
		t.Fatal("preflight changed expiration")
	}
	if f.Count(t, `SELECT count(*) FROM execution_requests WHERE state='queued' AND claim_token IS NULL`) != 1 || f.Count(t, `SELECT count(*) FROM model_calls`) != 0 || f.Count(t, `SELECT count(*) FROM execution_effects`) != 0 {
		t.Fatal("source outage acquired execution authority or effects")
	}
	source, e := preflightSource(ctx, f.db, f.load, f.command.ID(), f.source)
	if e != nil {
		t.Fatal(e)
	}
	claim, e := Claim(ctx, f.db, f.readyPolicy, f.command.ID())
	if e != nil {
		t.Fatal(e)
	}
	if _, e = prepare(ctx, f.db, f.load, claim, func(context.Context, string, string) (workspacebundle.Bundle, error) { return source.bundle, nil }); e != nil {
		t.Fatal(e)
	}
}
func TestSourcePreflightRejectsAuthorityChangesDuringRead(t *testing.T) {
	for _, which := range []string{"generation", "policy", "profile", "adev", "config", "source", "cancel"} {
		t.Run(which, func(t *testing.T) {
			f := queuedPreparationFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			_, e := preflightSource(ctx, f.db, f.load, f.command.ID(), func(ctx context.Context, repo, sha string) (workspacebundle.Bundle, error) {
				b, e := f.source(ctx, repo, sha)
				switch which {
				case "generation":
					if _, e := f.Admin.Pool.Exec(`UPDATE intake_heads SET generation=2`); e != nil {
						t.Fatal(e)
					}
				case "policy":
					f.inputs.Policy.Freeze = true
				case "profile":
					f.inputs.Profile = append(f.inputs.Profile, ' ')
				case "adev":
					f.inputs.ADev = append(f.inputs.ADev, ' ')
				case "config":
					f.inputs.Intake.TriggerLabel = "changed"
				case "source":
					b.SourceSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
				case "cancel":
					cancel()
				}
				return b, e
			})
			if e == nil {
				t.Fatal("source acquisition accepted changed authority")
			}
			if f.Count(t, `SELECT count(*) FROM execution_requests WHERE state='queued' AND claim_token IS NULL`) != 1 {
				t.Fatal("read-only preflight changed request")
			}
		})
	}
}
