package execution

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
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

func TestSourcePreflightRetiresRevokedHeadAndSelectsNextRequest(t *testing.T) {
	f := queuedPreparationFixture(t)
	ctx := context.Background()
	first := f.command.ID()
	f.seedReadinessSnapshot(t, f.Snapshot(2, true))
	f.command.Generation = 2
	addQueuedWorkItem(t, f)
	if _, e := f.accept(f.envelope(t, f.command)); e != nil {
		t.Fatal(e)
	}
	head, e := f.db.NextExecution(ctx)
	if e != nil || head != first {
		t.Fatal("wrong initial queue head")
	}
	calls := 0
	_, e = acquireQueuedSource(ctx, f.db, f.load, head, func(context.Context, string, string) (workspacebundle.Bundle, error) {
		calls++
		return workspacebundle.Bundle{}, nil
	})
	var revoked revokedSourceAuthority
	if !errors.As(e, &revoked) || calls != 0 {
		t.Fatal("revoked generation fetched source")
	}
	next, e := f.db.NextExecution(ctx)
	if e != nil || next != f.command.ID() {
		t.Fatal("revoked request blocked later valid work")
	}
	if _, e = acquireQueuedSource(ctx, f.db, f.load, next, f.source); e != nil {
		t.Fatal(e)
	}
	if _, e = Claim(ctx, f.db, f.readyPolicy, next); e != nil {
		t.Fatal(e)
	}
	if f.Count(t, `SELECT count(*) FROM execution_requests WHERE state='stopped' AND claim_token IS NULL`) != 1 {
		t.Fatal("old request not retired without a claim")
	}
}
func TestSourcePreflightObservationFailurePreservesTwoQueuedRequests(t *testing.T) {
	for _, kind := range []string{"source", "loader", "observation", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			f := queuedPreparationFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			first := f.command.ID()
			addQueuedWorkItem(t, f)
			if _, e := f.accept(f.envelope(t, f.command)); e != nil {
				t.Fatal(e)
			}
			before, e := f.db.ReadExecution(ctx, first)
			if e != nil {
				t.Fatal(e)
			}
			load := f.load
			fetch := f.source
			switch kind {
			case "source":
				fetch = func(context.Context, string, string) (workspacebundle.Bundle, error) {
					return workspacebundle.Bundle{}, errors.New("source unavailable")
				}
			case "loader":
				load = func(context.Context) (PreparationInputs, error) {
					return PreparationInputs{}, store.ErrObservationUnavailable
				}
			case "observation":
				if _, e := f.Admin.Pool.Exec(`REVOKE SELECT ON intake_heads FROM aisdlc_worker; REVOKE SELECT(repository,issue_number,generation,content_key) ON intake_heads FROM aisdlc_worker`); e != nil {
					t.Fatal(e)
				}
			case "cancel":
				cancel()
			}
			if _, e = acquireQueuedSource(ctx, f.db, load, first, fetch); e == nil {
				t.Fatal("unavailable observation accepted")
			}
			after, e := f.db.ReadExecution(context.Background(), first)
			if e != nil || !before.ExpiresAt.Equal(after.ExpiresAt) {
				t.Fatal("original expiry changed")
			}
			if f.Count(t, `SELECT count(*) FROM execution_requests WHERE state='queued' AND claim_token IS NULL`) != 2 {
				t.Fatal("temporary outage retired a request")
			}
		})
	}
}

func addQueuedWorkItem(t *testing.T, f *preparationFixture) {
	t.Helper()
	var plan string
	if e := f.Admin.Pool.QueryRow(`SELECT plan_id FROM work_items WHERE uuid=$1`, f.command.WorkItemID).Scan(&plan); e != nil {
		t.Fatal(e)
	}
	f.command.WorkItemID = store.NewID()
	if e := store.UpsertWorkItem(context.Background(), f.Admin.Pool, store.WorkItemRecord{PlanID: plan, ID: "second", UUID: f.command.WorkItemID, IssueNumbers: []int{3}, AcceptanceCriteria: []string{"fixture"}, Status: "ready"}); e != nil {
		t.Fatal(e)
	}
	f.seedReadinessSnapshot(t, f.Snapshot(f.command.Generation, true))
}
