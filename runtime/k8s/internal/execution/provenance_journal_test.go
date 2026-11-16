package execution

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildworkload"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/provenance"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

type provenanceFixture struct {
	b                buildFixture
	cfg              *provenance.Configuration
	j                *store.ProvenanceJournal
	record           store.BuiltRecord
	command          provenance.Command
	payload, receipt []byte
	proof            provenance.Proof
}

func newProvenanceFixture(t *testing.T) provenanceFixture {
	t.Helper()
	ctx := context.Background()
	b := newBuildFixture(t)
	b.cfg.IssueMaxAgeSeconds = 300
	b.command.ProfileSHA256 = b.cfg.ProfileDigest()
	extendBuildQueueAuthority(t, &b)
	claim := buildClaim(t, b)
	w := buildWriterDone(t, b, claim)
	pub := buildPublisherDone(t, b, claim)
	if e := b.journal.FinishBuild(ctx, claim, buildResult(b, w, pub)); e != nil {
		t.Fatal(e)
	}
	if e := b.f.Admin.ProvisionRolesForProvenance(ctx, b.passwords, "verification", true, true, true, true, true); e != nil {
		t.Fatal(e)
	}
	u, e := url.Parse(os.Getenv("AI_SDLC_TEST_DATABASE_URL"))
	if e != nil {
		t.Fatal(e)
	}
	u.User = url.UserPassword("aisdlc_verifier", b.passwords["verifier"])
	db, e := store.Open(ctx, u.String())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = db.Close() })
	r, e := db.ReadBuiltRecord(ctx, claim.ID)
	if e != nil {
		t.Fatal(e)
	}
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	spki, e := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if e != nil {
		t.Fatal(e)
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "isolated journal fixture"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, e := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if e != nil {
		t.Fatal(e)
	}
	trust := provenance.Trust{Version: provenance.PolicyVersion, Scope: provenance.LocalScope, Repository: r.Command.Repository, Environment: r.Command.Environment, ClusterUID: r.Context.Profile.ClusterUID, BuildNamespaceUID: r.Context.Profile.NamespaceUID, Registry: r.Result.ImageURL, RegistryCA: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), SignerPublicKey: string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: spki})), SignerSHA256: authority.Digest(spki), PublisherImage: r.Result.PublisherImage, VerifierImage: "quay.io/os-santiago/ai-sdlc@sha256:" + strings.Repeat("e", 64), VerifierVersion: provenance.VerifierVersion, CompilerVersion: buildworkload.CompilerVersion, PredicateType: provenance.PredicateType, Transparency: provenance.LocalTransparency, ApprovedAt: now.Add(-time.Minute).UnixMilli(), ExpiresAt: now.Add(time.Hour).UnixMilli()}
	cfg := &provenance.Configuration{Build: *b.cfg, Trust: trust}
	c, e := provenance.NewCommand(*cfg, r, now)
	if e != nil {
		t.Fatal(e)
	}
	payload, _ := json.Marshal(c)
	signed, e := b.f.Material.Identities["build-controller"].Seal(cfg.Build.Outer, c.ID(), "verify-built", payload)
	if e != nil {
		t.Fatal(e)
	}
	receipt, _ := json.Marshal(signed)
	contextRaw, _ := r.Context.Encode()
	// Synthetic proof isolates journal authorization/atomicity. Crypto and actual
	// archived Chains compatibility are tested independently in provenance.
	proof := provenance.Proof{Version: provenance.PolicyVersion, Scope: provenance.LocalScope, BuildRecordSHA256: r.Digest(), RetainedContextSHA256: authority.Digest(contextRaw), TrustSHA256: trust.Digest(), VerifierImage: trust.VerifierImage, VerifierVersion: provenance.VerifierVersion, ImageDigest: r.Result.ImageDigest, SignatureManifestDigest: "sha256:" + strings.Repeat("1", 64), SignatureBlobDigest: "sha256:" + strings.Repeat("2", 64), AttestationManifestDigest: "sha256:" + strings.Repeat("3", 64), AttestationBlobDigest: "sha256:" + strings.Repeat("4", 64), BuildObservationSHA256: r.Build.Digest(), PublishObservationSHA256: r.Publish.Digest(), Transparency: provenance.LocalTransparency}
	return provenanceFixture{b, cfg, store.NewProvenanceJournal(db, func() (provenance.Configuration, error) { return *cfg, nil }), r, c, payload, receipt, proof}
}
func (p provenanceFixture) claim(t *testing.T) store.ProvenanceClaim {
	t.Helper()
	ctx := context.Background()
	if e := p.j.QueueProvenance(ctx, p.payload, p.receipt); e != nil {
		t.Fatal("queue", e)
	}
	c, e := p.j.ClaimProvenance(ctx, p.command.ID())
	if e != nil {
		t.Fatal("claim", e)
	}
	return c
}
func TestProvenanceJournalExclusiveClaimAndImmutableCompletion(t *testing.T) {
	p := newProvenanceFixture(t)
	ctx := context.Background()
	if e := p.j.QueueProvenance(ctx, p.payload, p.receipt); e != nil {
		t.Fatal(e)
	}
	if p.j.QueueProvenance(ctx, p.payload, p.receipt) == nil {
		t.Fatal("duplicate queue")
	}
	var wg sync.WaitGroup
	claims := make(chan store.ProvenanceClaim, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if c, e := p.j.ClaimProvenance(ctx, p.command.ID()); e == nil {
				claims <- c
			}
		}()
	}
	wg.Wait()
	close(claims)
	var winner store.ProvenanceClaim
	n := 0
	for c := range claims {
		n++
		winner = c
	}
	if n != 1 {
		t.Fatalf("claims=%d", n)
	}
	cfg, r, e := p.j.ReadProvenanceInput(ctx, winner)
	if e != nil || cfg.Trust.Digest() != p.cfg.Trust.Digest() || r.Digest() != p.record.Digest() {
		t.Fatal("input", e)
	}
	if e = p.j.FinishProvenance(ctx, winner, p.proof); e != nil {
		t.Fatal("finish", e)
	}
	record, e := p.j.ReadProvenanceRecord(ctx, winner.ID)
	if e != nil || record.Proof != p.proof {
		t.Fatal("historical record", e)
	}
	p.cfg.Build.Outer.Freeze = true
	again, e := p.j.ReadProvenanceRecord(ctx, winner.ID)
	if e != nil || again.Digest() != record.Digest() {
		t.Fatal("history changed with current policy", e)
	}
	if p.j.FinishProvenance(ctx, winner, p.proof) == nil {
		t.Fatal("completion replay")
	}
	for _, q := range []string{`UPDATE provenance_records SET proof=proof`, `DELETE FROM provenance_records`, `UPDATE provenance_requests SET payload=payload`, `UPDATE provenance_requests SET configuration=configuration`, `UPDATE provenance_requests SET expires_at=clock_timestamp()`} {
		if _, e = p.j.Pool.Exec(q); e == nil {
			t.Fatal("immutable authority mutation", q)
		}
	}
	for _, db := range []*store.DB{p.b.journal.DB, p.b.f.db, p.b.f.Admin} {
		j := store.NewProvenanceJournal(db, func() (provenance.Configuration, error) { return *p.cfg, nil })
		if _, e = j.ReadProvenanceRecord(ctx, winner.ID); e == nil {
			t.Fatal("wrong role read result")
		}
	}
	if e = p.b.f.Admin.ProvisionRolesForBuild(ctx, p.b.passwords, "verification", true, true, true, true); e != nil {
		t.Fatal(e)
	}
	if _, e = p.j.ReadProvenanceRecord(ctx, winner.ID); e == nil {
		t.Fatal("build capability retained provenance access")
	}
}
func TestProvenanceJournalCurrentAuthorityAndAtomicRollback(t *testing.T) {
	for _, name := range []string{"freeze", "trust", "revoked-key", "withdrawn", "bad-proof", "insert-failure", "clock"} {
		t.Run(name, func(t *testing.T) {
			p := newProvenanceFixture(t)
			ctx := context.Background()
			c := p.claim(t)
			proof := p.proof
			switch name {
			case "freeze":
				p.cfg.Build.Inner.Freeze = true
			case "trust":
				p.cfg.Trust.ExpiresAt--
			case "revoked-key":
				delete(p.cfg.Build.Outer.Keys, p.b.f.Material.Identities["build-controller"].KeyID)
			case "withdrawn":
				if _, e := p.b.f.Admin.Pool.Exec(`UPDATE intake_heads SET generation=generation+1`); e != nil {
					t.Fatal(e)
				}
			case "bad-proof":
				proof.ReleaseAuthorized = true
			case "insert-failure":
				if _, e := p.b.f.Admin.Pool.Exec(`REVOKE INSERT(request_id,digest,proof) ON provenance_records FROM aisdlc_verifier`); e != nil {
					t.Fatal(e)
				}
			case "clock":
				c.Until = c.Until.Add(time.Millisecond)
			}
			if p.j.FinishProvenance(ctx, c, proof) == nil {
				t.Fatal("stale or invalid completion")
			}
			if p.b.f.Count(t, `SELECT count(*) FROM provenance_records`) != 0 || p.b.f.Count(t, `SELECT count(*) FROM provenance_requests WHERE state='verified'`) != 0 {
				t.Fatal("partial completion")
			}
		})
	}
}
func TestProvenanceJournalLostAcknowledgementAndRestart(t *testing.T) {
	for _, phase := range []string{"claim", "finish"} {
		t.Run(phase, func(t *testing.T) {
			p := newProvenanceFixture(t)
			ctx := context.Background()
			if e := p.j.QueueProvenance(ctx, p.payload, p.receipt); e != nil {
				t.Fatal(e)
			}
			var c store.ProvenanceClaim
			var e error
			if phase == "finish" {
				c, e = p.j.ClaimProvenance(ctx, p.command.ID())
				if e != nil {
					t.Fatal(e)
				}
			}
			db, dropped := executionCommitFault(t, 1, "verifier")
			j := store.NewProvenanceJournal(db, func() (provenance.Configuration, error) { return *p.cfg, nil })
			if phase == "claim" {
				returned, e := j.ClaimProvenance(ctx, p.command.ID())
				if e == nil || returned.Token != "" {
					t.Fatal("uncertain claim exposed")
				}
			} else if j.FinishProvenance(ctx, c, p.proof) == nil {
				t.Fatal("lost finish acknowledgement")
			}
			select {
			case <-dropped:
			default:
				t.Fatal("COMMIT response not dropped")
			}
			restarted := store.NewProvenanceJournal(p.j.DB, func() (provenance.Configuration, error) { return *p.cfg, nil })
			if _, e = restarted.ClaimProvenance(ctx, p.command.ID()); e == nil {
				t.Fatal("restart replay")
			}
			if phase == "finish" {
				r, e := restarted.ReadProvenanceRecord(ctx, p.command.ID())
				if e != nil || r.Proof != p.proof {
					t.Fatal("durable proof lost", e)
				}
			}
		})
	}
}

func TestProvenanceJournalRealExpiryDoesNotRenewHistory(t *testing.T) {
	p := newProvenanceFixture(t)
	ctx := context.Background()
	// Preserve the real 65-second claim budget, then let wall-clock authority
	// expire. No test SQL rewrites timestamps or issues a replacement command.
	p.command.ExpiresAt = time.Now().Add(68 * time.Second).UnixMilli()
	p.payload, _ = json.Marshal(p.command)
	signed, e := p.b.f.Material.Identities["build-controller"].Seal(p.cfg.Build.Outer, p.command.ID(), "verify-built", p.payload)
	if e != nil {
		t.Fatal(e)
	}
	p.receipt, _ = json.Marshal(signed)
	c := p.claim(t)
	time.Sleep(time.Until(c.Until.Add(5 * time.Millisecond)))
	if _, _, e = p.j.ReadProvenanceInput(ctx, c); e == nil {
		t.Fatal("expired authority read input")
	}
	if p.j.FinishProvenance(ctx, c, p.proof) == nil {
		t.Fatal("expired authority completed")
	}
	if e = p.j.ExpireProvenance(ctx); e != nil {
		t.Fatal(e)
	}
	if p.b.f.Count(t, `SELECT count(*) FROM provenance_requests WHERE state='failed' AND reason='expired'`) != 1 || p.b.f.Count(t, `SELECT count(*) FROM provenance_records`) != 0 {
		t.Fatal("expiration state")
	}
	if _, e = p.j.ClaimProvenance(ctx, c.ID); e == nil {
		t.Fatal("expired claim renewed")
	}
	// A differently timed signed command still cannot reuse the same build.
	now := time.Now()
	next, e := provenance.NewCommand(*p.cfg, p.record, now)
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(next)
	s, e := p.b.f.Material.Identities["build-controller"].Seal(p.cfg.Build.Outer, next.ID(), "verify-built", raw)
	if e != nil {
		t.Fatal(e)
	}
	receipt, _ := json.Marshal(s)
	if p.j.QueueProvenance(ctx, raw, receipt) == nil {
		t.Fatal("another command renewed consumed build")
	}
}

func TestProvenanceJournalRevocationDuringBlockedHistory(t *testing.T) {
	for _, phase := range []string{"queue", "claim", "input", "finish"} {
		t.Run(phase, func(t *testing.T) {
			p := newProvenanceFixture(t)
			ctx := context.Background()
			var claim store.ProvenanceClaim
			if phase == "input" || phase == "finish" {
				claim = p.claim(t)
			} else if phase == "claim" {
				if e := p.j.QueueProvenance(ctx, p.payload, p.receipt); e != nil {
					t.Fatal(e)
				}
			}
			// Synchronize configuration snapshots without racing on shared policy maps.
			var mu sync.Mutex
			j := store.NewProvenanceJournal(p.j.DB, func() (provenance.Configuration, error) { mu.Lock(); defer mu.Unlock(); return *p.cfg, nil })
			tx, e := p.b.f.Admin.Pool.BeginTx(ctx, nil)
			if e != nil {
				t.Fatal(e)
			}
			defer tx.Rollback()
			if _, e = tx.Exec(`LOCK TABLE build_contexts IN ACCESS EXCLUSIVE MODE`); e != nil {
				t.Fatal(e)
			}
			var blocker int
			if e = tx.QueryRow(`SELECT pg_backend_pid()`).Scan(&blocker); e != nil {
				t.Fatal(e)
			}
			done := make(chan error, 1)
			go func() {
				var e error
				switch phase {
				case "queue":
					e = j.QueueProvenance(ctx, p.payload, p.receipt)
				case "claim":
					var c store.ProvenanceClaim
					c, e = j.ClaimProvenance(ctx, p.command.ID())
					if c.Token != "" {
						e = nil
					}
				case "input":
					var cfg provenance.Configuration
					var r store.BuiltRecord
					cfg, r, e = j.ReadProvenanceInput(ctx, claim)
					if cfg.Trust.Version != "" || r.Command.Version != "" {
						e = nil
					}
				case "finish":
					e = j.FinishProvenance(ctx, claim, p.proof)
				}
				done <- e
			}()
			blocked := false
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				if e = p.b.f.Admin.Pool.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE usename='aisdlc_verifier' AND wait_event_type='Lock' AND $1=ANY(pg_blocking_pids(pid)))`, blocker).Scan(&blocked); e != nil {
					t.Fatal(e)
				}
				if blocked {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if !blocked {
				t.Fatal("verifier did not observably block on historical context")
			}
			mu.Lock()
			p.cfg.Build.Outer.Freeze = true
			mu.Unlock()
			if e = tx.Commit(); e != nil {
				t.Fatal(e)
			}
			if e = <-done; e == nil {
				t.Fatal("revocation during history read exposed authority or succeeded")
			}
			if p.b.f.Count(t, `SELECT count(*) FROM provenance_records`) != 0 {
				t.Fatal("revoked completion persisted")
			}
			if phase == "claim" && p.b.f.Count(t, `SELECT count(*) FROM provenance_requests WHERE state='claimed'`) != 0 {
				t.Fatal("revoked claim committed")
			}
			if phase == "queue" && p.b.f.Count(t, `SELECT count(*) FROM provenance_requests`) != 0 {
				t.Fatal("revoked queue committed")
			}
		})
	}
}
