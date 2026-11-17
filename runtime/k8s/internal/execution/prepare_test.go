package execution

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

type preparationFixture struct {
	*fixture
	claim  store.ExecutionClaim
	mu     sync.Mutex
	inputs PreparationInputs
}

func newPreparationFixture(t *testing.T, configure ...func(*fixture)) *preparationFixture {
	t.Helper()
	f := newFixture(t, configure...)
	adev := []byte("reviewed-fixture: use bounded deterministic validation\n")
	if len(f.adevOverride) > 0 {
		adev = append([]byte(nil), f.adevOverride...)
	}
	seconds, model := 30, "fixture"
	if f.profileSeconds != 0 {
		seconds = f.profileSeconds
	}
	if f.profileModel != "" {
		model = f.profileModel
	}
	profile, _ := json.Marshal(executor.Profile{Version: executor.ProfileVersion, Model: model, MaxSteps: 4, MaxSeconds: seconds, MaxTokens: 1024, MaxRepairs: f.maxRepairs, ADevSHA256: workspacebundle.Digest(adev)})
	f.command.ProfileSHA256 = workspacebundle.Digest(profile)
	if _, err := f.accept(f.envelope(t, f.command)); err != nil {
		t.Fatal(err)
	}
	claim, err := Claim(context.Background(), f.db, f.readyPolicy, f.command.ID())
	if err != nil {
		t.Fatal(err)
	}
	return &preparationFixture{fixture: f, claim: claim, inputs: PreparationInputs{Policy: f.Material.Policy, Intake: f.Config, Profile: profile, ADev: adev}}
}
func (f *preparationFixture) load(context.Context) (PreparationInputs, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return copyInputs(f.inputs), nil
}
func (f *preparationFixture) source(ctx context.Context, repository, sha string) (workspacebundle.Bundle, error) {
	if ctx.Err() != nil || repository != f.command.Repository || sha != f.command.SourceSHA {
		return workspacebundle.Bundle{}, ErrAuthority
	}
	return workspacebundle.Bundle{Version: workspacebundle.Version, SourceSHA: sha, Files: []workspacebundle.File{{Path: "README.md", Mode: 0644, Data: []byte("fixture source\n")}}}, nil
}
func TestPreparationDerivesImmutableInputsFromDurableProof(t *testing.T) {
	f := newPreparationFixture(t)
	p, err := prepare(context.Background(), f.db, f.load, f.claim, f.source)
	if err != nil {
		t.Fatal(err)
	}
	raw := p.Assignment()
	a, err := executor.ParseAssignment(raw, workspacebundle.Digest(raw), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	snapshot := f.Snapshot(1, true)
	if a.Repository != f.command.Repository || a.WorkItemID != f.command.WorkItemID || a.SourceSHA != f.command.SourceSHA || a.Title != snapshot.Issue.Title || a.Body != snapshot.Issue.Body {
		t.Fatal("assignment did not derive from verified command and snapshot")
	}
	source, err := workspacebundle.Decode(p.Source())
	if err != nil {
		t.Fatal(err)
	}
	digest, err := source.TreeDigest()
	if err != nil || digest != a.TreeSHA256 {
		t.Fatal("prepared tree binding differs")
	}
	for _, accessor := range []func() []byte{p.Assignment, p.Source, p.Profile, p.ADev} {
		original := accessor()
		changed := accessor()
		changed[0] ^= 1
		if string(accessor()) != string(original) {
			t.Fatal("accessor exposed mutable internal bytes")
		}
	}
	if f.Count(t, `SELECT count(*) FROM execution_requests WHERE state='claimed'`) != 1 {
		t.Fatal("preparation advanced lifecycle")
	}
}
func TestPreparationRejectsChangedAuthorityAcrossSourceIO(t *testing.T) {
	for _, which := range []string{"generation", "profile", "adev", "policy", "config", "expired", "stopped", "source"} {
		t.Run(which, func(t *testing.T) {
			f := newPreparationFixture(t)
			started, release := make(chan struct{}), make(chan struct{})
			result := make(chan error, 1)
			go func() {
				p, err := prepare(context.Background(), f.db, f.load, f.claim, func(ctx context.Context, repo, sha string) (workspacebundle.Bundle, error) {
					close(started)
					select {
					case <-release:
					case <-ctx.Done():
						return workspacebundle.Bundle{}, ctx.Err()
					}
					b, e := f.source(ctx, repo, sha)
					if which == "source" {
						b.SourceSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
					}
					return b, e
				})
				if len(p.Assignment()) != 0 {
					err = nil
				}
				result <- err
			}()
			<-started
			switch which {
			case "generation":
				if _, err := f.Admin.Pool.Exec(`UPDATE intake_heads SET generation=2`); err != nil {
					t.Fatal(err)
				}
			case "expired":
				if _, err := f.Admin.Pool.Exec(`UPDATE execution_requests SET claim_until=clock_timestamp()-interval '1 second'`); err != nil {
					t.Fatal(err)
				}
			case "config":
				f.mu.Lock()
				f.inputs.Intake.TriggerLabel = "changed-trigger"
				f.mu.Unlock()
			case "stopped":
				if err := f.db.StopExecutionClaim(context.Background(), f.claim, "aborted"); err != nil {
					t.Fatal(err)
				}
			case "profile":
				f.mu.Lock()
				f.inputs.Profile = append(f.inputs.Profile, ' ')
				f.mu.Unlock()
			case "adev":
				f.mu.Lock()
				f.inputs.ADev = append(f.inputs.ADev, ' ')
				f.mu.Unlock()
			case "policy":
				f.mu.Lock()
				f.inputs.Policy.Freeze = true
				f.mu.Unlock()
			}
			close(release)
			if err := <-result; err == nil {
				t.Fatal("changed proof returned prepared inputs")
			}
			if f.Count(t, `SELECT count(*) FROM execution_requests WHERE state='stopped'`) != 1 {
				t.Fatal("failed preparation retained an active claim")
			}
		})
	}
}
func TestPreparationRejectsWrongTokenWithoutAffectingOwner(t *testing.T) {
	f := newPreparationFixture(t)
	wrong := f.claim
	wrong.Token = store.NewID()
	calls := 0
	p, err := prepare(context.Background(), f.db, f.load, wrong, func(context.Context, string, string) (workspacebundle.Bundle, error) {
		calls++
		return workspacebundle.Bundle{}, nil
	})
	if err == nil || len(p.Assignment()) != 0 || calls != 0 {
		t.Fatal("wrong token fetched source or returned preparation")
	}
	if f.Count(t, `SELECT count(*) FROM execution_requests WHERE state='claimed'`) != 1 {
		t.Fatal("wrong token stopped owner")
	}
}

func TestPreparationCancellationDuringSourceStopsOnlyOwnedClaim(t *testing.T) {
	f := newPreparationFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		p, err := prepare(ctx, f.db, f.load, f.claim, func(ctx context.Context, repository, sha string) (workspacebundle.Bundle, error) {
			close(started)
			<-ctx.Done()
			return workspacebundle.Bundle{}, ctx.Err()
		})
		if len(p.Assignment()) != 0 {
			err = nil
		}
		result <- err
	}()
	select {
	case <-started:
	case err := <-result:
		t.Fatalf("preparation failed before source cancellation: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("source call did not start")
	}
	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("canceled source produced preparation")
		}
	case <-time.After(4 * time.Second):
		t.Fatal("cancellation did not complete bounded cleanup")
	}
	if f.Count(t, `SELECT count(*) FROM execution_requests WHERE state='stopped'`) != 1 {
		t.Fatal("canceled owner remained active")
	}
}

// This optional integration test uses isolated synthetic issue/admission rows
// and real public source acquisition. It never touches Homedir live workflow.
func TestActualPinnedHomedirPreparationFixture(t *testing.T) {
	if os.Getenv("AI_SDLC_TEST_HOMEDIR_PREPARATION") != "1" {
		t.Skip("explicit public source fixture required")
	}
	f := newFixture(t)
	f.Material.Policy.Repository = "os-santiago/homedir"
	f.Config.Repository = "os-santiago/homedir"
	f.command.Repository = f.Config.Repository
	f.command.AuthorityRevision = f.Material.Policy.Revision()
	snapshot := f.Snapshot(1, true)
	snapshot.BaseSHA = "0622cdaeb59a11a27bd5f3d7e18183c401a5e412"
	snapshot.Revision = snapshot.ContentRevision()
	f.seedReadinessSnapshot(t, snapshot)
	adev, err := os.ReadFile("../../deploy/helm/ai-sdlc-k8s-runtime/files/adev-profile.yaml")
	if err != nil {
		t.Fatal(err)
	}
	profile, _ := json.Marshal(executor.Profile{Version: executor.ProfileVersion, Model: "gpt-6.1-sol", MaxSteps: 8, MaxSeconds: 120, MaxTokens: 8192, ADevSHA256: workspacebundle.Digest(adev)})
	f.command.ProfileSHA256 = workspacebundle.Digest(profile)
	if _, err = f.accept(f.envelope(t, f.command)); err != nil {
		t.Fatal(err)
	}
	claim, err := Claim(context.Background(), f.db, f.readyPolicy, f.command.ID())
	if err != nil {
		t.Fatal(err)
	}
	load := func(context.Context) (PreparationInputs, error) {
		return PreparationInputs{Policy: f.Material.Policy, Intake: f.Config, Profile: profile, ADev: adev}, nil
	}
	start := time.Now()
	prepared, err := Prepare(context.Background(), f.db, load, claim, "")
	if err != nil {
		t.Fatal("actual public source preparation failed", err)
	}
	bundle, err := workspacebundle.Decode(prepared.Source())
	if err != nil {
		t.Fatal(err)
	}
	tree, err := bundle.TreeDigest()
	if err != nil || tree != "a392d4d3be50cc89cb1cc324fb736952fe9e86b47e5d131912163d363c147277" || len(bundle.Files) != 1158 {
		t.Fatal("prepared Homedir differs from independently verified pinned source")
	}
	assignment, err := executor.ParseAssignment(prepared.Assignment(), workspacebundle.Digest(prepared.Assignment()), time.Now())
	if err != nil || assignment.TreeSHA256 != tree || assignment.SourceSHA != snapshot.BaseSHA || assignment.Body != snapshot.Issue.Body {
		t.Fatal("actual preparation lost its verified bindings")
	}
	if f.Count(t, `SELECT count(*) FROM attempts`) != 0 {
		t.Fatal("preparation created execution effects")
	}
	t.Logf("real Homedir preparation %.3fs, %d files, source %s, tree %s; synthetic authority fixture, no live admission or E2E", time.Since(start).Seconds(), len(bundle.Files), snapshot.BaseSHA, tree)
}

func TestPreparationRejectsInPlaceRevocationOnFinalReload(t *testing.T) {
	for _, which := range []string{"key", "grant", "actor"} {
		t.Run(which, func(t *testing.T) {
			f := newPreparationFixture(t)
			shared := f.inputs
			calls := 0
			load := func(context.Context) (PreparationInputs, error) {
				calls++
				if calls == 6 {
					switch which {
					case "key":
						delete(shared.Policy.Keys, f.Material.Identities["orchestrator"].KeyID)
					case "grant":
						for i := range shared.Policy.Grants {
							if shared.Policy.Grants[i].Operation == "execute-work" {
								shared.Policy.Grants[i].MinimumLevel = "auto-deploy"
							}
						}
					case "actor":
						shared.Intake.AuthorizedActorIDs[0] = 99
					}
				}
				return shared, nil
			}
			prepared, err := prepare(context.Background(), f.db, load, f.claim, f.source)
			if calls != 6 {
				t.Fatalf("did not reach final reload: calls=%d error=%v", calls, err)
			}
			if err == nil || len(prepared.Assignment()) != 0 {
				t.Fatal("in-place final revocation returned prepared inputs")
			}
			if f.Count(t, `SELECT count(*) FROM execution_requests WHERE state='stopped'`) != 1 {
				t.Fatal("revoked preparation retained active claim")
			}
		})
	}
}
