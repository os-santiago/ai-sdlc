package authority

import (
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T) (Policy, map[string]Identity) {
	t.Helper()
	p := Policy{Version: "1", Repository: "example/project", Environment: "production-test", Autonomy: "auto-deploy", Keys: map[string]PublicKey{}}
	identities := map[string]Identity{}
	for role := range roles {
		i, key, err := NewIdentity(role, role+"-1")
		if err != nil {
			t.Fatal(err)
		}
		identities[role] = i
		p.Keys[i.KeyID] = key
	}
	for op, pair := range operations {
		p.Grants = append(p.Grants, Grant{Caller: pair[0], Target: pair[1], Operation: op, MinimumLevel: floors[op], EventType: eventTypes[op]})
	}
	return p, identities
}

func TestEveryAllowedTransition(t *testing.T) {
	p, identities := fixture(t)
	for _, grant := range p.Grants {
		t.Run(grant.Operation, func(t *testing.T) {
			payload := []byte(`{"repository":"example/project","id":7}`)
			r, err := identities[grant.Caller].Seal(p, "event-1", grant.Operation, payload)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Unix(1800000000, 0)
			for _, carrier := range []string{grant.Caller, "relay"} {
				transport, err := identities[carrier].Carry(p, r, "POST", "/internal/event", now)
				if err != nil {
					t.Fatal(err)
				}
				if err := p.VerifyDelivery(transport, r, grant.Target, grant.Operation, "POST", "/internal/event", payload, now); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestAuthorityDenials(t *testing.T) {
	for _, scenario := range []string{"freeze", "shadow", "suggest", "wrong-repository", "wrong-environment", "forged-actor", "maintenance", "unknown-operation", "wrong-target", "downgraded-floor", "wrong-event-type", "duplicate-grant", "unknown-level", "shared-key"} {
		t.Run(scenario, func(t *testing.T) {
			p, _ := fixture(t)
			caller, target, op, repo, env := "orchestrator", "worker", "execute-work", p.Repository, p.Environment
			switch scenario {
			case "freeze":
				p.Freeze = true
			case "shadow", "suggest":
				p.Autonomy = scenario
			case "wrong-repository":
				repo = "other/project"
			case "wrong-environment":
				env = "production"
			case "forged-actor":
				caller = "issue-author"
			case "maintenance":
				caller = "installer"
			case "unknown-operation":
				op = "merge-anything"
			case "wrong-target":
				target = "release-manager"
			case "downgraded-floor":
				for i := range p.Grants {
					if p.Grants[i].Operation == op {
						p.Grants[i].MinimumLevel = "shadow"
					}
				}
			case "wrong-event-type":
				p.Grants[0].EventType = "Anything"
			case "duplicate-grant":
				p.Grants = append(p.Grants, p.Grants[0])
			case "unknown-level":
				p.Autonomy = "unlimited"
			case "shared-key":
				key := p.Keys["relay-1"]
				key.Role = "worker"
				p.Keys["worker-1"] = key
			}
			if err := p.Allows(caller, target, op, repo, env); err == nil {
				t.Fatal("unauthorized transition allowed")
			}
		})
	}
}

func TestAutonomyFloors(t *testing.T) {
	for level, rank := range levels {
		p, _ := fixture(t)
		p.Autonomy = level
		for _, grant := range p.Grants {
			err := p.Allows(grant.Caller, grant.Target, grant.Operation, p.Repository, p.Environment)
			if (err == nil) != (rank >= levels[floors[grant.Operation]]) {
				t.Fatalf("unexpected %s / %s: %v", level, grant.Operation, err)
			}
		}
	}
}

func TestStrictPolicyParsing(t *testing.T) {
	p, _ := fixture(t)
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParsePolicy(raw); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"", "null", "{}", string(raw) + " {}", strings.Replace(string(raw), `"version":"1"`, `"version":"1","version":"1"`, 1), strings.Replace(string(raw), `"version":"1"`, `"version":"1","unknown":true`, 1), strings.Repeat("[", 33) + strings.Repeat("]", 33), strings.Repeat(" ", 128*1024+1)} {
		if _, err := ParsePolicy([]byte(raw)); err == nil {
			t.Fatal("invalid policy accepted")
		}
	}
}

func TestSignedDeliveryTampering(t *testing.T) {
	for _, scenario := range []string{"payload", "empty-payload", "oversize-payload", "id", "producer", "audience", "operation", "repository", "environment", "signature", "revoked-key", "wrong-private-key", "relay-forgery", "method", "path", "transport-caller", "transport-signature", "transport-expired", "transport-future", "timestamp-min", "timestamp-max", "nonce", "policy-change"} {
		t.Run(scenario, func(t *testing.T) {
			p, ids := fixture(t)
			payload := []byte(`{"issue":1}`)
			r, err := ids["intake"].Seal(p, "event-1", "admit-issue", payload)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Unix(1800000000, 0)
			transport, err := ids["relay"].Carry(p, r, "POST", "/admit", now)
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "payload":
				payload = []byte(`{"issue":2}`)
			case "empty-payload":
				payload = nil
			case "oversize-payload":
				payload = make([]byte, 1024*1024+1)
			case "id":
				r.ID = "event-2"
			case "producer":
				r.Producer = "worker"
			case "audience":
				r.Target = "worker"
			case "operation":
				r.Operation = "execute-work"
			case "repository":
				r.Repository = "other/project"
			case "environment":
				r.Environment = "production"
			case "signature":
				r.Signature = "invalid"
			case "revoked-key":
				delete(p.Keys, r.KeyID)
			case "wrong-private-key":
				i := ids["intake"]
				i.PrivateKey = ids["worker"].PrivateKey
				if _, err := i.Seal(p, "event-2", "admit-issue", payload); err == nil {
					t.Fatal("wrong signing key accepted")
				}
				return
			case "relay-forgery":
				if _, err := ids["relay"].Seal(p, "event-2", "admit-issue", payload); err == nil {
					t.Fatal("relay forged intake")
				}
				return
			case "method":
				transport.Method = "GET"
			case "path":
				transport.Path = "/other"
			case "transport-caller":
				transport.Caller = "worker"
			case "transport-signature":
				transport.Signature = "invalid"
			case "transport-expired":
				now = now.Add(301 * time.Second)
			case "transport-future":
				now = now.Add(-31 * time.Second)
			case "timestamp-min":
				transport.IssuedAt = math.MinInt64
			case "timestamp-max":
				transport.IssuedAt = math.MaxInt64
			case "nonce":
				transport.Nonce = "00"
			case "policy-change":
				p.Autonomy = "shadow"
			}
			if err := p.VerifyDelivery(transport, r, "admission-controller", "admit-issue", "POST", "/admit", payload, now); err == nil {
				t.Fatal("tampered delivery accepted")
			}
		})
	}
}

func TestDurableReceiptFreshTransportAfterOutageAndRotation(t *testing.T) {
	p, ids := fixture(t)
	payload := []byte(`{"issue":1}`)
	r, err := ids["intake"].Seal(p, "event-1", "admit-issue", payload)
	if err != nil {
		t.Fatal(err)
	}
	then := time.Unix(1800000000, 0)
	old, err := ids["relay"].Carry(p, r, "POST", "/admit", then)
	if err != nil {
		t.Fatal(err)
	}
	newRelay, key, err := NewIdentity("relay", "relay-2")
	if err != nil {
		t.Fatal(err)
	}
	delete(p.Keys, "relay-1")
	p.Keys[newRelay.KeyID] = key
	now := then.Add(24 * time.Hour)
	fresh, err := newRelay.Carry(p, r, "POST", "/admit", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.VerifyDelivery(old, r, "admission-controller", "admit-issue", "POST", "/admit", payload, now); err == nil {
		t.Fatal("expired transport accepted")
	}
	if err := p.VerifyDelivery(fresh, r, "admission-controller", "admit-issue", "POST", "/admit", payload, now); err != nil {
		t.Fatal(err)
	}
	p.Freeze = true
	if err := p.VerifyDelivery(fresh, r, "admission-controller", "admit-issue", "POST", "/admit", payload, now); err == nil {
		t.Fatal("freeze bypassed")
	}
}

func TestCommittedPolicyFixture(t *testing.T) {
	raw, err := os.ReadFile("../../../../spec/fixtures/component-authority.v1.json")
	if err != nil {
		t.Fatal(err)
	}
	p, err := ParsePolicy(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Freeze || p.Autonomy != "shadow" || len(p.Grants) != len(operations) {
		t.Fatal("fixture must be complete and frozen")
	}
	p.Freeze = false
	p.Autonomy = "auto-deploy"
	for _, grant := range p.Grants {
		if err := p.Allows(grant.Caller, grant.Target, grant.Operation, p.Repository, p.Environment); err != nil {
			t.Fatal(err)
		}
	}
}

func TestExplicitPolicyFields(t *testing.T) {
	p, _ := fixture(t)
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"version", "repository", "environment", "autonomy", "freeze", "keys", "grants"} {
		value := fields[field]
		delete(fields, field)
		altered, _ := json.Marshal(fields)
		if _, err := ParsePolicy(altered); err == nil {
			t.Fatalf("missing %s accepted", field)
		}
		fields[field] = value
	}
	fields["freeze"] = json.RawMessage("null")
	altered, _ := json.Marshal(fields)
	if _, err := ParsePolicy(altered); err == nil {
		t.Fatal("null freeze accepted")
	}
	duplicate := strings.Replace(string(raw), `"freeze":false`, `"freeze":true,"Freeze":false`, 1)
	if _, err := ParsePolicy([]byte(duplicate)); err == nil {
		t.Fatal("case-insensitive duplicate accepted")
	}
}
