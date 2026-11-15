package execution

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

func reviewJournalFixture(t *testing.T, profileOverride ...string) (*lifecycleFixture, *store.DB, store.ReviewRun) {
	t.Helper()
	f, db, v := verificationClaimFixture(t)
	ctx := context.Background()
	if _, err := f.Admin.Pool.Exec(`GRANT SELECT ON execution_requests TO aisdlc_verifier; GRANT SELECT(repository,issue_number,generation,content_key) ON intake_heads TO aisdlc_verifier; GRANT SELECT,INSERT,UPDATE ON review_runs,review_effects,review_model_calls TO aisdlc_verifier`); err != nil {
		t.Fatal(err)
	}
	proof := []byte(`{"behaviorVerified":true,"publicationAuthorized":false}`)
	profile, config, assignment := strings.Repeat("b", 64), strings.Repeat("c", 64), strings.Repeat("d", 64)
	if len(profileOverride) == 1 {
		profile = profileOverride[0]
	}
	if _, err := db.QueueReview(ctx, v.ID, authority.Digest(proof), profile, config, assignment); err == nil {
		t.Fatal("unverified candidate queued")
	}
	c, err := db.ClaimVerification(ctx, v.ID, v.PayloadDigest)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"config", "baseline", "candidate", "reference", "tests"} {
		e, err := db.BeginVerificationEffect(ctx, c, kind)
		if err != nil {
			t.Fatal(err)
		}
		if db.AcknowledgeVerificationEffect(ctx, e, store.NewID()) != nil {
			t.Fatal("synthetic verification effect")
		}
	}
	if db.ConfirmVerificationCleanup(ctx, c) != nil || db.FinishVerification(ctx, c, proof) != nil {
		t.Fatal("synthetic behavior proof")
	}
	if _, err := db.QueueReview(ctx, v.ID, strings.Repeat("a", 64), profile, config, assignment); err == nil {
		t.Fatal("changed behavior proof queued")
	}
	r, err := db.QueueReview(ctx, v.ID, authority.Digest(proof), profile, config, assignment)
	if err != nil {
		t.Fatal("review queue", err)
	}
	if _, err := db.QueueReview(ctx, v.ID, authority.Digest(proof), profile, config, assignment); err == nil {
		t.Fatal("review reset")
	}
	return f, db, r
}
func claimReview(t *testing.T, db *store.DB, r store.ReviewRun) store.ReviewClaim {
	t.Helper()
	c, e := db.ClaimReview(context.Background(), r.ID, r.ConfigurationDigest)
	if e != nil {
		t.Fatal(e)
	}
	return c
}
func startReviewFixture(t *testing.T, db *store.DB, c store.ReviewClaim) string {
	t.Helper()
	ctx := context.Background()
	pod := store.NewID()
	for _, kind := range []string{"config", "pod", "import", "start"} {
		e, err := db.BeginReviewEffect(ctx, c, kind)
		if err != nil {
			t.Fatal(kind, err)
		}
		uid, digest := pod, ""
		if kind == "config" {
			uid = store.NewID()
		}
		if kind == "import" {
			digest = strings.Repeat("e", 64)
		}
		if err = db.AcknowledgeReviewEffect(ctx, e, uid, digest); err != nil {
			t.Fatal("ack "+kind, err)
		}
	}
	return pod
}
func TestReviewJournalExclusiveClaimsAndGenerationIsolation(t *testing.T) {
	f, db, r := reviewJournalFixture(t)
	ctx := context.Background()
	initialGenerationCalls := f.Count(t, `SELECT count(*) FROM model_calls`)
	if _, err := f.db.ClaimReview(ctx, r.ID, r.ConfigurationDigest); err == nil {
		t.Fatal("generator claimed reviewer")
	}
	if _, err := db.ClaimReview(ctx, r.ID, strings.Repeat("f", 64)); err == nil {
		t.Fatal("configuration substitution")
	}
	var wg sync.WaitGroup
	winners := make(chan store.ReviewClaim, 12)
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if c, e := db.ClaimReview(ctx, r.ID, r.ConfigurationDigest); e == nil {
				winners <- c
			}
		}()
	}
	wg.Wait()
	close(winners)
	var c store.ReviewClaim
	count := 0
	for v := range winners {
		c = v
		count++
	}
	if count != 1 || c.Until.After(time.Now().Add(301*time.Second)) || c.Until.After(r.ExpiresAt) {
		t.Fatal("unbounded/nonexclusive claim")
	}
	if _, e := db.BeginReviewEffect(ctx, c, "pod"); e == nil {
		t.Fatal("pod before config")
	}
	wrong := c
	wrong.Token = store.NewID()
	if _, e := db.BeginReviewEffect(ctx, wrong, "config"); e == nil {
		t.Fatal("foreign claim")
	}
	pod := startReviewFixture(t, db, c)
	if _, e := db.BeginReviewEffect(ctx, c, "start"); e == nil {
		t.Fatal("start replay")
	}
	if _, e := db.BeginReviewEffect(ctx, c, "export"); e == nil {
		t.Fatal("export without model evidence")
	}
	request := []byte(`{"model":"fixture-model","messages":[{"role":"user","content":"review"}]}`)
	call, e := db.BeginReviewModelCall(ctx, c, pod, 1, request)
	if e != nil {
		t.Fatal(e)
	}
	if _, e := db.BeginReviewModelCall(ctx, c, pod, 1, request); e == nil {
		t.Fatal("provider call replay")
	}
	if _, e := db.BeginReviewModelCall(ctx, c, pod, 2, request); e == nil {
		t.Fatal("new call bypassed uncertain predecessor")
	}
	response := []byte(`{"choices":[{"message":{"role":"assistant","content":"VERDICT: approve"}}]}`)
	observation := []byte(`{"scope":"synthetic-store-test","verdict":"approve"}`)
	if db.CompleteReviewModelCall(ctx, call, response, observation) != nil {
		t.Fatal("completion")
	}
	if _, e := db.ReadReviewModelEvidence(ctx, c); e == nil {
		t.Fatal("unconfirmed handoff became evidence")
	}
	if db.AcknowledgeReviewModelCall(ctx, call, strings.Repeat("f", 64)) == nil {
		t.Fatal("substituted response")
	}
	if db.AcknowledgeReviewModelCall(ctx, call, authority.Digest(response)) != nil {
		t.Fatal("handoff")
	}
	if db.AcknowledgeReviewModelCall(ctx, call, authority.Digest(response)) == nil {
		t.Fatal("handoff replay")
	}
	evidence, e := db.ReadReviewModelEvidence(ctx, c)
	if e != nil || len(evidence) != 1 || evidence[0].ResponseDigest != authority.Digest(response) {
		t.Fatal("durable evidence", e)
	}
	export, e := db.BeginReviewEffect(ctx, c, "export")
	if e != nil {
		t.Fatal(e)
	}
	if db.AcknowledgeReviewEffect(ctx, export, pod, strings.Repeat("e", 64)) != nil {
		t.Fatal("export receipt")
	}
	if _, e := db.BeginReviewModelCall(ctx, c, pod, 2, request); e == nil {
		t.Fatal("model continued after export")
	}
	if db.ConfirmReviewCleanup(ctx, c) != nil {
		t.Fatal("known resource cleanup")
	}
	if db.CheckReviewClaim(ctx, c) != nil {
		t.Fatal("cleanup should not fabricate result")
	}
	var generationCalls int
	if f.Admin.Pool.QueryRow(`SELECT count(*) FROM model_calls`).Scan(&generationCalls) != nil || generationCalls != initialGenerationCalls {
		t.Fatal("review reused generation model ledger")
	}
}
func TestReviewJournalUnknownCreateAndNoSendCleanup(t *testing.T) {
	for _, unsent := range []bool{false, true} {
		t.Run(map[bool]string{true: "not-sent", false: "lost-reply"}[unsent], func(t *testing.T) {
			_, db, r := reviewJournalFixture(t)
			ctx := context.Background()
			c := claimReview(t, db, r)
			e, err := db.BeginReviewEffect(ctx, c, "config")
			if err != nil {
				t.Fatal(err)
			}
			if unsent {
				if db.CancelUnsentReviewEffect(ctx, e) != nil {
					t.Fatal("no-send seal")
				}
				if db.AcknowledgeReviewEffect(ctx, e, store.NewID(), "") == nil {
					t.Fatal("canceled intent acknowledged")
				}
			}
			if db.StopReview(ctx, c, "uncertain") != nil {
				t.Fatal("stop")
			}
			cleanup, err := db.NextReviewCleanup(ctx)
			if err != nil || cleanup.Claim.ID != c.ID {
				t.Fatal("cleanup claim", err)
			}
			if db.FinishReviewCleanup(ctx, cleanup, true) != nil {
				t.Fatal("cleanup disposition")
			}
			blocked, err := db.ReviewCleanupBlocked(ctx)
			if err != nil || blocked == unsent {
				t.Fatal("unknown resource barrier", blocked, err)
			}
			if _, err := db.ClaimReview(ctx, r.ID, r.ConfigurationDigest); err == nil {
				t.Fatal("terminal review replay")
			}
		})
	}
}
func TestReviewJournalBudgetsAndForeignPod(t *testing.T) {
	_, db, r := reviewJournalFixture(t)
	ctx := context.Background()
	c := claimReview(t, db, r)
	pod := startReviewFixture(t, db, c)
	request := []byte(`{"model":"fixture-model"}`)
	response := []byte(`{"fixture":true}`)
	obs := []byte(`{"tools":["read_file"]}`)
	if _, err := db.BeginReviewModelCall(ctx, c, store.NewID(), 1, request); err == nil {
		t.Fatal("foreign pod")
	}
	for sequence := 1; sequence <= 8; sequence++ {
		call, e := db.BeginReviewModelCall(ctx, c, pod, sequence, request)
		if e != nil {
			t.Fatal(sequence, e)
		}
		if db.CompleteReviewModelCall(ctx, call, response, obs) != nil || db.AcknowledgeReviewModelCall(ctx, call, authority.Digest(response)) != nil {
			t.Fatal("bounded model cycle")
		}
	}
	if _, e := db.BeginReviewModelCall(ctx, c, pod, 9, request); e == nil {
		t.Fatal("call budget reset")
	}
	if _, e := db.BeginReviewModelCall(ctx, c, pod, 1, request); e == nil {
		t.Fatal("old sequence replay")
	}
}
func TestReviewJournalExpiresWhileCallRowIsLocked(t *testing.T) {
	f, db, r := reviewJournalFixture(t)
	ctx := context.Background()
	c := claimReview(t, db, r)
	pod := startReviewFixture(t, db, c)
	call, e := db.BeginReviewModelCall(ctx, c, pod, 1, []byte(`{"model":"fixture-model"}`))
	if e != nil {
		t.Fatal(e)
	}
	if e = f.Admin.Pool.QueryRow(`UPDATE review_runs SET claim_until=clock_timestamp()+interval '400 milliseconds' WHERE id=$1 RETURNING claim_until`, c.ID).Scan(&c.Until); e != nil {
		t.Fatal(e)
	}
	call.Claim = c
	blocker, e := f.Admin.Pool.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer blocker.Rollback()
	if _, e = blocker.Exec(`SELECT 1 FROM review_model_calls WHERE run_id=$1 FOR UPDATE`, c.ID); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- db.CompleteReviewModelCall(ctx, call, []byte(`{"ok":true}`), []byte(`{"tools":[]}`)) }()
	time.Sleep(500 * time.Millisecond)
	if blocker.Commit() != nil {
		t.Fatal("unlock")
	}
	if e = <-done; e == nil {
		t.Fatal("expired call completed after lock wait")
	}
	var state string
	if f.Admin.Pool.QueryRow(`SELECT state FROM review_model_calls WHERE run_id=$1`, c.ID).Scan(&state) != nil || state != "started" {
		t.Fatal("expired write did not roll back")
	}
	if db.ExpireReviews(ctx) != nil {
		t.Fatal("expiry sweep")
	}
	if db.CheckReviewClaim(ctx, c) == nil {
		t.Fatal("expired claim usable")
	}
}

func TestReviewJournalLostCommitNeverAuthorizesInvocationOrReplay(t *testing.T) {
	for _, stage := range []string{"claim", "intent", "model", "completion", "handoff"} {
		t.Run(stage, func(t *testing.T) {
			f, db, r := reviewJournalFixture(t)
			ctx := context.Background()
			faulted, dropped := executionCommitFault(t, 1, "verifier")
			if stage == "claim" {
				claim, err := faulted.ClaimReview(ctx, r.ID, r.ConfigurationDigest)
				if err == nil || claim.ID != "" {
					t.Fatal("lost claim commit returned authority")
				}
			} else {
				c := claimReview(t, db, r)
				if stage == "intent" {
					e, err := faulted.BeginReviewEffect(ctx, c, "config")
					if err == nil || e.Token != "" {
						t.Fatal("lost intent returned effect authority")
					}
					if _, err = db.BeginReviewEffect(ctx, c, "config"); err == nil {
						t.Fatal("lost intent replay")
					}
				} else {
					pod := startReviewFixture(t, db, c)
					req := []byte(`{"model":"fixture-model"}`)
					response := []byte(`{"fixture":true}`)
					obs := []byte(`{"tools":["read_file"]}`)
					if stage == "model" {
						call, err := faulted.BeginReviewModelCall(ctx, c, pod, 1, req)
						if err == nil || call.Token != "" {
							t.Fatal("lost model commit authorized call")
						}
					} else {
						call, err := db.BeginReviewModelCall(ctx, c, pod, 1, req)
						if err != nil {
							t.Fatal(err)
						}
						if stage == "completion" {
							if faulted.CompleteReviewModelCall(ctx, call, response, obs) == nil {
								t.Fatal("lost completion acknowledged")
							}
						} else {
							if db.CompleteReviewModelCall(ctx, call, response, obs) != nil {
								t.Fatal("complete")
							}
							if faulted.AcknowledgeReviewModelCall(ctx, call, authority.Digest(response)) == nil {
								t.Fatal("lost handoff acknowledged")
							}
						}
					}
					if _, err := db.BeginReviewModelCall(ctx, c, pod, 1, req); err == nil {
						t.Fatal("uncertain provider invocation replayed")
					}
					// The controller stops its parent on every uncertain outcome,
					// including a handoff whose server commit may have succeeded.
					if db.StopReview(ctx, c, "uncertain") != nil {
						t.Fatal("terminal stop")
					}
					if _, err := db.ReadReviewModelEvidence(ctx, c); err == nil {
						t.Fatal("stopped review produced evidence")
					}
				}
			}
			select {
			case <-dropped:
			case <-time.After(time.Second):
				t.Fatal("real COMMIT reply not dropped")
			}
			var rows int
			if f.Admin.Pool.QueryRow(`SELECT count(*) FROM review_runs`).Scan(&rows) != nil || rows != 1 {
				t.Fatal("journal duplicated")
			}
		})
	}
}

func TestReviewJournalNotGrantedByInstalledProfiles(t *testing.T) {
	f, db, _ := reviewJournalFixture(t)
	passwords := map[string]string{}
	for role := range f.Material.Identities {
		passwords[role] = strings.ReplaceAll(role, "-", "_") + strings.Repeat("q", 40)
	}
	if f.Admin.ProvisionRolesForMode(context.Background(), passwords, "verification") != nil {
		t.Fatal("verification profile")
	}
	for _, table := range []string{"review_runs", "review_effects", "review_model_calls"} {
		if _, err := db.Pool.Exec(`SELECT * FROM ` + table); err == nil {
			t.Fatal("inactive review granted by installed profile")
		}
		if _, err := f.db.Pool.Exec(`SELECT * FROM ` + table); err == nil {
			t.Fatal("generation identity sees review ledger")
		}
	}
}

func TestInstalledReviewPrivilegesAreExplicitScopedAndRevocable(t *testing.T) {
	f, db, r := reviewJournalFixture(t)
	ctx := context.Background()
	passwords := map[string]string{}
	for role := range f.Material.Identities {
		passwords[role] = strings.ReplaceAll(role, "-", "_") + strings.Repeat("q", 40)
	}
	if f.Admin.ProvisionRolesForOptions(ctx, passwords, "candidate", true) == nil {
		t.Fatal("review enabled without verification")
	}
	if e := f.Admin.ProvisionRolesForOptions(ctx, passwords, "verification", true); e != nil {
		t.Fatal(e)
	}
	if _, e := db.ReadReview(ctx, r.ID); e != nil {
		t.Fatal("review not readable", e)
	}
	for _, query := range []string{`UPDATE review_runs SET candidate_id='forged' WHERE false`, `UPDATE review_effects SET resource_name='forged' WHERE false`, `UPDATE review_model_calls SET request_digest=repeat('a',64) WHERE false`, `DELETE FROM review_runs WHERE false`, `UPDATE execution_candidates SET delta='forged' WHERE false`, `UPDATE release_gates SET state='verified' WHERE false`, `SELECT * FROM managed_component_roles`} {
		if _, e := db.Pool.Exec(query); e == nil {
			t.Fatal("excess review privilege", query)
		}
	}
	if _, e := f.db.Pool.Exec(`SELECT * FROM review_runs`); e == nil {
		t.Fatal("generation identity reads review")
	}
	if f.Admin.ProvisionRolesForOptions(ctx, passwords, "verification", false) != nil {
		t.Fatal("review disable failed")
	}
	for _, query := range []string{`SELECT * FROM review_runs`, `UPDATE review_runs SET state='failed' WHERE false`, `UPDATE review_effects SET digest=repeat('a',64) WHERE false`, `UPDATE review_model_calls SET state='uncertain' WHERE false`} {
		if _, e := db.Pool.Exec(query); e == nil {
			t.Fatal("review privilege retained after disable", query)
		}
	}
}

func TestReviewClaimCannotReviveAfterFailedDurableStop(t *testing.T) {
	f, db, r := reviewJournalFixture(t)
	ctx := context.Background()
	c := claimReview(t, db, r)
	pod := startReviewFixture(t, db, c)
	request := []byte(`{"model":"fixture","messages":[]}`)
	call, e := db.BeginReviewModelCall(ctx, c, pod, 1, request)
	if e != nil {
		t.Fatal(e)
	}
	response := []byte(`{"choices":[]}`)
	observation := []byte(`{"verdict":""}`)
	if db.CompleteReviewModelCall(ctx, call, response, observation) != nil || db.AcknowledgeReviewModelCall(ctx, call, authority.Digest(response)) != nil {
		t.Fatal("fixture handoff")
	}
	// Force a real failed UPDATE; the committed model response remains in the DB.
	if _, e = f.Admin.Pool.Exec(`REVOKE UPDATE ON review_runs FROM aisdlc_verifier`); e != nil {
		t.Fatal(e)
	}
	ownerCopy := c
	if db.StopReview(ctx, c, "uncertain") == nil {
		t.Fatal("stop fault did not occur")
	}
	if _, e = f.Admin.Pool.Exec(`GRANT UPDATE ON review_runs TO aisdlc_verifier`); e != nil {
		t.Fatal(e)
	}
	if f.Count(t, `SELECT count(*) FROM review_runs WHERE state='claimed'`) != 1 {
		t.Fatal("fixture did not retain uncertain parent")
	}
	if db.CheckReviewClaim(ctx, c) == nil || db.CheckReviewClaim(ctx, ownerCopy) == nil {
		t.Fatal("owner revived after failed stop")
	}
	if _, e = db.ReadReviewModelEvidence(ctx, c); e == nil {
		t.Fatal("uncertain handoff became evidence")
	}
	if _, e = db.BeginReviewModelCall(ctx, c, pod, 2, request); e == nil {
		t.Fatal("uncertain parent invoked next call")
	}
	copied := store.ReviewClaim{ID: c.ID, Token: c.Token, Until: c.Until}
	if db.CheckReviewClaim(ctx, copied) == nil {
		t.Fatal("serialized claim adopted")
	}
	if _, e = db.ClaimReview(ctx, r.ID, r.ConfigurationDigest); e == nil {
		t.Fatal("restart adopted original claim")
	}
	if e = db.StopReview(ctx, c, "uncertain"); e != nil {
		t.Fatal("revoked owner cannot finish durable stop", e)
	}
}
