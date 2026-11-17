package execution

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"

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
}

func NewRepairFixtureForTest(t *testing.T, enabled bool) RepairFixtureForTest {
	t.Helper()
	f, verifier, r, c, summary := reviewFeedbackFixtureConfigured(t, []func(*fixture){func(f *fixture) {
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
		Profile: append([]byte(nil), f.inputs.Profile...), OriginalSource: append([]byte(nil), f.prepared.originalSource...), Review: r,
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
