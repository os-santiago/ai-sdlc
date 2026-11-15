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
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/delivery"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/publication"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/verification"
)

func publicationJournalFixture(t *testing.T) (*lifecycleFixture, *store.DB, store.PublicationRequest) {
	t.Helper()
	f, verifier, review := reviewProofFixture(t, true)
	ctx := context.Background()
	if _, e := f.Admin.Pool.Exec(`GRANT SELECT,INSERT ON publication_handoffs TO aisdlc_verifier; GRANT SELECT ON publication_handoffs,review_runs,verification_requests TO aisdlc_release_manager; GRANT SELECT,INSERT,UPDATE ON publication_requests,publication_effects TO aisdlc_release_manager`); e != nil {
		t.Fatal(e)
	}
	passed, e := verifier.ReadPassedReview(ctx, review.VerificationID)
	if e != nil {
		t.Fatal(e)
	}
	original, e := verifier.ReadVerification(ctx, review.VerificationID)
	if e != nil {
		t.Fatal(e)
	}
	var v verification.Command
	if json.Unmarshal(original.Payload, &v) != nil {
		t.Fatal("verification command")
	}
	c := publication.Command{Version: publication.Version, VerificationID: v.ID(), CandidateID: v.CandidateID, WorkItemID: v.WorkItemID, Repository: v.Repository, IssueNumber: v.IssueNumber, Generation: v.Generation, IssueRevision: v.IssueRevision, SourceSHA: v.SourceSHA, DeltaSHA256: v.DeltaSHA256, CandidateTreeSHA256: v.CandidateTreeSHA256, BehaviorProofSHA256: authority.Digest(passed.Behavior), IndependentReviewSHA256: authority.Digest(passed.Proof), ReviewerProfileSHA256: review.ProfileDigest, AuthorityRevision: f.Material.Policy.Revision(), ExpiresAt: review.ExpiresAt.UnixMilli()}
	raw, _ := json.Marshal(c)
	key := f.Material.Identities["verifier"]
	if e := verifier.WithTx(ctx, func(tx *sql.Tx) error {
		if e := store.RecordPublicationHandoff(ctx, tx, review.ID, c.ID(), authority.Digest(raw), c.IndependentReviewSHA256); e != nil {
			return e
		}
		return delivery.Append(ctx, tx, f.Material.Policy, key, c.ID(), "publish-candidate", raw)
	}); e != nil {
		t.Fatal("signed publication handoff", e)
	}
	signed, e := key.Seal(f.Material.Policy, c.ID(), "publish-candidate", raw)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = publication.Verify(f.Material.Policy, signed, raw, time.Now()); e != nil {
		t.Fatal("publication contract", e)
	}
	receipt, _ := json.Marshal(signed)
	r := store.PublicationRequest{ID: c.ID(), ReviewID: review.ID, VerificationID: review.VerificationID, CandidateID: review.CandidateID, Repository: c.Repository, PayloadDigest: authority.Digest(raw), Payload: raw, Receipt: receipt, ExpiresAt: time.UnixMilli(c.ExpiresAt)}
	u, e := url.Parse(os.Getenv("AI_SDLC_TEST_DATABASE_URL"))
	if e != nil {
		t.Fatal(e)
	}
	u.User = url.UserPassword("aisdlc_release_manager", "release_manager"+strings.Repeat("q", 40))
	db, e := store.Open(ctx, u.String())
	if e != nil {
		t.Fatal("release fixture")
	}
	t.Cleanup(func() { db.Close() })
	if e = db.WithTx(ctx, func(tx *sql.Tx) error {
		return store.InsertPublication(ctx, tx, r, c.IndependentReviewSHA256, c.BehaviorProofSHA256)
	}); e != nil {
		t.Fatal("publication queue", e)
	}
	return f, db, r
}
func publicationClaim(t *testing.T, db *store.DB, r store.PublicationRequest) store.PublicationClaim {
	t.Helper()
	c, e := db.ClaimPublication(context.Background(), r.ID, strings.Repeat("f", 64))
	if e != nil {
		t.Fatal(e)
	}
	return c
}
func publicationReceipt(kind string) store.PublicationReceipt {
	r := store.PublicationReceipt{Version: "ai-sdlc.publication-receipt/v1", Kind: kind, SHA: strings.Repeat("a", 40)}
	if kind == "branch" || kind == "pull-request" {
		r.Branch = "ai-sdlc/issue-3-" + strings.Repeat("b", 32)
	}
	if kind == "pull-request" {
		r.PullRequest = 17
	}
	return r
}
func completePublicationEffects(t *testing.T, db *store.DB, c store.PublicationClaim) {
	t.Helper()
	for _, kind := range []string{"tree", "commit", "branch", "pull-request"} {
		e, err := db.BeginPublicationEffect(context.Background(), c, kind, authority.Digest([]byte(kind)))
		if err != nil {
			t.Fatal(err)
		}
		if db.AcknowledgePublicationEffect(context.Background(), e, publicationReceipt(kind)) != nil {
			t.Fatal("effect receipt")
		}
	}
}
func publicationProof(t *testing.T, db *store.DB, c store.PublicationClaim) map[string]any {
	t.Helper()
	evidence, e := db.ReadPublicationEffects(context.Background(), c)
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(evidence)
	return map[string]any{"version": "ai-sdlc.publication-proof/v1", "publicationID": c.ID, "planSHA256": c.PlanDigest, "effectsSHA256": authority.Digest(raw), "commitSHA": strings.Repeat("a", 40), "branch": "ai-sdlc/issue-3-" + strings.Repeat("b", 32), "pullRequest": 17, "mergeAuthorized": false, "deploymentAuthorized": false}
}
func TestPublicationJournalOneOwnerAndBoundedNonrenewableClaim(t *testing.T) {
	f, db, r := publicationJournalFixture(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	results := make(chan store.PublicationClaim, 12)
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, e := db.ClaimPublication(ctx, r.ID, strings.Repeat("f", 64))
			if e == nil {
				results <- c
			}
		}()
	}
	wg.Wait()
	close(results)
	count := 0
	var c store.PublicationClaim
	for result := range results {
		count++
		c = result
	}
	if count != 1 || c.Until.After(time.Now().Add(121*time.Second)) || c.Until.After(r.ExpiresAt) || c.CommitTime.Nanosecond() != 0 {
		t.Fatal("invalid exclusive publication ownership")
	}
	if f.db.CheckPublication(ctx, c) == nil {
		t.Fatal("generator used publication claim")
	}
	if _, e := db.ClaimPublication(ctx, r.ID, c.PlanDigest); e == nil {
		t.Fatal("publication claim adopted")
	}
	wrong := c
	wrong.Token = store.NewID()
	if db.StopPublication(ctx, wrong, "uncertain") == nil || db.CheckPublication(ctx, c) != nil {
		t.Fatal("foreign claimant stopped owner")
	}
}
func TestPublicationProofRequiresAcknowledgedEffectsAndCannotAuthorizeMerge(t *testing.T) {
	f, db, r := publicationJournalFixture(t)
	ctx := context.Background()
	c := publicationClaim(t, db, r)
	completePublicationEffects(t, db, c)
	proof := publicationProof(t, db, c)
	encode := func() []byte { raw, _ := json.Marshal(proof); return raw }
	for key, value := range map[string]any{"effectsSHA256": strings.Repeat("0", 64), "planSHA256": strings.Repeat("0", 64), "commitSHA": strings.Repeat("0", 40), "branch": "main", "pullRequest": 18, "mergeAuthorized": true, "deploymentAuthorized": true} {
		old := proof[key]
		proof[key] = value
		if db.FinishPublication(ctx, c, encode()) == nil {
			t.Fatal("invalid publication proof", key)
		}
		proof[key] = old
	}
	if f.db.FinishPublication(ctx, c, encode()) == nil {
		t.Fatal("generator completed publication")
	}
	if db.FinishPublication(ctx, c, encode()) != nil {
		t.Fatal("valid publication proof")
	}
	if db.FinishPublication(ctx, c, encode()) == nil {
		t.Fatal("publication replay")
	}
	if id, e := db.NextPublication(ctx); e != nil || id != "" {
		t.Fatal("published work queued")
	}
}
func TestPublicationUnknownOutcomeQuarantinesWithoutAdoption(t *testing.T) {
	_, db, r := publicationJournalFixture(t)
	ctx := context.Background()
	c := publicationClaim(t, db, r)
	effect, e := db.BeginPublicationEffect(ctx, c, "tree", strings.Repeat("e", 64))
	if e != nil {
		t.Fatal(e)
	}
	if db.StopPublication(ctx, c, "uncertain") != nil {
		t.Fatal("stop")
	}
	if blocked, e := db.PublicationBlocked(ctx); e != nil || !blocked {
		t.Fatal("unknown write did not quarantine")
	}
	if db.AcknowledgePublicationEffect(ctx, effect, publicationReceipt("tree")) == nil {
		t.Fatal("stopped attempt adopted receipt")
	}
	if _, e = db.ClaimPublication(ctx, r.ID, c.PlanDigest); e == nil {
		t.Fatal("unknown write resumed")
	}
}
func TestPublicationUnsentEffectRetiresWithoutQuarantiningFutureWork(t *testing.T) {
	_, db, r := publicationJournalFixture(t)
	ctx := context.Background()
	c := publicationClaim(t, db, r)
	effect, e := db.BeginPublicationEffect(ctx, c, "tree", strings.Repeat("e", 64))
	if e != nil {
		t.Fatal(e)
	}
	if db.SealPublicationNotSent(ctx, effect) != nil || db.StopPublication(ctx, c, "revoked") != nil {
		t.Fatal("known no-send stop")
	}
	if blocked, e := db.PublicationBlocked(ctx); e != nil || blocked {
		t.Fatal("known no-send became unknown")
	}
	if db.AcknowledgePublicationEffect(ctx, effect, publicationReceipt("tree")) == nil {
		t.Fatal("known no-send adopted response")
	}
}
func TestPublicationLostCommitCannotAuthorizeEffectOrReplay(t *testing.T) {
	for _, stage := range []string{"claim", "intent", "ack", "finish"} {
		t.Run(stage, func(t *testing.T) {
			f, db, r := publicationJournalFixture(t)
			ctx := context.Background()
			fault, dropped := executionCommitFault(t, 1, "release-manager")
			if stage == "claim" {
				if c, e := fault.ClaimPublication(ctx, r.ID, strings.Repeat("f", 64)); e == nil || c.Token != "" {
					t.Fatal("lost commit returned claim")
				}
			} else {
				c := publicationClaim(t, db, r)
				if stage == "finish" {
					completePublicationEffects(t, db, c)
					proof := publicationProof(t, db, c)
					raw, _ := json.Marshal(proof)
					if fault.FinishPublication(ctx, c, raw) == nil {
						t.Fatal("lost final commit appeared confirmed")
					}
					if _, e := db.ClaimPublication(ctx, r.ID, c.PlanDigest); e == nil {
						t.Fatal("published commit replay")
					}
				} else if stage == "intent" {
					if e, err := fault.BeginPublicationEffect(ctx, c, "tree", strings.Repeat("e", 64)); err == nil || e.Token != "" {
						t.Fatal("lost intent authorized POST")
					}
				} else {
					e, err := db.BeginPublicationEffect(ctx, c, "tree", strings.Repeat("e", 64))
					if err != nil {
						t.Fatal(err)
					}
					if fault.AcknowledgePublicationEffect(ctx, e, publicationReceipt("tree")) == nil {
						t.Fatal("lost ACK appeared confirmed")
					}
					if db.StopPublication(ctx, c, "uncertain") != nil {
						t.Fatal("caller did not stop ambiguous result")
					}
				}
				if db.CheckPublication(ctx, c) == nil {
					t.Fatal("ambiguous publication remained usable")
				}
			}
			select {
			case <-dropped:
			case <-time.After(time.Second):
				t.Fatal("actual COMMIT response was not dropped")
			}
			state := "failed"
			if stage == "finish" {
				state = "published"
			}
			if f.Count(t, "SELECT count(*) FROM publication_requests WHERE state='"+state+"'") != 1 {
				t.Fatal("lost commit changed durable outcome")
			}
		})
	}
}
func TestPublicationJournalRemainsInactiveUnderInstalledReviewProfile(t *testing.T) {
	f, db, _ := publicationJournalFixture(t)
	passwords := map[string]string{}
	for role := range f.Material.Identities {
		passwords[role] = strings.ReplaceAll(role, "-", "_") + strings.Repeat("q", 40)
	}
	if f.Admin.ProvisionRolesForOptions(context.Background(), passwords, "verification", true) != nil {
		t.Fatal("installed review profile")
	}
	for _, table := range []string{"publication_requests", "publication_effects", "publication_handoffs"} {
		if _, e := db.Pool.Exec(`SELECT * FROM ` + table); e == nil {
			t.Fatal("publication activated by review installation", table)
		}
	}
}

func TestPublicationCanReadProofsWithoutChangingThem(t *testing.T) {
	_, db, r := publicationJournalFixture(t)
	if _, e := db.ReadPublicationReview(context.Background(), r.VerificationID); e != nil {
		t.Fatal(e)
	}
	for _, q := range []string{`UPDATE review_runs SET proof='changed' WHERE false`, `UPDATE verification_requests SET proof='changed' WHERE false`} {
		if _, e := db.Pool.Exec(q); e == nil {
			t.Fatal("publisher can rewrite independent evidence")
		}
	}
}
func TestPublicationExpiryAfterEffectRowWaitRollsBackReceipt(t *testing.T) {
	f, db, r := publicationJournalFixture(t)
	ctx := context.Background()
	c := publicationClaim(t, db, r)
	effect, e := db.BeginPublicationEffect(ctx, c, "tree", strings.Repeat("e", 64))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.Admin.Pool.Exec(`UPDATE publication_requests SET claim_until=clock_timestamp()+interval '350 milliseconds' WHERE id=$1`, c.ID); e != nil {
		t.Fatal(e)
	}
	blocker, e := f.Admin.Pool.Begin()
	if e != nil {
		t.Fatal(e)
	}
	defer blocker.Rollback()
	if _, e = blocker.Exec(`SELECT 1 FROM publication_effects WHERE request_id=$1 FOR UPDATE`, c.ID); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- db.AcknowledgePublicationEffect(ctx, effect, publicationReceipt("tree")) }()
	time.Sleep(500 * time.Millisecond)
	if blocker.Commit() != nil {
		t.Fatal("unlock")
	}
	if <-done == nil {
		t.Fatal("expired claim acknowledged after wait")
	}
	if f.Count(t, `SELECT count(*) FROM publication_effects WHERE completed_at IS NOT NULL`) != 0 {
		t.Fatal("expired receipt committed")
	}
	if db.ExpirePublications(ctx) != nil {
		t.Fatal("expiry")
	}
	if blocked, e := db.PublicationBlocked(ctx); e != nil || !blocked {
		t.Fatal("expired unknown effect not quarantined")
	}
}
