package execution

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/intake"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/verification"
)

// This fixture uses a captured candidate and the real signed worker outbox;
// explicit verifier grants remain confined to the isolated test database.
func verificationClaimFixture(t *testing.T) (*lifecycleFixture, *store.DB, store.VerificationRequest) {
	t.Helper()
	f, l, _, raw := candidateFixture(t, func(f *fixture) {
		f.Material.Policy.Repository = "os-santiago/homedir"
		f.Config.Repository = f.Material.Policy.Repository
		f.command.Repository = f.Material.Policy.Repository
		f.command.AuthorityRevision = f.Material.Policy.Revision()
		snapshot := f.Snapshot(1, true)
		snapshot.Issue.Body += "\n" + verification.RecipeMarker + "\n"
		snapshot.Revision = snapshot.ContentRevision()
		f.seedReadinessSnapshot(t, snapshot)
	})
	ctx := context.Background()
	captured, err := l.CaptureCandidate(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.Admin.Pool.Exec(`GRANT SELECT,INSERT ON verification_handoffs TO aisdlc_worker; GRANT UPDATE(cleanup_state) ON execution_lifecycles TO aisdlc_worker; GRANT SELECT ON verification_handoffs,execution_candidates TO aisdlc_verifier; GRANT SELECT,INSERT,UPDATE ON verification_requests,verification_effects TO aisdlc_verifier`); err != nil {
		t.Fatal(err)
	}
	var config, pod string
	if err = f.Admin.Pool.QueryRow(`SELECT config_uid,pod_uid FROM execution_lifecycles WHERE request_id=$1`, captured.RequestID).Scan(&config, &pod); err != nil {
		t.Fatal(err)
	}
	if err = f.db.ConfirmExecutionCleanup(ctx, l.durable, config, pod); err != nil {
		t.Fatal(err)
	}
	sent, err := HandoffCandidate(ctx, f.db, func() (authority.Policy, authority.Identity, intake.Config, string, error) {
		return f.Material.Policy, f.Material.Identities["worker"], f.Config, f.command.ProfileSHA256, nil
	})
	if err != nil || !sent {
		t.Fatal("handoff", err)
	}
	var r store.VerificationRequest
	if err = f.Admin.Pool.QueryRow(`SELECT payload,receipt FROM authorized_outbox WHERE operation='verify-artifact'`).Scan(&r.Payload, &r.Receipt); err != nil {
		t.Fatal(err)
	}
	var receipt authority.Receipt
	if json.Unmarshal(r.Receipt, &receipt) != nil {
		t.Fatal("receipt")
	}
	c, err := verification.Verify(f.Material.Policy, receipt, r.Payload, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	r.ID = c.ID()
	r.CandidateID = c.CandidateID
	r.PayloadDigest = authority.Digest(r.Payload)
	r.ExpiresAt = time.UnixMilli(c.ExpiresAt)
	u, err := url.Parse(os.Getenv("AI_SDLC_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword("aisdlc_verifier", "verifier"+strings.Repeat("q", 40))
	db, err := store.Open(ctx, u.String())
	if err != nil {
		t.Fatal("verifier connection")
	}
	t.Cleanup(func() { db.Close() })
	if err = db.WithTx(ctx, func(tx *sql.Tx) error { return store.InsertVerification(ctx, tx, r) }); err != nil {
		t.Fatal("enqueue", err)
	}
	return f, db, r
}
func TestVerificationExclusiveClaimEffectsAndFinish(t *testing.T) {
	f, db, r := verificationClaimFixture(t)
	ctx := context.Background()
	if _, err := f.db.ClaimVerification(ctx, r.ID, r.PayloadDigest); err == nil {
		t.Fatal("worker claimed verification")
	}
	var wg sync.WaitGroup
	winners := make(chan store.VerificationClaim, 12)
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if c, e := db.ClaimVerification(ctx, r.ID, r.PayloadDigest); e == nil {
				winners <- c
			}
		}()
	}
	wg.Wait()
	close(winners)
	var c store.VerificationClaim
	count := 0
	for winner := range winners {
		c = winner
		count++
	}
	if count != 1 || c.Until.After(time.Now().Add(181*time.Second)) {
		t.Fatal("nonexclusive or unbounded claim", count)
	}
	wrong := c
	wrong.Token = store.NewID()
	if _, err := db.BeginVerificationEffect(ctx, wrong, "config"); err == nil {
		t.Fatal("foreign token")
	}
	if _, err := db.BeginVerificationEffect(ctx, c, "candidate"); err == nil {
		t.Fatal("pod before config")
	}
	for _, kind := range []string{"config", "baseline", "candidate", "reference", "tests"} {
		e, err := db.BeginVerificationEffect(ctx, c, kind)
		if err != nil {
			t.Fatal(kind, err)
		}
		if _, err = db.BeginVerificationEffect(ctx, c, kind); err == nil {
			t.Fatal("effect replay")
		}
		if err = db.ConfirmVerificationCleanup(ctx, c); err == nil {
			t.Fatal("unacknowledged effect cleaned")
		}
		uid := store.NewID()
		if err = db.AcknowledgeVerificationEffect(ctx, e, uid); err != nil {
			t.Fatal("ack", err)
		}
		if err = db.AcknowledgeVerificationEffect(ctx, e, store.NewID()); err == nil {
			t.Fatal("UID replaced")
		}
	}
	proof := []byte(`{"behaviorVerified":true,"publicationAuthorized":false}`)
	if db.FinishVerification(ctx, c, proof) == nil {
		t.Fatal("finished before cleanup")
	}
	if db.ConfirmVerificationCleanup(ctx, c) != nil || db.FinishVerification(ctx, c, proof) != nil {
		t.Fatal("finish")
	}
	if db.CheckVerificationClaim(ctx, c) == nil || db.FinishVerification(ctx, c, proof) == nil {
		t.Fatal("terminal claim reused")
	}
}
func TestVerificationExpiredClaimDoesNotAdoptUnknownResources(t *testing.T) {
	f, db, r := verificationClaimFixture(t)
	ctx := context.Background()
	c, err := db.ClaimVerification(ctx, r.ID, r.PayloadDigest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.BeginVerificationEffect(ctx, c, "config"); err != nil {
		t.Fatal(err)
	}
	// Crash after intent, before a UID acknowledgement. Only fixture admin moves time.
	if _, err = f.Admin.Pool.Exec(`UPDATE verification_requests SET claim_until=clock_timestamp()-interval '1 second' WHERE id=$1`, r.ID); err != nil {
		t.Fatal(err)
	}
	if db.ExpireVerifications(ctx) != nil {
		t.Fatal("expiry")
	}
	if _, err = db.ClaimVerification(ctx, r.ID, r.PayloadDigest); err == nil {
		t.Fatal("expired claim adopted")
	}
	cleanup, err := db.NextVerificationCleanup(ctx)
	if err != nil || cleanup.Claim.ID != r.ID {
		t.Fatal("recovery", err)
	}
	if db.FinishVerificationCleanup(ctx, cleanup, true) != nil {
		t.Fatal("cleanup disposition")
	}
	var state string
	if f.Admin.Pool.QueryRow(`SELECT cleanup_state FROM verification_requests WHERE id=$1`, r.ID).Scan(&state) != nil || state != "uncertain" {
		t.Fatal("unknown resource considered removed", state)
	}
	blocked, err := db.VerificationCleanupBlocked(ctx)
	if err != nil || !blocked {
		t.Fatal("uncertainty must block")
	}
	again, err := db.NextVerificationCleanup(ctx)
	if err != nil || again.Claim.ID != "" {
		t.Fatal("uncertain cleanup replay")
	}
}
func TestVerificationCleanupAttemptsAreBounded(t *testing.T) {
	f, db, r := verificationClaimFixture(t)
	ctx := context.Background()
	c, err := db.ClaimVerification(ctx, r.ID, r.PayloadDigest)
	if err != nil {
		t.Fatal(err)
	}
	e, err := db.BeginVerificationEffect(ctx, c, "config")
	if err != nil {
		t.Fatal(err)
	}
	if db.AcknowledgeVerificationEffect(ctx, e, store.NewID()) != nil || db.StopVerification(ctx, c, "uncertain") != nil {
		t.Fatal("stop")
	}
	for attempt := 1; attempt <= 3; attempt++ {
		cleanup, err := db.NextVerificationCleanup(ctx)
		if err != nil || cleanup.Attempt != attempt {
			t.Fatal("attempt", attempt, err)
		}
		next, err := db.NextVerificationCleanup(ctx)
		if err != nil || next.Claim.ID != "" {
			t.Fatal("cooldown bypass")
		}
		if db.FinishVerificationCleanup(ctx, cleanup, false) != nil {
			t.Fatal("failed cleanup")
		}
		if attempt < 3 {
			if _, err = f.Admin.Pool.Exec(`UPDATE verification_requests SET cleanup_next_at=clock_timestamp() WHERE id=$1`, r.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	var state string
	if f.Admin.Pool.QueryRow(`SELECT cleanup_state FROM verification_requests WHERE id=$1`, r.ID).Scan(&state) != nil || state != "uncertain" {
		t.Fatal("retry bound", state)
	}
	next, err := db.NextVerificationCleanup(ctx)
	if err != nil || next.Claim.ID != "" {
		t.Fatal("fourth cleanup")
	}
}

func TestVerificationPrivilegesAreIndependentAndRevocable(t *testing.T) {
	f, db, r := verificationClaimFixture(t)
	ctx := context.Background()
	passwords := map[string]string{}
	for role := range f.Material.Identities {
		passwords[role] = strings.ReplaceAll(role, "-", "_") + strings.Repeat("q", 40)
	}
	if err := f.Admin.ProvisionRolesForMode(ctx, passwords, "verification"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ReadVerification(ctx, r.ID); err != nil {
		t.Fatal("verification role unavailable", err)
	}
	if _, err := db.ReadVerificationCandidate(ctx, r.CandidateID); err != nil {
		t.Fatal("candidate evidence unavailable", err)
	}
	for _, query := range []string{`UPDATE execution_candidates SET delta='changed' WHERE false`, `UPDATE intake_observations SET observed_at=clock_timestamp() WHERE false`, `UPDATE verification_requests SET payload='changed' WHERE false`, `DELETE FROM verification_requests WHERE false`, `UPDATE verification_effects SET resource_name='changed' WHERE false`, `SELECT * FROM managed_component_roles`, `UPDATE release_gates SET state='verified' WHERE false`} {
		if _, err := db.Pool.Exec(query); err == nil {
			t.Fatal("forbidden verifier privilege", query)
		}
	}
	if _, err := f.db.Pool.Exec(`SELECT * FROM verification_handoffs`); err != nil {
		t.Fatal("worker handoff unavailable", err)
	}
	if err := f.Admin.ProvisionRolesForMode(ctx, passwords, "candidate"); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{`SELECT * FROM verification_requests`, `UPDATE verification_requests SET state='failed' WHERE false`, `UPDATE verification_effects SET resource_uid=NULL WHERE false`, `SELECT * FROM execution_candidates`} {
		if _, err := db.Pool.Exec(query); err == nil {
			t.Fatal("downgrade retained verifier privilege", query)
		}
	}
	if _, err := f.db.Pool.Exec(`SELECT * FROM verification_handoffs`); err == nil {
		t.Fatal("candidate profile retained handoff")
	}
}

func TestVerificationUnsentEffectCanBeSealedAfterExpiryWithoutReplay(t *testing.T) {
	f, db, r := verificationClaimFixture(t)
	ctx := context.Background()
	// Only fixture admin bounds time; production cannot renew an execution.
	if _, err := f.Admin.Pool.Exec(`UPDATE verification_requests SET expires_at=clock_timestamp()+interval '400 milliseconds' WHERE id=$1`, r.ID); err != nil {
		t.Fatal(err)
	}
	c, err := db.ClaimVerification(ctx, r.ID, r.PayloadDigest)
	if err != nil {
		t.Fatal(err)
	}
	e, err := db.BeginVerificationEffect(ctx, c, "config")
	if err != nil {
		t.Fatal(err)
	}
	wrong := e
	wrong.Token = store.NewID()
	if db.CancelUnsentVerificationEffect(ctx, wrong) == nil {
		t.Fatal("foreign effect cancelled")
	}
	if f.db.CancelUnsentVerificationEffect(ctx, e) == nil {
		t.Fatal("worker cancelled verifier effect")
	}
	if db.ConfirmVerificationCleanup(ctx, c) == nil {
		t.Fatal("unknown effect accepted as no-send")
	}
	time.Sleep(time.Until(c.Until) + 10*time.Millisecond)
	if db.CheckVerificationClaim(ctx, c) == nil {
		t.Fatal("claim still valid")
	}
	if db.CancelUnsentVerificationEffect(ctx, e) != nil {
		t.Fatal("cannot seal no-send after expiry")
	}
	if db.CancelUnsentVerificationEffect(ctx, e) == nil {
		t.Fatal("cancel replay")
	}
	if db.AcknowledgeVerificationEffect(ctx, e, store.NewID()) == nil {
		t.Fatal("cancelled effect acknowledged")
	}
	if _, err = db.BeginVerificationEffect(ctx, c, "config"); err == nil {
		t.Fatal("cancelled effect replayed")
	}
	if db.ConfirmVerificationCleanup(ctx, c) != nil {
		t.Fatal("known no-send blocks cleanup")
	}
	effects, err := db.ReadVerificationEffects(ctx, c)
	if err != nil || len(effects) != 1 || !effects[0].NotSent || effects[0].UID != "" {
		t.Fatal("durable no-send lost")
	}
	if db.FinishVerification(ctx, c, []byte(`{"behaviorVerified":true}`)) == nil {
		t.Fatal("no-send authorized success")
	}
	if db.ExpireVerifications(ctx) != nil {
		t.Fatal("expiry")
	}
	if blocked, err := db.VerificationCleanupBlocked(ctx); err != nil || blocked {
		t.Fatal("no-send blocked next work")
	}
}

func TestLiveVerificationClaimCannotReuseCancelledEffect(t *testing.T) {
	_, db, r := verificationClaimFixture(t)
	ctx := context.Background()
	c, err := db.ClaimVerification(ctx, r.ID, r.PayloadDigest)
	if err != nil {
		t.Fatal(err)
	}
	e, err := db.BeginVerificationEffect(ctx, c, "config")
	if err != nil {
		t.Fatal(err)
	}
	if db.CancelUnsentVerificationEffect(ctx, e) != nil || db.CheckVerificationClaim(ctx, c) != nil {
		t.Fatal("cancel unexpectedly changed live claim")
	}
	if db.AcknowledgeVerificationEffect(ctx, e, store.NewID()) == nil {
		t.Fatal("cancelled effect acknowledged during valid claim")
	}
	if _, err = db.BeginVerificationEffect(ctx, c, "config"); err == nil {
		t.Fatal("cancelled effect replayed during valid claim")
	}
	if db.ConfirmVerificationCleanup(ctx, c) != nil {
		t.Fatal("no-send cleanup")
	}
	if db.FinishVerification(ctx, c, []byte(`{"behaviorVerified":true}`)) == nil {
		t.Fatal("no-send produced success")
	}
}
