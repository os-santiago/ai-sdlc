package productioncompletion

import (
	"encoding/json"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"strings"
	"testing"
	"time"
)

func TestProductionFactRejectsAliasedSignedPayloadAndWrongIdentity(t *testing.T) {
	m, e := authority.Bootstrap("os-santiago/saferus", "ai-sdlc-production")
	if e != nil {
		t.Fatal(e)
	}
	p := m.Policy
	p.Freeze = false
	p.Autonomy = "auto-deploy"
	f := Fact{TerminalID: "complete-" + strings.Repeat("a", 64), TerminalRecordSHA256: strings.Repeat("b", 64), WorkItemID: "11111111-1111-1111-1111-111111111111", Repository: p.Repository, IssueNumber: 4, Generation: 1, IssueRevision: strings.Repeat("c", 64), SourceSHA: strings.Repeat("d", 40), Image: "registry.example/saferus@sha256:" + strings.Repeat("e", 64), CompletedAt: time.Now().Add(-24 * time.Hour).UnixMilli()}
	f.ReleaseURL = "https://github.com/" + p.Repository + "/releases/tag/ai-sdlc/" + f.TerminalID
	c, e := New(p, f)
	if e != nil {
		t.Fatal("historical fact acquired a deadline", e)
	}
	raw, _ := json.Marshal(c)
	signed, e := m.Identities["release-manager"].Seal(p, c.ID(), "record-production", raw)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = Verify(p, signed, raw); e != nil {
		t.Fatal(e)
	}
	alias := []byte(strings.Replace(string(raw), `"workItemId":`, `"WorkItemId":`, 1))
	signed, e = m.Identities["release-manager"].Seal(p, c.ID(), "record-production", alias)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = Verify(p, signed, alias); e == nil {
		t.Fatal("signed key alias")
	}
	if _, e = m.Identities["deployer"].Seal(p, c.ID(), "record-production", raw); e == nil {
		t.Fatal("wrong producer")
	}
	for _, mutate := range []func(*Fact){func(f *Fact) { f.Repository = "os-santiago/homedir" }, func(f *Fact) { f.SourceSHA = "main" }, func(f *Fact) { f.ReleaseURL += "?redirect=1" }, func(f *Fact) { f.CompletedAt = time.Now().Add(time.Minute).UnixMilli() }, func(f *Fact) { f.WorkItemID = "arbitrary" }} {
		bad := f
		mutate(&bad)
		if _, e = New(p, bad); e == nil {
			t.Fatal("unbound fact")
		}
	}
	p.Freeze = true
	if _, e = New(p, f); e == nil {
		t.Fatal("frozen transport")
	}
}
