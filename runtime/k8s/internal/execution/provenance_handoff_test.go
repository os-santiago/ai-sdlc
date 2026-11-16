package execution

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/delivery"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/provenance"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/provenanceflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

// Synthetic DB-only cleanup isolates handoff authorization; it does not claim
// actual Kubernetes deletion or advance an installed workflow.
func provenanceCleanupFixture(t *testing.T, p provenanceFixture) {
	t.Helper()
	ctx := context.Background()
	c, e := p.b.journal.NextBuildRecovery(ctx)
	if e != nil || c.ID != p.record.Command.ID() {
		t.Fatal("original cleanup claim", e)
	}
	original, e := p.b.journal.ReadBuildCleanup(ctx, c)
	if e != nil {
		t.Fatal(e)
	}
	proof := store.BuildCleanupProof{Version: "ai-sdlc.build-cleanup/v1", BuildID: c.ID, SlotDigest: c.SlotDigest, Resources: original.Resources, ObservedAbsentAt: time.Now().UnixMilli()}
	if e = p.b.journal.RecordBuildCleanup(ctx, c, proof); e != nil {
		t.Fatal(e)
	}
}
func TestProvenanceSignedHandoffInboxAndReplay(t *testing.T) {
	p := newProvenanceFixture(t)
	ctx := context.Background()
	load := func() (provenance.Configuration, authority.Identity, error) {
		return *p.cfg, p.b.f.Material.Identities["build-controller"], nil
	}
	if sent, e := provenanceflow.DispatchBuilt(ctx, p.b.journal.DB, load); e != nil || sent {
		t.Fatal("dispatched before cleanup", e)
	}
	provenanceCleanupFixture(t, p)
	if sent, e := provenanceflow.DispatchBuilt(ctx, p.b.journal.DB, load); e != nil || !sent {
		t.Fatal("dispatch", e)
	}
	if sent, e := provenanceflow.DispatchBuilt(ctx, p.b.journal.DB, load); e != nil || sent {
		t.Fatal("dispatch replay", e)
	}
	var payload, raw []byte
	if e := p.b.journal.Pool.QueryRow(`SELECT payload,receipt FROM authorized_outbox WHERE operation='verify-built'`).Scan(&payload, &raw); e != nil {
		t.Fatal(e)
	}
	var signed authority.Receipt
	if authority.DecodeStrict(raw, &signed) != nil {
		t.Fatal("signed outbox")
	}
	command, e := provenance.VerifyCommand(*p.cfg, signed, payload, time.Now())
	if e != nil || command.BuildRecordSHA256 != p.record.Digest() {
		t.Fatal("command", e)
	}
	carried, e := p.b.f.Material.Identities["relay"].Carry(p.cfg.Build.Outer, signed, http.MethodPost, "/internal/verify-built", time.Now())
	if e != nil {
		t.Fatal(e)
	}
	envelope := delivery.Envelope{Receipt: signed, Transport: carried, Payload: payload}
	inbox := delivery.Inbox{DB: p.j.DB, Target: "verifier"}
	for n := 0; n < 2; n++ {
		envelope.Transport, e = p.b.f.Material.Identities["relay"].Carry(p.cfg.Build.Outer, signed, http.MethodPost, "/internal/verify-built", time.Now())
		if e != nil {
			t.Fatal(e)
		}
		_, duplicate, e := inbox.Accept(ctx, p.cfg.Build.Outer, envelope, "verify-built", http.MethodPost, "/internal/verify-built", time.Now(), provenanceflow.InboxEffect(p.j))
		if e != nil || duplicate != (n == 1) {
			t.Fatal("inbox", duplicate, e)
		}
	}
	if p.b.f.Count(t, `SELECT count(*) FROM provenance_requests`) != 1 || p.b.f.Count(t, `SELECT count(*) FROM provenance_handoffs`) != 1 {
		t.Fatal("duplicate delivery effects")
	}
	envelope.Transport, e = p.b.f.Material.Identities["relay"].Carry(p.cfg.Build.Outer, signed, http.MethodPost, "/internal/verify-built", time.Now())
	if e != nil {
		t.Fatal(e)
	}
	envelope.Payload = append(envelope.Payload, ' ')
	if _, _, e = inbox.Accept(ctx, p.cfg.Build.Outer, envelope, "verify-built", http.MethodPost, "/internal/verify-built", time.Now(), provenanceflow.InboxEffect(p.j)); e == nil {
		t.Fatal("changed signed delivery")
	}
}
func TestProvenanceHandoffRollbackAndCleanupSubstitution(t *testing.T) {
	for _, name := range []string{"outbox-failure", "wrong-cleanup-uid", "unknown-create", "changed-policy", "wrong-role"} {
		t.Run(name, func(t *testing.T) {
			p := newProvenanceFixture(t)
			ctx := context.Background()
			provenanceCleanupFixture(t, p)
			load := func() (provenance.Configuration, authority.Identity, error) {
				return *p.cfg, p.b.f.Material.Identities["build-controller"], nil
			}
			if name == "outbox-failure" {
				if _, e := p.b.f.Admin.Pool.Exec(`ALTER TABLE authorized_outbox ADD CONSTRAINT fixture_deny_provenance CHECK(operation<>'verify-built')`); e != nil {
					t.Fatal(e)
				}
				// A TRUNCATE in the next fixture does not remove constraints.
				// Register after opening the DB so LIFO cleanup runs first.
				t.Cleanup(func() {
					if _, e := p.b.f.Admin.Pool.Exec(`ALTER TABLE authorized_outbox DROP CONSTRAINT fixture_deny_provenance`); e != nil {
						t.Error("remove injected constraint", e)
					}
				})
			}
			if name == "wrong-cleanup-uid" || name == "unknown-create" {
				var raw []byte
				if e := p.b.f.Admin.Pool.QueryRow(`SELECT proof FROM build_cleanups`).Scan(&raw); e != nil {
					t.Fatal(e)
				}
				var proof store.BuildCleanupProof
				if json.Unmarshal(raw, &proof) != nil {
					t.Fatal("cleanup")
				}
				if name == "wrong-cleanup-uid" {
					proof.Resources[0].UID = store.NewID()
				} else {
					proof.UnknownCreate = true
				}
				raw, _ = json.Marshal(proof)
				if _, e := p.b.f.Admin.Pool.Exec(`UPDATE build_cleanups SET proof=$1,digest=$2`, raw, authority.Digest(raw)); e != nil {
					t.Fatal(e)
				}
			}
			if name == "changed-policy" {
				n := 0
				load = func() (provenance.Configuration, authority.Identity, error) {
					n++
					if n == 2 {
						p.cfg.Build.Outer.Freeze = true
					}
					return *p.cfg, p.b.f.Material.Identities["build-controller"], nil
				}
			}
			db := p.b.journal.DB
			if name == "wrong-role" {
				db = p.j.DB
			}
			if sent, e := provenanceflow.DispatchBuilt(ctx, db, load); e == nil || sent {
				t.Fatal("unsafe dispatch", e)
			}
			if p.b.f.Count(t, `SELECT count(*) FROM provenance_handoffs`) != 0 || p.b.f.Count(t, `SELECT count(*) FROM authorized_outbox WHERE operation='verify-built'`) != 0 {
				t.Fatal("partial dispatch")
			}
		})
	}
}

type provenanceVerifierFixture struct {
	run func(context.Context, store.BuiltRecord) (provenance.Proof, error)
}

func (v provenanceVerifierFixture) Verify(ctx context.Context, r store.BuiltRecord) (provenance.Proof, error) {
	return v.run(ctx, r)
}
func TestProvenanceRuntimeFencesFactoryAndCompletion(t *testing.T) {
	for _, name := range []string{"valid", "factory-revocation", "network-revocation", "network-error", "lost-commit"} {
		t.Run(name, func(t *testing.T) {
			p := newProvenanceFixture(t)
			ctx := context.Background()
			if e := p.j.QueueProvenance(ctx, p.payload, p.receipt); e != nil {
				t.Fatal(e)
			}
			calls := 0
			runtimeDB := p.j.DB
			var dropped <-chan struct{}
			if name == "lost-commit" {
				runtimeDB, dropped = executionCommitFault(t, 5, "verifier")
			}
			runtime := provenanceflow.Runtime{DB: runtimeDB, Load: func() (provenance.Configuration, error) { return *p.cfg, nil }, Verifier: func(cfg provenance.Configuration) (provenanceflow.EvidenceVerifier, error) {
				if name == "factory-revocation" {
					p.cfg.Build.Outer.Freeze = true
				}
				return provenanceVerifierFixture{run: func(c context.Context, r store.BuiltRecord) (provenance.Proof, error) {
					calls++
					deadline, ok := c.Deadline()
					if !ok || deadline.After(time.UnixMilli(p.command.ExpiresAt)) || r.Digest() != p.record.Digest() {
						t.Error("unbounded or changed verifier input")
					}
					if name == "network-revocation" {
						p.cfg.Build.Outer.Freeze = true
					}
					if name == "network-error" {
						return provenance.Proof{}, provenance.ErrProvenance
					}
					return p.proof, nil
				}}, nil
			}}
			state, e := runtime.PollOnce(ctx)
			if name == "lost-commit" {
				select {
				case <-dropped:
				default:
					t.Fatal("finish COMMIT acknowledgement not dropped")
				}
			}
			if name == "valid" || name == "lost-commit" {
				if e != nil || state != "provenance-recorded" || calls != 1 {
					t.Fatal("runtime", state, e, calls)
				}
				if state, e = runtime.PollOnce(ctx); e != nil || state != "provenance-idle" || calls != 1 {
					t.Fatal("runtime replay", state, e, calls)
				}
			} else {
				if e == nil || p.b.f.Count(t, `SELECT count(*) FROM provenance_records`) != 0 {
					t.Fatal("invalid completion", state, e)
				}
				expected := 1
				if name == "factory-revocation" {
					expected = 0
				}
				if calls != expected {
					t.Fatal("network call count", calls)
				}
				if p.b.f.Count(t, `SELECT count(*) FROM provenance_requests WHERE state='failed'`) != 1 {
					t.Fatal("attempt not retired")
				}
			}
		})
	}
}

func TestProvenanceHandoffWithdrawalDuringOutboxWait(t *testing.T) {
	p := newProvenanceFixture(t)
	ctx := context.Background()
	provenanceCleanupFixture(t, p)
	tx, e := p.b.f.Admin.Pool.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback()
	if _, e = tx.Exec(`LOCK TABLE authorized_outbox IN ACCESS EXCLUSIVE MODE`); e != nil {
		t.Fatal(e)
	}
	var blocker int
	if e = tx.QueryRow(`SELECT pg_backend_pid()`).Scan(&blocker); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() {
		_, e := provenanceflow.DispatchBuilt(ctx, p.b.journal.DB, func() (provenance.Configuration, authority.Identity, error) {
			return *p.cfg, p.b.f.Material.Identities["build-controller"], nil
		})
		done <- e
	}()
	blocked := false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if e = p.b.f.Admin.Pool.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE usename='aisdlc_build_controller' AND wait_event_type='Lock' AND $1=ANY(pg_blocking_pids(pid)))`, blocker).Scan(&blocked); e != nil {
			t.Fatal(e)
		}
		if blocked {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !blocked {
		t.Fatal("outbox wait was not observed")
	}
	if _, e = p.b.f.Admin.Pool.Exec(`UPDATE intake_heads SET generation=generation+1`); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	if e = <-done; e == nil {
		t.Fatal("withdrawal during outbox wait dispatched stale command")
	}
	if p.b.f.Count(t, `SELECT count(*) FROM provenance_handoffs`) != 0 || p.b.f.Count(t, `SELECT count(*) FROM authorized_outbox WHERE operation='verify-built'`) != 0 {
		t.Fatal("stale partial handoff")
	}
}

// This positive control runs after the failure-injection cases, including on a
// second -count invocation against the same disposable database.
func TestProvenanceHandoffAfterFailureInjectionRemainsUsable(t *testing.T) {
	p := newProvenanceFixture(t)
	var leaked bool
	if e := p.b.f.Admin.Pool.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_constraint WHERE conname='fixture_deny_provenance')`).Scan(&leaked); e != nil || leaked {
		t.Fatal("fault constraint escaped its subtest", e)
	}
	provenanceCleanupFixture(t, p)
	sent, e := provenanceflow.DispatchBuilt(context.Background(), p.b.journal.DB, func() (provenance.Configuration, authority.Identity, error) {
		return *p.cfg, p.b.f.Material.Identities["build-controller"], nil
	})
	if e != nil || !sent {
		t.Fatal("valid handoff after fault injection", e)
	}
}
