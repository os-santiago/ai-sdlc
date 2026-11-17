package execution

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/intake"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

// RepairFixtureForTest bridges external-package flow tests to the existing
// isolated PostgreSQL journal fixture. It is compiled only by go test. Its
// generation/model/behavior effects are synthetic, not Kubernetes evidence.
type RepairFixtureForTest struct {
	Admin, Worker, Verifier, Orchestrator *store.DB
	Policy                                authority.Policy
	Identities                            map[string]authority.Identity
	Config                                intake.Config
	Command                               Command
	Runtime                               RuntimeConfig
	Profile                               []byte
	OriginalSource                        []byte
	Review                                store.ReviewRun
	Prepare                               func(*testing.T, string) (Prepared, *Lifecycle)
	PrepareOnly                           func(*testing.T, string) Prepared
	LoadLifecycle                         LifecycleLoader
}

func NewRepairFixtureForTest(t *testing.T, enabled bool, ttl ...time.Duration) RepairFixtureForTest {
	t.Helper()
	return newRepairFixtureForTest(t, enabled, ttl, nil)
}

func NewKubeRepairFixtureForTest(t *testing.T, placement Placement, adev []byte) RepairFixtureForTest {
	t.Helper()
	return newRepairFixtureForTest(t, true, nil, func(f *fixture) {
		f.adevOverride = append([]byte(nil), adev...)
		f.profileSeconds = 120
		f.profileModel = "codex-cli"
		f.placementOverride = &placement
	})
}
func newRepairFixtureForTest(t *testing.T, enabled bool, ttl []time.Duration, setup func(*fixture)) RepairFixtureForTest {
	t.Helper()
	f, verifier, r, c, summary := reviewFeedbackFixtureConfigured(t, []func(*fixture){func(f *fixture) {
		if setup != nil {
			setup(f)
		}
		if len(ttl) > 0 {
			f.command.ExpiresAt = time.Now().Add(ttl[0]).UnixMilli()
		}
		if enabled {
			f.maxRepairs = 1
		}
	}})
	if err := verifier.RejectReviewWithFeedback(context.Background(), c, summary); err != nil {
		t.Fatal(err)
	}
	u, e := url.Parse(os.Getenv("AI_SDLC_TEST_DATABASE_URL"))
	if e != nil {
		t.Fatal(e)
	}
	u.User = url.UserPassword("aisdlc_orchestrator", "orchestrator"+strings.Repeat("q", 40))
	orch, e := store.Open(context.Background(), u.String())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { orch.Close() })
	var profile executor.Profile
	if e = json.Unmarshal(f.inputs.Profile, &profile); e != nil {
		t.Fatal(e)
	}
	model := ModelPolicy{Limits: store.ModelLimits{Calls: 4, InputBytes: 512 * 1024, OutputBytes: 128 * 1024}, ResponseLimit: 32 * 1024}
	return RepairFixtureForTest{Admin: f.Admin, Worker: f.db, Verifier: verifier, Orchestrator: orch, Policy: f.Material.Policy, Identities: f.Material.Identities,
		Config: f.Config, Command: f.command, Runtime: RuntimeConfig{Version: "ai-sdlc.candidate-runtime/v1", Profile: profile, ADev: string(f.inputs.ADev), Placement: f.placement, Model: model},
		PrepareOnly: func(t *testing.T, id string) Prepared {
			t.Helper()
			claim, e := Claim(context.Background(), f.db, f.readyPolicy, id)
			if e != nil {
				t.Fatal(e)
			}
			p, e := prepare(context.Background(), f.db, f.load, claim, f.source)
			if e != nil {
				t.Fatal(e)
			}
			return p
		},
		LoadLifecycle: f.loadLifecycle,
		Profile:       append([]byte(nil), f.inputs.Profile...), OriginalSource: append([]byte(nil), f.prepared.originalSource...), Review: r,
		Prepare: func(t *testing.T, id string) (Prepared, *Lifecycle) {
			t.Helper()
			claim, e := Claim(context.Background(), f.db, f.readyPolicy, id)
			if e != nil {
				t.Fatal("child claim", e)
			}
			p, e := prepare(context.Background(), f.db, f.load, claim, f.source)
			if e != nil {
				t.Fatal("child preparation", e)
			}
			l, e := OpenLifecycle(context.Background(), f.db, f.loadLifecycle, p)
			if e != nil {
				t.Fatal("child lifecycle", e)
			}
			return p, l
		},
	}
}

func RepairCommitFaultForTest(t *testing.T, role string) (*store.DB, <-chan struct{}) {
	return executionCommitFault(t, 1, role)
}

// CleanupRepairForTest records synthetic UID cleanup through the actual worker
// method; no Kubernetes operation is represented by this journal fixture.
func CleanupRepairForTest(t *testing.T, f RepairFixtureForTest, l *Lifecycle, id string) {
	t.Helper()
	var config, pod string
	if e := f.Admin.Pool.QueryRow(`SELECT config_uid,pod_uid FROM execution_lifecycles WHERE request_id=$1`, id).Scan(&config, &pod); e != nil {
		t.Fatal(e)
	}
	if e := f.Worker.ConfirmExecutionCleanup(context.Background(), l.durable, config, pod); e != nil {
		t.Fatal(e)
	}
}
func RejectRepairReviewForTest(t *testing.T, f RepairFixtureForTest, id string) store.ReviewRun {
	t.Helper()
	ctx := context.Background()
	v, e := f.Verifier.ReadVerification(ctx, id)
	if e != nil {
		t.Fatal(e)
	}
	c, e := f.Verifier.ClaimVerification(ctx, id, v.PayloadDigest)
	if e != nil {
		t.Fatal(e)
	}
	resources := map[string]string{}
	for _, kind := range []string{"config", "baseline", "candidate", "reference", "tests"} {
		resources[kind] = store.NewID()
		effect, e := f.Verifier.BeginVerificationEffect(ctx, c, kind)
		if e != nil {
			t.Fatal(e)
		}
		if e = f.Verifier.AcknowledgeVerificationEffect(ctx, effect, resources[kind]); e != nil {
			t.Fatal(e)
		}
	}
	proof, _ := json.Marshal(map[string]any{"version": "ai-sdlc.behavior-proof/v1", "command": json.RawMessage(v.Payload), "resourceUIDs": resources, "behaviorVerified": true, "publicationAuthorized": false})
	if e = f.Verifier.ConfirmVerificationCleanup(ctx, c); e != nil {
		t.Fatal(e)
	}
	if e = f.Verifier.FinishVerification(ctx, c, proof); e != nil {
		t.Fatal(e)
	}
	r, e := f.Verifier.QueueReview(ctx, id, authority.Digest(proof), f.Review.ProfileDigest, f.Review.ConfigurationDigest, f.Review.AssignmentDigest)
	if e != nil {
		t.Fatal(e)
	}
	claim, result := reviewFeedbackEffects(t, f.Admin, f.Verifier, r)
	if e = f.Verifier.RejectReviewWithFeedback(ctx, claim, result); e != nil {
		t.Fatal(e)
	}
	return r
}
