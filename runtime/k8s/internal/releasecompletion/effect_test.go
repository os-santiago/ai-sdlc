package releasecompletion

import (
	"strings"
	"testing"
	"time"
)

func effectPlan() Plan {
	p := Plan{Version: "ai-sdlc.terminal-release-plan/v1", CommandID: "complete-" + strings.Repeat("a", 64), Repository: "os-santiago/saferus", IssueNumber: 4, Generation: 1, IssueRevision: strings.Repeat("b", 64), SourceSHA: strings.Repeat("c", 40), TreeSHA: strings.Repeat("d", 40), Image: "registry.example/saferus@sha256:" + strings.Repeat("e", 64), TargetSHA256: strings.Repeat("f", 64), DeploymentRecordSHA256: strings.Repeat("1", 64), ProvenanceRecordSHA256: strings.Repeat("2", 64), BuildRecordSHA256: strings.Repeat("3", 64), Title: "AI-SDLC kind test production: issue #4", ExpiresAt: time.Now().Add(time.Minute).UnixMilli()}
	p.Tag = "ai-sdlc/" + p.CommandID
	p.Body = p.BodyText()
	return p
}
func TestReceiptBindsExactRequestSourceAndOriginalWindow(t *testing.T) {
	p := effectPlan()
	sent := time.Now()
	q, e := Request(p, "tag", 0, sent)
	if e != nil {
		t.Fatal(e)
	}
	r := EffectReceipt{Step: "tag", PlanSHA256: p.Digest(), RequestSHA256: q.Digest(), Tag: p.Tag, CommitSHA: p.SourceSHA, ObservedAt: sent.UnixMilli()}
	if ValidateReceipt(p, q, 0, r, sent, time.Now()) != nil {
		t.Fatal("valid")
	}
	for _, mutate := range []func(*EffectReceipt){func(r *EffectReceipt) { r.RequestSHA256 = strings.Repeat("0", 64) }, func(r *EffectReceipt) { r.CommitSHA = strings.Repeat("0", 40) }, func(r *EffectReceipt) { r.ObservedAt = sent.UnixMilli() - 1 }, func(r *EffectReceipt) { r.ObservedAt = p.ExpiresAt }, func(r *EffectReceipt) { r.ReleaseID = 99 }, func(r *EffectReceipt) { r.Step = "publish" }} {
		bad := r
		mutate(&bad)
		if ValidateReceipt(p, q, 0, bad, sent, time.Now()) == nil {
			t.Fatal("unbound receipt")
		}
	}
	q.Body = []byte(`{"ref":"refs/tags/main"}`)
	if ValidateReceipt(p, q, 0, r, sent, time.Now()) == nil {
		t.Fatal("changed request")
	}
}
func TestPlanRejectsArbitraryNotesDestinationAndMutableSource(t *testing.T) {
	p := effectPlan()
	for _, mutate := range []func(*Plan){func(p *Plan) { p.Repository = "os-santiago/homedir" }, func(p *Plan) { p.SourceSHA = "main" }, func(p *Plan) { p.Tag = "v1" }, func(p *Plan) { p.Body += "external notes" }, func(p *Plan) { p.Image = "registry.example/saferus:latest" }, func(p *Plan) { p.ExpiresAt = time.Now().Add(10 * time.Minute).UnixMilli() }} {
		bad := p
		mutate(&bad)
		if _, e := Request(bad, "tag", 0, time.Now()); e == nil {
			t.Fatal("unbounded plan")
		}
	}
	for _, value := range []string{"https://github.com.evil/" + p.Repository + "/releases/tag/" + p.Tag, "https://user@github.com/" + p.Repository + "/releases/tag/" + p.Tag, "https://github.com/" + p.Repository + "/releases/tag/" + p.Tag + "?redirect=1"} {
		if ValidReleaseURL(value, p) {
			t.Fatal("unbound URL")
		}
	}
}
