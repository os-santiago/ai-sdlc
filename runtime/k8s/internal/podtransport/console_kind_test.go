package podtransport

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasing"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// Actual isolated Kubernetes/API/data-plane rollback fixture. The TLS server's
// business response is synthetic; no actual vault/crypto/E2E claim is permitted.
func TestRealConsoleDeploymentRollback(t *testing.T) {
	path := os.Getenv("AI_SDLC_CONSOLE_FIXTURE_JSON")
	if path == "" {
		t.Skip("owned disposable console cluster required")
	}
	raw, e := os.ReadFile(path)
	if e != nil {
		t.Fatal("fixture input unavailable")
	}
	var input struct {
		Client Config
		Target releasing.Target
		Report string
	}
	if len(raw) > 256*1024 || authority.DecodeStrict(raw, &input) != nil || input.Report == "" {
		t.Fatal("invalid fixture input")
	}
	client, e := NewClient(input.Client)
	if e != nil {
		t.Fatal(e)
	}
	reader := &ConsoleDeployment{client: client}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	raw, e = reader.request(ctx, http.MethodGet, consolePath, nil)
	var d apps.Deployment
	if e != nil || authority.DecodeStrict(raw, &d) != nil {
		t.Fatal("original deployment unavailable")
	}
	input.Target.DeploymentSpecSHA256, e = ConsoleSpecDigest(d.Spec)
	if e != nil {
		t.Fatal("fixture security spec", e)
	}
	raw, e = reader.request(ctx, http.MethodGet, "/api/v1/namespaces/"+consoleNS+"/services/"+consoleName, nil)
	var s core.Service
	if e != nil || authority.DecodeStrict(raw, &s) != nil {
		t.Fatal("original service unavailable")
	}
	input.Target.ServiceSpecSHA256, e = ConsoleServiceDigest(s.Spec)
	if e != nil {
		t.Fatal(e)
	}
	adapter, e := NewConsoleDeployment(client, input.Target)
	if e != nil {
		t.Fatal(e)
	}
	before, e := adapter.Observe(ctx)
	if e != nil {
		t.Fatal("baseline observation", e)
	}
	baseline, e := adapter.VerifyHealth(ctx, before)
	if e != nil {
		t.Fatal("baseline service TLS contract", e)
	}
	now := time.Now()
	plan := releasing.Plan{Version: releasing.PlanVersion, TargetSHA256: input.Target.Digest(), PreviousImage: input.Target.BaselineImage, Image: building.Registry + "@sha256:" + strings.Repeat("f", 64), CompiledAt: now.UnixMilli(), ExpiresAt: now.Add(releasing.PlanMaxAge).UnixMilli()}
	calls := 0
	effects := &ConsoleEffects{Deployment: adapter, Gate: consoleGate{func(_ context.Context, s ConsoleSnapshot, digest string, rollback bool) error {
		calls++
		if s.TargetSHA256 != input.Target.Digest() || !hex64.MatchString(digest) || rollback != (calls == 2) {
			return errors.New("fixture gate denied")
		}
		return nil
	}}}
	applied, e := effects.Apply(ctx, plan, before)
	if e != nil {
		t.Fatal("conditional image-only API patch", e)
	}
	wait, stop := context.WithTimeout(ctx, 8*time.Second)
	_, e = adapter.WaitReady(wait, applied.After)
	timedOut := errors.Is(wait.Err(), context.DeadlineExceeded)
	stop()
	if e == nil || !timedOut {
		t.Fatal("unpullable candidate did not reach the bounded readiness deadline")
	}
	current, e := adapter.Observe(ctx)
	if e != nil {
		t.Fatal("current owned failed rollout", e)
	}
	rolled, e := effects.Rollback(ctx, applied, current)
	if e != nil {
		t.Fatal("conditional rollback", e)
	}
	if _, e = adapter.WaitReady(ctx, rolled.After); e != nil {
		t.Fatal("baseline rollback readiness", e)
	}
	health, e := adapter.VerifyHealth(ctx, rolled.After)
	if e != nil {
		t.Fatal("rollback service TLS contract", e)
	}
	if calls != 2 || health.Rollout.Image != input.Target.BaselineImage || health.Rollout.DeploymentUID != baseline.Rollout.DeploymentUID || health.Rollout.Generation != before.Generation+2 {
		t.Fatal("unexpected target or effect count")
	}
	report := map[string]any{"version": 1, "scope": "isolated-kubernetes-console-rollback", "workflowEvidence": false, "vaultCryptographyValidated": false, "candidateSuccessfulRolloutValidated": false, "status": "passed", "baseline": baseline, "applied": applied, "rolledBack": rolled, "health": health}
	data, _ := json.MarshalIndent(report, "", "  ")
	if os.WriteFile(input.Report, append(data, '\n'), 0600) != nil {
		t.Fatal("public fixture report unavailable")
	}
}
