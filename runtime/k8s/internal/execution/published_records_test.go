package execution

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
)

func TestPublishedRecordSurvivesLeaseExpiryWithoutRenewingAuthority(t *testing.T) {
	f, db, r := publicationJournalFixture(t)
	ctx := context.Background()
	if _, e := db.ReadPublishedRecord(ctx, r.ID); e == nil {
		t.Fatal("queued request became publication fact")
	}
	// This is an isolated owned database: force a short expiry then wait for real
	// PostgreSQL wall time, without extending/reviving any production authority.
	if _, e := f.Admin.Pool.Exec(`UPDATE publication_requests SET expires_at=clock_timestamp()+interval '2 seconds' WHERE id=$1`, r.ID); e != nil {
		t.Fatal(e)
	}
	c := publicationClaim(t, db, r)
	completePublicationEffects(t, db, c)
	raw, _ := json.Marshal(publicationProof(t, db, c))
	if e := db.FinishPublication(ctx, c, raw); e != nil {
		t.Fatal(e)
	}
	before, e := db.ReadPublishedRecord(ctx, r.ID)
	if e != nil || before.ProofDigest != authority.Digest(raw) || !before.PublishedAt.Before(before.AuthorityExpiresAt) {
		t.Fatal("missing atomic evidence", e)
	}
	if _, e = f.db.ReadPublishedRecord(ctx, r.ID); e == nil {
		t.Fatal("worker read release fact")
	}
	for _, query := range []string{`UPDATE published_records SET proof='changed' WHERE false`, `DELETE FROM published_records WHERE false`, `INSERT INTO published_records(publication_id,published_at) SELECT 'invalid',clock_timestamp() WHERE false`} {
		if _, e = db.Pool.Exec(query); e == nil {
			t.Fatal("operational identity can mutate/backdate fact", query)
		}
	}
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		var expired bool
		if f.Admin.Pool.QueryRow(`SELECT expires_at<=clock_timestamp() FROM publication_requests WHERE id=$1`, r.ID).Scan(&expired) != nil {
			t.Fatal("expiry observation")
		}
		if expired {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if db.CheckPublication(ctx, c) == nil {
		t.Fatal("expired claim revived")
	}
	if e = db.ExpirePublications(ctx); e != nil {
		t.Fatal(e)
	}
	after, e := db.ReadPublishedRecord(ctx, r.ID)
	if e != nil || after.ProofDigest != before.ProofDigest || !after.PublishedAt.Equal(before.PublishedAt) {
		t.Fatal("terminal evidence expired", e)
	}
	if _, e = db.ClaimPublication(ctx, r.ID, c.PlanDigest); e == nil || db.FinishPublication(ctx, c, raw) == nil {
		t.Fatal("historical evidence renewed publication")
	}
}

func TestPublishedRecordRejectsLegacyOrTamperedEvidence(t *testing.T) {
	for _, query := range []string{
		`DELETE FROM published_records WHERE publication_id=$1`,
		`UPDATE published_records SET payload='tampered' WHERE publication_id=$1`,
		`UPDATE published_records SET effects='[]' WHERE publication_id=$1`,
		`UPDATE published_records SET authority_expires_at=authority_expires_at+interval '1 day' WHERE publication_id=$1`,
		`UPDATE publication_requests SET plan_digest=repeat('0',64) WHERE id=$1`,
		`UPDATE publication_requests SET proof='{}' WHERE id=$1`,
	} {
		t.Run(query, func(t *testing.T) {
			f, db, r := publicationJournalFixture(t)
			c := publicationClaim(t, db, r)
			completePublicationEffects(t, db, c)
			raw, _ := json.Marshal(publicationProof(t, db, c))
			if e := db.FinishPublication(context.Background(), c, raw); e != nil {
				t.Fatal(e)
			}
			// Only the disposable fixture administrator can perform this corruption.
			if _, e := f.Admin.Pool.Exec(query, r.ID); e != nil {
				t.Fatal(e)
			}
			if _, e := db.ReadPublishedRecord(context.Background(), r.ID); e == nil {
				t.Fatal("missing/corrupt terminal fact accepted")
			}
		})
	}
}

func TestPublicationRecordFailureRollsBackTerminalState(t *testing.T) {
	f, db, r := publicationJournalFixture(t)
	c := publicationClaim(t, db, r)
	completePublicationEffects(t, db, c)
	raw, _ := json.Marshal(publicationProof(t, db, c))
	if _, e := f.Admin.Pool.Exec(`REVOKE INSERT(publication_id,payload_digest,payload,receipt,proof_digest,proof,effects_digest,effects,authority_expires_at) ON published_records FROM aisdlc_release_manager`); e != nil {
		t.Fatal(e)
	}
	if db.FinishPublication(context.Background(), c, raw) == nil {
		t.Fatal("terminal transition without durable fact")
	}
	if f.Count(t, `SELECT count(*) FROM published_records`) != 0 || f.Count(t, `SELECT count(*) FROM publication_requests WHERE state='claimed' AND proof IS NULL`) != 1 {
		t.Fatal("non-atomic publication completion")
	}
}

func TestPublishedRecordCannotExtendParentExpiry(t *testing.T) {
	_, db, r := publicationJournalFixture(t)
	c := publicationClaim(t, db, r)
	completePublicationEffects(t, db, c)
	raw, _ := json.Marshal(publicationProof(t, db, c))
	_, err := db.Pool.Exec(`INSERT INTO published_records(publication_id,payload_digest,payload,receipt,proof_digest,proof,effects_digest,effects,authority_expires_at) SELECT id,payload_digest,payload,receipt,$2,$3,$2,$3,expires_at+interval '1 day' FROM publication_requests WHERE id=$1`, r.ID, authority.Digest(raw), raw)
	if err == nil {
		t.Fatal("operational role extended historical authority")
	}
	if err = db.FinishPublication(context.Background(), c, raw); err != nil {
		t.Fatal("rejected insertion poisoned valid completion", err)
	}
	if _, err = db.ReadPublishedRecord(context.Background(), r.ID); err != nil {
		t.Fatal(err)
	}
}
