package execution

import (
	"context"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasecompletion"
	"testing"
	"time"
)

func TestTerminalPlanDerivesOnlyAuthenticatedSourceAndArtifact(t *testing.T) {
	f, claim := completedDeployment(t)
	r, e := f.j.DB.ReadDeployedForCompletion(context.Background(), claim.ID)
	if e != nil {
		t.Fatal(e)
	}
	now := time.Now()
	c, e := releasecompletion.NewCommand(*f.p.cfg, r.Deployment, r.Upstream.Promotion, r.Upstream.Provenance, r.Upstream.Build, now)
	if e != nil {
		t.Fatal(e)
	}
	p, e := releasecompletion.CompilePlan(*f.p.cfg, c, r.Deployment, r.Upstream.Promotion, r.Upstream.Provenance, r.Upstream.Build, now)
	if e != nil {
		t.Fatal(e)
	}
	if p.SourceSHA != r.Upstream.Build.Command.SourceSHA || p.Image != r.Deployment.Receipt.After.Image || p.Tag != "ai-sdlc/"+c.ID() || p.ExpiresAt != c.ExpiresAt {
		t.Fatal("terminal plan does not bind original successful deployment")
	}
	if p.BindRecords(*f.p.cfg, c, r.Deployment, r.Upstream.Promotion, r.Upstream.Provenance, r.Upstream.Build, now) != nil {
		t.Fatal("derived plan rejected")
	}
	for _, mutate := range []func(*releasecompletion.Plan){func(p *releasecompletion.Plan) { p.SourceSHA = "main" }, func(p *releasecompletion.Plan) { p.Tag = "v1.0.0" }, func(p *releasecompletion.Plan) { p.Body = "untrusted model text" }, func(p *releasecompletion.Plan) { p.ExpiresAt++ }, func(p *releasecompletion.Plan) { p.IssueNumber++ }, func(p *releasecompletion.Plan) { p.Image = f.p.cfg.Target.BaselineImage }} {
		changed := p
		mutate(&changed)
		if changed.BindRecords(*f.p.cfg, c, r.Deployment, r.Upstream.Promotion, r.Upstream.Provenance, r.Upstream.Build, now) == nil {
			t.Fatal("altered terminal plan accepted")
		}
	}
}
