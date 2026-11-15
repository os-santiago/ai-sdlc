package execution

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/delivery"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/intake"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/bootstrap"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/podtransport"
)

// A separate test process consumes the real signed outbox with its own database
// identity. This avoids importing the verifier into the worker implementation.
func verifyCapturedFixture(t *testing.T, f *fixture, component *bootstrap.Component, driver string, config podtransport.Config, image, report, profile, source, scenario string, calls int32) {
	t.Helper()
	ctx := context.Background()
	if calls != 3 || driver == "" {
		t.Fatal("verification fixture inputs")
	}
	if _, err := f.Admin.Pool.Exec(`GRANT SELECT,INSERT ON verification_handoffs TO aisdlc_worker;
 GRANT SELECT ON execution_candidates,execution_requests,execution_lifecycles,execution_models,model_allocations,model_budgets,verification_handoffs,work_items,plans,intake_heads,intake_revisions,intake_cursors,intake_observations,admission_heads,planning_heads,admissions TO aisdlc_verifier;
 GRANT SELECT,INSERT,UPDATE ON verification_requests,verification_effects,authorized_inbox TO aisdlc_verifier; GRANT SELECT,INSERT ON authorized_nonces TO aisdlc_verifier`); err != nil {
		t.Fatal(err)
	}
	load := func() (authority.Policy, authority.Identity, intake.Config, string, error) {
		p, i, e := component.Authority()
		return p, i, f.Config, profile, e
	}
	if sent, err := HandoffCandidate(ctx, f.db, load); err != nil || !sent {
		t.Fatal("signed verification handoff", err)
	}
	var envelope delivery.Envelope
	var receipt []byte
	if err := f.Admin.Pool.QueryRow(`SELECT payload,receipt FROM authorized_outbox WHERE operation='verify-artifact'`).Scan(&envelope.Payload, &receipt); err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(receipt, &envelope.Receipt) != nil {
		t.Fatal("outbox receipt")
	}
	var err error
	envelope.Transport, err = f.Material.Identities["relay"].Carry(f.Material.Policy, envelope.Receipt, http.MethodPost, "/internal/verify-artifact", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	retry := envelope
	retry.Transport, err = f.Material.Identities["relay"].Carry(f.Material.Policy, envelope.Receipt, http.MethodPost, "/internal/verify-artifact", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	control := struct {
		Policy                                          authority.Policy
		Intake                                          intake.Config
		Profile, Image, ReportFile, SourceSHA, Scenario string
		Config                                          podtransport.Config
		Envelope, RetryEnvelope                         delivery.Envelope
	}{f.Material.Policy, f.Config, profile, image, report, source, scenario, config, envelope, retry}
	raw, _ := json.Marshal(control)
	path := filepath.Join(t.TempDir(), "verification.json")
	if os.WriteFile(path, raw, 0600) != nil {
		t.Fatal("fixture control")
	}
	runCtx, cancel := context.WithTimeout(ctx, 210*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, driver, "-test.run", "^TestRealVerificationFlowFixture$", "-test.v")
	cmd.Env = append(os.Environ(), "AI_SDLC_VERIFICATION_FLOW_FIXTURE="+path)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if len(output) > 4096 {
			output = output[len(output)-4096:]
		}
		t.Fatalf("independent verifier: %v %s", err, output)
	}
	if sent, err := HandoffCandidate(ctx, f.db, load); err != nil || sent {
		t.Fatal("handoff replay", err)
	}
}
