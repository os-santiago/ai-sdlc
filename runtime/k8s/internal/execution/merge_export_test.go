package execution

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/intake"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

// Exported only into the external test binary. All signatures, synthetic forge
// receipts and installed grants belong to its disposable PostgreSQL fixture.
type PublishedMergeTestFixture struct {
	Admin, DB                                                  *store.DB
	Policy                                                     authority.Policy
	Identity                                                   authority.Identity
	Intake                                                     intake.Config
	Profile, ReviewProfile, ReviewConfiguration, PublicationID string
}

func NewPublishedMergeTestFixture(t *testing.T, lifetime ...time.Duration) PublishedMergeTestFixture {
	t.Helper()
	f, db, r := publicationJournalFixture(t, func(f *fixture) {
		f.Material.Policy.Autonomy = "auto-merge-low"
		if len(lifetime) == 1 {
			f.command.ExpiresAt = time.Now().Add(lifetime[0]).UnixMilli()
		}
	})
	ctx := context.Background()
	c := publicationClaim(t, db, r)
	for _, kind := range []string{"tree", "commit", "branch", "pull-request"} {
		effect, e := db.BeginPublicationEffect(ctx, c, kind, authority.Digest([]byte(kind)))
		if e != nil {
			t.Fatal(e)
		}
		receipt := publicationReceipt(kind)
		if kind != "tree" {
			receipt.SHA = strings.Repeat("c", 40)
		}
		if db.AcknowledgePublicationEffect(ctx, effect, receipt) != nil {
			t.Fatal("fixture publication receipt")
		}
	}
	proof := publicationProof(t, db, c)
	proof["commitSHA"] = strings.Repeat("c", 40)
	raw, _ := json.Marshal(proof)
	if e := db.FinishPublication(ctx, c, raw); e != nil {
		t.Fatal(e)
	}
	passwords := map[string]string{}
	for role := range f.Material.Identities {
		passwords[role] = strings.ReplaceAll(role, "-", "_") + strings.Repeat("q", 40)
	}
	if e := f.Admin.ProvisionRolesForMerge(ctx, passwords, "verification", true, true, true); e != nil {
		t.Fatal(e)
	}

	return PublishedMergeTestFixture{f.Admin, db, f.Material.Policy, f.Material.Identities["release-manager"], f.Config, f.command.ProfileSHA256, strings.Repeat("b", 64), strings.Repeat("c", 64), r.ID}
}

// RecordClosedIssueGeneration writes what the intake poller records when it
// observes the fixture issue closed: the next generation, ineligible, with a
// new content key and a fresh observation. Fixture-only; production intake
// writes these rows under its own lease and signed delivery.
func (f PublishedMergeTestFixture) RecordClosedIssueGeneration(t *testing.T) {
	t.Helper()
	var heads int
	if e := f.Admin.Pool.QueryRow(`SELECT count(*) FROM intake_heads`).Scan(&heads); e != nil || heads != 1 {
		t.Fatal("fixture intake head", heads, e)
	}
	var generation int64
	var payload []byte
	if e := f.Admin.Pool.QueryRow(`SELECT h.generation,r.payload FROM intake_heads h JOIN intake_revisions r ON r.repository=h.repository AND r.issue_number=h.issue_number AND r.generation=h.generation`).Scan(&generation, &payload); e != nil {
		t.Fatal(e)
	}
	var s intake.Snapshot
	if e := json.Unmarshal(payload, &s); e != nil {
		t.Fatal(e)
	}
	s.Issue.State, s.Eligible, s.BaseSHA, s.Generation = "closed", false, "", generation+1
	s.Revision = s.ContentRevision()
	if e := s.Validate(f.Intake); e != nil {
		t.Fatal("closed snapshot", e)
	}
	raw, _ := json.Marshal(s)
	key := authority.Digest([]byte(s.Revision + "\x00false\x00" + s.ConfigDigest + "\x00" + s.AuthorityRevision))
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO intake_revisions(repository,issue_number,revision,generation,config_digest,event_id,payload) VALUES($1,$2,$3,$4,$5,$6,$7)`, []any{s.Repository, s.Issue.Number, s.Revision, s.Generation, s.ConfigDigest, s.ID(), raw}},
		{`UPDATE intake_heads SET content_key=$1,generation=$2 WHERE repository=$3 AND issue_number=$4`, []any{key, s.Generation, s.Repository, s.Issue.Number}},
		{`UPDATE intake_observations SET generation=$1,content_key=$2,observed_at=clock_timestamp() WHERE repository=$3 AND issue_number=$4`, []any{s.Generation, key, s.Repository, s.Issue.Number}},
	} {
		if _, e := f.Admin.Pool.Exec(statement.sql, statement.args...); e != nil {
			t.Fatal(e)
		}
	}
}
