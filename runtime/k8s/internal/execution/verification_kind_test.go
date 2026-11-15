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

	load := func() (authority.Policy, authority.Identity, intake.Config, string, error) {
		p, i, e := component.Authority()
		return p, i, f.Config, profile, e
	}
	if sent, err := HandoffCandidate(ctx, f.db, load); err != nil || sent {
		t.Fatal("operational handoff duplicated", err)
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
		PublicationIdentities                           map[string]authority.Identity
		Identity                                        authority.Identity
		Intake                                          intake.Config
		Profile, Image, ReportFile, SourceSHA, Scenario string
		Config                                          podtransport.Config
		Envelope, RetryEnvelope                         delivery.Envelope
	}{f.Material.Policy, nil, f.Material.Identities["verifier"], f.Config, profile, image, report, source, scenario, config, envelope, retry}
	if os.Getenv("AI_SDLC_PUBLICATION_FIXTURE") == "1" {
		control.PublicationIdentities = map[string]authority.Identity{"relay": f.Material.Identities["relay"], "release-manager": f.Material.Identities["release-manager"]}
	}
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
