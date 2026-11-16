package execution

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/mergegate"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/merging"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/publication"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

// Explicit fixture-only grants: no installed profile enables this capability.
func mergeJournalFixture(t *testing.T) (*lifecycleFixture, *store.MergeJournal, store.MergeRequest, merging.Command) {
	t.Helper()
	f, db, pub := publicationJournalFixture(t)
	ctx := context.Background()
	c := publicationClaim(t, db, pub)
	for _, kind := range []string{"tree", "commit", "branch", "pull-request"} {
		effect, e := db.BeginPublicationEffect(ctx, c, kind, authority.Digest([]byte(kind)))
		if e != nil {
			t.Fatal(e)
		}
		receipt := publicationReceipt(kind)
		if kind != "tree" {
			receipt.SHA = strings.Repeat("c", 40)
		}
		if e = db.AcknowledgePublicationEffect(ctx, effect, receipt); e != nil {
			t.Fatal(e)
		}
	}
	proof := publicationProof(t, db, c)
	proof["commitSHA"] = strings.Repeat("c", 40)
	raw, _ := json.Marshal(proof)
	if e := db.FinishPublication(ctx, c, raw); e != nil {
		t.Fatal(e)
	}
	record, e := db.ReadPublishedRecord(ctx, pub.ID)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.Admin.Pool.Exec(`GRANT SELECT,INSERT,UPDATE ON merge_requests,merge_effects TO aisdlc_release_manager; GRANT SELECT ON merged_records TO aisdlc_release_manager; GRANT INSERT(merge_id,payload_digest,payload,receipt,gate,policy,proof_digest,proof,effect_digest,effect,authority_expires_at) ON merged_records TO aisdlc_release_manager`); e != nil {
		t.Fatal(e)
	}
	var original publication.Command
	if json.Unmarshal(pub.Payload, &original) != nil {
		t.Fatal("publication command")
	}
	f.Material.Policy.Autonomy = "auto-merge-low"
	now := time.Now()
	cmd := merging.Command{Version: merging.Version, PublicationID: pub.ID, PublicationRecordSHA256: record.Digest(), GateSHA256: strings.Repeat("e", 64), Repository: original.Repository, IssueNumber: original.IssueNumber, PullRequest: 17, HeadSHA: strings.Repeat("c", 40), BaseSHA: original.SourceSHA, TreeSHA: strings.Repeat("a", 40), Branch: "ai-sdlc/issue-3-" + strings.Repeat("b", 32), AuthorityRevision: f.Material.Policy.Revision(), IssuedAt: now.UnixMilli(), ExpiresAt: now.Add(25 * time.Second).UnixMilli()}
	gate, _ := json.Marshal(mergegate.Observation{Expected: mergegate.Expected{Repository: cmd.Repository, PullRequest: cmd.PullRequest, HeadSHA: cmd.HeadSHA, BaseSHA: cmd.BaseSHA, TreeSHA: cmd.TreeSHA, Branch: cmd.Branch}, PolicySHA256: strings.Repeat("e", 64), ChecksSHA256: strings.Repeat("f", 64), ReviewsSHA256: strings.Repeat("a", 64), ObservedAt: now, Ready: true, Reasons: []string{}})
	cmd.GateSHA256 = authority.Digest(gate)
	raw, _ = json.Marshal(cmd)
	signed, e := f.Material.Identities["release-manager"].Seal(f.Material.Policy, cmd.ID(), "merge-publication", raw)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = merging.Verify(f.Material.Policy, signed, raw, now); e != nil {
		t.Fatal(e)
	}
	receipt, _ := json.Marshal(signed)
	r := store.MergeRequest{ID: cmd.ID(), PublicationID: pub.ID, RecordDigest: record.Digest(), GateDigest: cmd.GateSHA256, AuthorityRevision: cmd.AuthorityRevision, PayloadDigest: authority.Digest(raw), Payload: raw, Receipt: receipt, Gate: gate, ExpiresAt: time.UnixMilli(cmd.ExpiresAt)}
	return f, store.NewMergeJournal(db, func() (authority.Policy, error) { return f.Material.Policy, nil }), r, cmd
}
func queuedMerge(t *testing.T, db *store.MergeJournal, r store.MergeRequest) store.MergeClaim {
	t.Helper()
	ctx := context.Background()
	if e := db.QueueMerge(ctx, r); e != nil {
		t.Fatal("queue merge", e)
	}
	c, e := db.ClaimMerge(ctx, r.ID)
	if e != nil {
		t.Fatal("claim merge", e)
	}
	return c
}
func mergeReceipt(c merging.Command) store.MergeReceipt {
	return store.MergeReceipt{Version: "ai-sdlc.merge-receipt/v1", CommitSHA: strings.Repeat("d", 40), HeadSHA: c.HeadSHA, BaseSHA: c.BaseSHA, TreeSHA: c.TreeSHA, PullRequest: c.PullRequest}
}
func completeMerge(t *testing.T, db *store.MergeJournal, c store.MergeClaim, cmd merging.Command) []byte {
	t.Helper()
	ctx := context.Background()
	effect, e := db.BeginMergeEffect(ctx, c, strings.Repeat("f", 64))
	if e != nil {
		t.Fatal(e)
	}
	if e = db.AcknowledgeMergeEffect(ctx, effect, mergeReceipt(cmd)); e != nil {
		t.Fatal(e)
	}
	evidence, e := db.ReadMergeEvidence(ctx, c)
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(evidence)
	p := store.MergeProof{Version: "ai-sdlc.merge-proof/v1", MergeID: c.ID, PublicationID: cmd.PublicationID, PublicationRecordSHA256: c.RecordDigest, GateSHA256: c.GateDigest, EffectSHA256: authority.Digest(raw), Receipt: mergeReceipt(cmd)}
	raw, _ = json.Marshal(p)
	return raw
}
func TestMergeJournalBindsPublicationAndExclusiveOwner(t *testing.T) {
	f, db, r, cmd := mergeJournalFixture(t)
	ctx := context.Background()
	for _, change := range []func(*merging.Command){func(c *merging.Command) { c.PublicationRecordSHA256 = strings.Repeat("0", 64) }, func(c *merging.Command) { c.HeadSHA = strings.Repeat("0", 40) }, func(c *merging.Command) { c.BaseSHA = strings.Repeat("0", 40) }, func(c *merging.Command) { c.TreeSHA = strings.Repeat("0", 40) }, func(c *merging.Command) { c.PullRequest++ }, func(c *merging.Command) { c.IssueNumber++ }, func(c *merging.Command) { c.Branch = "other" }} {
		changed := cmd
		change(&changed)
		var gate mergegate.Observation
		if json.Unmarshal(r.Gate, &gate) != nil {
			t.Fatal("gate fixture")
		}
		gate.Expected = mergegate.Expected{Repository: changed.Repository, PullRequest: changed.PullRequest, HeadSHA: changed.HeadSHA, BaseSHA: changed.BaseSHA, TreeSHA: changed.TreeSHA, Branch: changed.Branch}
		gateRaw, _ := json.Marshal(gate)
		changed.GateSHA256 = authority.Digest(gateRaw)
		bad := signedMergeRequest(t, f, r, changed, gateRaw)
		if db.QueueMerge(ctx, bad) == nil {
			t.Fatal("changed publication binding accepted")
		}
	}
	if store.NewMergeJournal(f.db, func() (authority.Policy, error) { return f.Material.Policy, nil }).QueueMerge(ctx, r) == nil {
		t.Fatal("worker queued merge")
	}
	if e := db.QueueMerge(ctx, r); e != nil {
		t.Fatal(e)
	}
	if db.QueueMerge(ctx, r) == nil {
		t.Fatal("duplicate publication merge")
	}
	var wg sync.WaitGroup
	claims := make(chan store.MergeClaim, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if c, e := db.ClaimMerge(ctx, r.ID); e == nil {
				claims <- c
			}
		}()
	}
	wg.Wait()
	close(claims)
	count := 0
	var c store.MergeClaim
	for candidate := range claims {
		count++
		c = candidate
	}
	if count != 1 || c.Until.After(r.ExpiresAt) || c.Until.After(time.Now().Add(91*time.Second)) {
		t.Fatal("nonexclusive/unbounded merge")
	}
	if store.NewMergeJournal(f.db, func() (authority.Policy, error) { return f.Material.Policy, nil }).CheckMerge(ctx, c) == nil {
		t.Fatal("worker reused merge claim")
	}
	wrong := c
	wrong.Token = store.NewID()
	if db.StopMerge(ctx, wrong, "uncertain") == nil || db.CheckMerge(ctx, c) != nil {
		t.Fatal("foreign claimant stopped owner")
	}
	if _, e := db.ClaimMerge(ctx, r.ID); e == nil {
		t.Fatal("claim adopted")
	}
}
func TestMergeUnknownEffectQuarantinesWithoutReplay(t *testing.T) {
	for _, notSent := range []bool{false, true} {
		t.Run(map[bool]string{false: "unknown", true: "not-sent"}[notSent], func(t *testing.T) {
			_, db, r, cmd := mergeJournalFixture(t)
			ctx := context.Background()
			c := queuedMerge(t, db, r)
			effect, e := db.BeginMergeEffect(ctx, c, strings.Repeat("f", 64))
			if e != nil {
				t.Fatal(e)
			}
			if notSent {
				if e = db.SealMergeNotSent(ctx, effect); e != nil {
					t.Fatal(e)
				}
			}
			if e = db.StopMerge(ctx, c, "uncertain"); e != nil {
				t.Fatal(e)
			}
			blocked, e := db.MergeBlocked(ctx)
			if e != nil || blocked == notSent {
				t.Fatal("unknown effect quarantine", e)
			}
			if _, e = db.BeginMergeEffect(ctx, c, strings.Repeat("f", 64)); e == nil {
				t.Fatal("effect replay")
			}
			if db.AcknowledgeMergeEffect(ctx, effect, mergeReceipt(cmd)) == nil {
				t.Fatal("stopped claim ACK")
			}
			if _, e = db.ClaimMerge(ctx, r.ID); e == nil {
				t.Fatal("claim adoption")
			}
		})
	}
}
func TestMergeCompletionIsAtomicImmutableAndCannotDeploy(t *testing.T) {
	f, db, r, cmd := mergeJournalFixture(t)
	ctx := context.Background()
	c := queuedMerge(t, db, r)
	raw := completeMerge(t, db, c, cmd)
	var p store.MergeProof
	_ = json.Unmarshal(raw, &p)
	p.DeploymentAuthorized = true
	bad, _ := json.Marshal(p)
	if db.FinishMerge(ctx, c, bad) == nil {
		t.Fatal("merge authorized deployment")
	}
	if e := db.FinishMerge(ctx, c, raw); e != nil {
		t.Fatal(e)
	}
	fact, e := db.ReadMergedRecord(ctx, r.ID)
	if e != nil || fact.ProofDigest != authority.Digest(raw) {
		t.Fatal("missing merge fact", e)
	}
	if _, e = f.db.ReadMergedRecord(ctx, r.ID); e == nil {
		t.Fatal("worker read merge fact")
	}
	for _, query := range []string{`UPDATE merged_records SET proof='changed' WHERE false`, `DELETE FROM merged_records WHERE false`, `INSERT INTO merged_records(merge_id,merged_at) SELECT 'invalid',clock_timestamp() WHERE false`} {
		if _, e = db.Pool.Exec(query); e == nil {
			t.Fatal("mutable/backdated fact")
		}
	}
	if _, e = db.ClaimMerge(ctx, r.ID); e == nil || db.FinishMerge(ctx, c, raw) == nil {
		t.Fatal("completed merge replay")
	}
}
func TestMergeRecordFailureRollsBackTerminalState(t *testing.T) {
	f, db, r, cmd := mergeJournalFixture(t)
	ctx := context.Background()
	c := queuedMerge(t, db, r)
	raw := completeMerge(t, db, c, cmd)
	if _, e := f.Admin.Pool.Exec(`REVOKE INSERT(merge_id,payload_digest,payload,receipt,gate,policy,proof_digest,proof,effect_digest,effect,authority_expires_at) ON merged_records FROM aisdlc_release_manager`); e != nil {
		t.Fatal(e)
	}
	if db.FinishMerge(ctx, c, raw) == nil || f.Count(t, `SELECT count(*) FROM merge_requests WHERE state='claimed' AND proof IS NULL`) != 1 || f.Count(t, `SELECT count(*) FROM merged_records`) != 0 {
		t.Fatal("non-atomic merge completion")
	}
}
func TestMergeLostCommitCannotAuthorizeEffectOrReplay(t *testing.T) {
	for _, stage := range []string{"claim", "intent", "ack", "finish"} {
		t.Run(stage, func(t *testing.T) {
			f, db, r, cmd := mergeJournalFixture(t)
			ctx := context.Background()
			if e := db.QueueMerge(ctx, r); e != nil {
				t.Fatal(e)
			}
			faultDB, dropped := executionCommitFault(t, 1, "release-manager")
			fault := store.NewMergeJournal(faultDB, func() (authority.Policy, error) { return f.Material.Policy, nil })
			if stage == "claim" {
				if c, e := fault.ClaimMerge(ctx, r.ID); e == nil || c.Token != "" {
					t.Fatal("lost claim returned authority")
				}
			} else {
				c, e := db.ClaimMerge(ctx, r.ID)
				if e != nil {
					t.Fatal(e)
				}
				switch stage {
				case "intent":
					if effect, e := fault.BeginMergeEffect(ctx, c, strings.Repeat("f", 64)); e == nil || effect.Token != "" {
						t.Fatal("lost intent authorized send")
					}
				case "ack":
					effect, e := db.BeginMergeEffect(ctx, c, strings.Repeat("f", 64))
					if e != nil {
						t.Fatal(e)
					}
					if fault.AcknowledgeMergeEffect(ctx, effect, mergeReceipt(cmd)) == nil {
						t.Fatal("lost ACK accepted")
					}
					if db.StopMerge(ctx, c, "uncertain") != nil {
						t.Fatal("uncertain stop")
					}
				case "finish":
					raw := completeMerge(t, db, c, cmd)
					if fault.FinishMerge(ctx, c, raw) == nil {
						t.Fatal("lost final COMMIT confirmed")
					}
				}
				if db.CheckMerge(ctx, c) == nil {
					t.Fatal("uncertain authority usable")
				}
			}
			select {
			case <-dropped:
			case <-time.After(time.Second):
				t.Fatal("COMMIT not actually dropped")
			}
			state := "failed"
			if stage == "finish" {
				state = "merged"
			}
			if f.Count(t, "SELECT count(*) FROM merge_requests WHERE state='"+state+"'") != 1 {
				t.Fatal("durable outcome")
			}
			_, e := db.ReadMergedRecord(ctx, r.ID)
			if (e == nil) != (stage == "finish") {
				t.Fatal("fact disagrees with commit")
			}
			if _, e = db.ClaimMerge(ctx, r.ID); e == nil {
				t.Fatal("replay after lost COMMIT")
			}
		})
	}
}

func TestMergeJournalRevalidatesSignedAuthorityAtEveryBoundary(t *testing.T) {
	for _, stage := range []string{"queue", "claim", "check", "intent", "ack", "finish"} {
		t.Run(stage, func(t *testing.T) {
			f, db, r, cmd := mergeJournalFixture(t)
			ctx := context.Background()
			if stage == "queue" {
				bad := r
				bad.Receipt = []byte(`{}`)
				if db.QueueMerge(ctx, bad) == nil {
					t.Fatal("unsigned merge accepted")
				}
				bad = r
				bad.Gate = []byte(`{}`)
				if db.QueueMerge(ctx, bad) == nil {
					t.Fatal("unbound gate accepted")
				}
				f.Material.Policy.Freeze = true
				if db.QueueMerge(ctx, r) == nil {
					t.Fatal("frozen queue")
				}
				return
			}
			if e := db.QueueMerge(ctx, r); e != nil {
				t.Fatal(e)
			}
			if stage == "claim" {
				f.Material.Policy.Freeze = true
				if _, e := db.ClaimMerge(ctx, r.ID); e == nil {
					t.Fatal("frozen claim")
				}
				return
			}
			c, e := db.ClaimMerge(ctx, r.ID)
			if e != nil {
				t.Fatal(e)
			}
			var effect store.MergeEffect
			var proof []byte
			if stage == "ack" {
				effect, e = db.BeginMergeEffect(ctx, c, strings.Repeat("f", 64))
				if e != nil {
					t.Fatal(e)
				}
			}
			if stage == "finish" {
				proof = completeMerge(t, db, c, cmd)
			}
			f.Material.Policy.Freeze = true
			switch stage {
			case "check":
				e = db.CheckMerge(ctx, c)
			case "intent":
				_, e = db.BeginMergeEffect(ctx, c, strings.Repeat("f", 64))
			case "ack":
				e = db.AcknowledgeMergeEffect(ctx, effect, mergeReceipt(cmd))
			case "finish":
				e = db.FinishMerge(ctx, c, proof)
			}
			if e == nil {
				t.Fatal("revoked authority remained usable")
			}
			if f.Count(t, `SELECT count(*) FROM merged_records`) != 0 {
				t.Fatal("revoked merge became fact")
			}
		})
	}
}
func TestMergeRequestLockWaitCannotOutliveAuthority(t *testing.T) {
	for _, stage := range []string{"claim", "check"} {
		t.Run(stage, func(t *testing.T) {
			f, db, r, _ := mergeJournalFixture(t)
			ctx := context.Background()
			if e := db.QueueMerge(ctx, r); e != nil {
				t.Fatal(e)
			}
			var c store.MergeClaim
			var e error
			column := "expires_at"
			if stage == "check" {
				c, e = db.ClaimMerge(ctx, r.ID)
				if e != nil {
					t.Fatal(e)
				}
				column = "claim_until"
			}
			if _, e = f.Admin.Pool.Exec(`UPDATE merge_requests SET `+column+`=clock_timestamp()+interval '1200 milliseconds' WHERE id=$1`, r.ID); e != nil {
				t.Fatal(e)
			}
			lock, e := f.Admin.Pool.BeginTx(ctx, nil)
			if e != nil {
				t.Fatal(e)
			}
			defer lock.Rollback()
			if _, e = lock.Exec(`SELECT 1 FROM merge_requests WHERE id=$1 FOR UPDATE`, r.ID); e != nil {
				t.Fatal(e)
			}
			result := make(chan error, 1)
			go func() {
				if stage == "check" {
					result <- db.CheckMerge(ctx, c)
				} else {
					_, e := db.ClaimMerge(ctx, r.ID)
					result <- e
				}
			}()
			deadline := time.Now().Add(time.Second)
			blocked := false
			for time.Now().Before(deadline) {
				if f.Admin.Pool.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE usename='aisdlc_release_manager' AND wait_event_type='Lock' AND query LIKE '%merge_requests%')`).Scan(&blocked) != nil {
					t.Fatal("lock observation")
				}
				if blocked {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if !blocked {
				t.Fatal("operation did not wait on real PostgreSQL lock")
			}
			for {
				var expired bool
				if f.Admin.Pool.QueryRow(`SELECT `+column+`<=clock_timestamp() FROM merge_requests WHERE id=$1`, r.ID).Scan(&expired) != nil {
					t.Fatal("expiry observation")
				}
				if expired {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if e = lock.Commit(); e != nil {
				t.Fatal(e)
			}
			if <-result == nil {
				t.Fatal("expired request survived lock wait")
			}
		})
	}
}
func TestMergeJournalRemainsInactiveUnderInstalledPublicationProfile(t *testing.T) {
	f, db, _, _ := mergeJournalFixture(t)
	passwords := map[string]string{}
	for role := range f.Material.Identities {
		passwords[role] = strings.ReplaceAll(role, "-", "_") + strings.Repeat("q", 40)
	}
	if f.Admin.ProvisionRolesForCapabilities(context.Background(), passwords, "verification", true, true) != nil {
		t.Fatal("installed publication profile")
	}
	for _, table := range []string{"merge_requests", "merge_effects", "merged_records"} {
		if _, e := db.Pool.Exec(`SELECT * FROM ` + table); e == nil {
			t.Fatal("publication profile enabled merge", table)
		}
	}
}
func TestMergedRecordRejectsCorruption(t *testing.T) {
	for _, query := range []string{
		`DELETE FROM merged_records WHERE merge_id=$1`,
		`UPDATE merged_records SET receipt='{}' WHERE merge_id=$1`,
		`UPDATE merged_records SET gate='{}' WHERE merge_id=$1`,
		`UPDATE merged_records SET policy='{}' WHERE merge_id=$1`,
		`UPDATE merged_records SET effect='{}' WHERE merge_id=$1`,
		`UPDATE merged_records SET authority_expires_at=authority_expires_at+interval '1 day' WHERE merge_id=$1`,
	} {
		t.Run(query, func(t *testing.T) {
			f, db, r, cmd := mergeJournalFixture(t)
			ctx := context.Background()
			c := queuedMerge(t, db, r)
			raw := completeMerge(t, db, c, cmd)
			if e := db.FinishMerge(ctx, c, raw); e != nil {
				t.Fatal(e)
			}
			if _, e := f.Admin.Pool.Exec(query, r.ID); e != nil {
				t.Fatal(e)
			}
			if _, e := db.ReadMergedRecord(ctx, r.ID); e == nil {
				t.Fatal("corrupt fact accepted")
			}
			if _, e := db.ReadMergedBuildSource(ctx, r.ID); e == nil {
				t.Fatal("corrupt fact became build input")
			}
		})
	}
}

func signedMergeRequest(t *testing.T, f *lifecycleFixture, r store.MergeRequest, c merging.Command, gate []byte) store.MergeRequest {
	t.Helper()
	raw, _ := json.Marshal(c)
	signed, e := f.Material.Identities["release-manager"].Seal(f.Material.Policy, c.ID(), "merge-publication", raw)
	if e != nil {
		t.Fatal(e)
	}
	receipt, _ := json.Marshal(signed)
	r.ID = c.ID()
	r.Payload = raw
	r.PayloadDigest = authority.Digest(raw)
	r.Receipt = receipt
	r.RecordDigest = c.PublicationRecordSHA256
	r.GateDigest = c.GateSHA256
	r.Gate = gate
	r.ExpiresAt = time.UnixMilli(c.ExpiresAt)
	return r
}
func TestMergeRejectsGateOlderThanItsFreshAuthority(t *testing.T) {
	f, db, r, c := mergeJournalFixture(t)
	ctx := context.Background()
	var g mergegate.Observation
	if json.Unmarshal(r.Gate, &g) != nil {
		t.Fatal("gate fixture")
	}
	// The gate remains close to issued time but must expire the entire capability.
	c.ExpiresAt = time.Now().Add(110 * time.Second).UnixMilli()
	bad := signedMergeRequest(t, f, r, c, r.Gate)
	if db.QueueMerge(ctx, bad) == nil {
		t.Fatal("gate TTL extended by signed command")
	}
	c.ExpiresAt = time.Now().Add(20 * time.Second).UnixMilli()
	g.ObservedAt = time.Now().Add(-31 * time.Second)
	raw, _ := json.Marshal(g)
	c.GateSHA256 = authority.Digest(raw)
	bad = signedMergeRequest(t, f, r, c, raw)
	if db.QueueMerge(ctx, bad) == nil {
		t.Fatal("stale gate accepted")
	}
}
func TestMergedRecordSurvivesExpiryWithoutRenewal(t *testing.T) {
	f, db, r, c := mergeJournalFixture(t)
	ctx := context.Background()
	c.ExpiresAt = time.Now().Add(2 * time.Second).UnixMilli()
	r = signedMergeRequest(t, f, r, c, r.Gate)
	claim := queuedMerge(t, db, r)
	raw := completeMerge(t, db, claim, c)
	if e := db.FinishMerge(ctx, claim, raw); e != nil {
		t.Fatal(e)
	}
	before, e := db.ReadMergedRecord(ctx, r.ID)
	if e != nil {
		t.Fatal(e)
	}
	deadline := time.Now().Add(4 * time.Second)
	expired := false
	for time.Now().Before(deadline) {
		if f.Admin.Pool.QueryRow(`SELECT expires_at<=clock_timestamp() FROM merge_requests WHERE id=$1`, r.ID).Scan(&expired) != nil {
			t.Fatal("expiry observation")
		}
		if expired {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !expired {
		t.Fatal("database expiry not observed")
	}
	if db.CheckMerge(ctx, claim) == nil {
		t.Fatal("historical claim renewed")
	}
	after, e := db.ReadMergedRecord(ctx, r.ID)
	if e != nil || after.ProofDigest != before.ProofDigest || !after.MergedAt.Equal(before.MergedAt) {
		t.Fatal("historical evidence expired", e)
	}
	source, e := db.ReadMergedBuildSource(ctx, r.ID)
	if e != nil || source.SourceSHA != mergeReceipt(c).CommitSHA || source.MergeRecordSHA256 != before.Digest() {
		t.Fatal("expired merge lost historical build identity", e)
	}
	if _, e = db.ClaimMerge(ctx, r.ID); e == nil {
		t.Fatal("terminal fact renewed authority")
	}
}
