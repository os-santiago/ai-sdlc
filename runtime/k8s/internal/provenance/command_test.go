package provenance

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestVerificationCommandUsesFreshAuthorityForExpiredHistory(t *testing.T) {
	f := newFixture(t)
	_, ca := tlsFixture(t)
	f.trust.RegistryCA = ca
	cfg := Configuration{Build: f.config, Trust: f.trust}
	now := time.Now().Add(21 * time.Minute)
	if f.record.Command.ExpiresAt >= now.UnixMilli() {
		t.Fatal("fixture build must be expired")
	}
	c, e := NewCommand(cfg, f.record, now)
	if e != nil {
		t.Fatal(e)
	}
	raw := jsonBytes(t, c)
	signed, e := f.material.Identities["build-controller"].Seal(cfg.Build.Outer, c.ID(), "verify-built", raw)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = VerifyCommand(cfg, signed, raw, now); e != nil {
		t.Fatal(e)
	}
	if c.ExpiresAt <= f.record.Command.ExpiresAt || c.BindRecord(cfg, f.record, now) != nil {
		t.Fatal("new verification is not separately bounded")
	}
	if _, e = VerifyCommand(cfg, signed, raw, time.UnixMilli(c.ExpiresAt)); e == nil {
		t.Fatal("expired verification accepted")
	}
	if c.BindRecord(cfg, f.record, time.UnixMilli(c.ExpiresAt)) == nil {
		t.Fatal("history renewed verification")
	}
}

func TestVerificationCommandRejectsSubstitutionAndRevocation(t *testing.T) {
	for _, name := range []string{"freeze-inner", "freeze-outer", "trust-change", "profile-change", "intake-change", "key-revoked", "record-change", "unknown", "alias", "missing", "future", "excess-budget", "wrong-operation"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			_, ca := tlsFixture(t)
			f.trust.RegistryCA = ca
			cfg := Configuration{Build: f.config, Trust: f.trust}
			now := time.Now()
			c, e := NewCommand(cfg, f.record, now)
			if e != nil {
				t.Fatal(e)
			}
			raw := jsonBytes(t, c)
			switch name {
			case "unknown":
				raw = append(raw[:len(raw)-1], []byte(`,"extra":true}`)...)
			case "alias":
				raw = bytes.Replace(raw, []byte(`"buildID"`), []byte(`"BuildID"`), 1)
			case "missing":
				var m map[string]any
				_ = json.Unmarshal(raw, &m)
				delete(m, "buildRecordSHA256")
				raw = jsonBytes(t, m)
			case "future":
				c.IssuedAt = now.Add(10 * time.Second).UnixMilli()
				raw = jsonBytes(t, c)
			case "excess-budget":
				c.ExpiresAt = now.Add(3 * time.Minute).UnixMilli()
				raw = jsonBytes(t, c)
			}
			key := f.material.Identities["build-controller"]
			op := "verify-built"
			if name == "wrong-operation" {
				key = f.material.Identities["release-manager"]
				op = "build-merged"
			}
			signed, e := key.Seal(cfg.Build.Outer, c.ID(), op, raw)
			if e != nil {
				t.Fatal(e)
			}
			switch name {
			case "freeze-inner":
				cfg.Build.Inner.Freeze = true
			case "freeze-outer":
				cfg.Build.Outer.Freeze = true
			case "trust-change":
				cfg.Trust.ExpiresAt--
			case "profile-change":
				cfg.Build.IssueMaxAgeSeconds++
			case "intake-change":
				cfg.Build.IntakeConfigSHA256 = f.record.Command.IssueRevision
			case "key-revoked":
				delete(cfg.Build.Outer.Keys, key.KeyID)
			case "record-change":
				f.record.RecordedAt = f.record.RecordedAt.Add(time.Millisecond)
			}
			if name == "record-change" {
				if c.BindRecord(cfg, f.record, now) == nil {
					t.Fatal("record substitution")
				}
				return
			}
			if _, e = VerifyCommand(cfg, signed, raw, now); e == nil {
				t.Fatal("invalid authority accepted")
			}
		})
	}
}

func TestVerificationProofBoundToCurrentCommand(t *testing.T) {
	f := newFixture(t)
	v, _ := f.serve(t, nil)
	cfg := Configuration{Build: f.config, Trust: f.trust}
	c, e := NewCommand(cfg, f.record, time.Now())
	if e != nil {
		t.Fatal(e)
	}
	proof, e := v.Verify(context.Background(), f.record)
	if e != nil {
		t.Fatal(e)
	}
	if c.BindProof(cfg, f.record, proof, time.Now()) != nil {
		t.Fatal("valid proof")
	}
	for _, name := range []string{"release", "record", "trust", "context", "observation", "evidence", "verifier", "freeze"} {
		t.Run(name, func(t *testing.T) {
			p := proof
			changed := cfg
			switch name {
			case "release":
				p.ReleaseAuthorized = true
			case "record":
				p.BuildRecordSHA256 = "changed"
			case "trust":
				p.TrustSHA256 = "changed"
			case "context":
				p.RetainedContextSHA256 = "changed"
			case "observation":
				p.BuildObservationSHA256 = "changed"
			case "evidence":
				p.AttestationBlobDigest = "mutable-tag"
			case "verifier":
				p.VerifierVersion = "unapproved"
			case "freeze":
				changed.Build.Outer.Freeze = true
			}
			if c.BindProof(changed, f.record, p, time.Now()) == nil {
				t.Fatal("unbound proof accepted")
			}
		})
	}
}
