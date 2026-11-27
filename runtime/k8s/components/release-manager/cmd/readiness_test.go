package main

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/publication"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/testsupport"
)

// readinessFixture returns the admin connection and a release-manager
// connection to the disposable test database with the merge capability
// installed.
func readinessFixture(t *testing.T) (admin, releaseManager *store.DB) {
	t.Helper()
	ctx := context.Background()
	f := testsupport.New(t)
	passwords := map[string]string{}
	for role := range f.Material.Identities {
		passwords[role] = strings.ReplaceAll(role, "-", "_") + strings.Repeat("q", 40)
	}
	if e := f.Admin.ProvisionRolesForMerge(ctx, passwords, "verification", true, true, true); e != nil {
		t.Fatal(e)
	}
	u, e := url.Parse(os.Getenv("AI_SDLC_TEST_DATABASE_URL"))
	if e != nil {
		t.Fatal("fixture connection invalid")
	}
	role, _ := store.ComponentRole("release-manager")
	u.User = url.UserPassword(role, passwords["release-manager"])
	db, e := store.Open(ctx, u.String())
	if e != nil {
		t.Fatal("release-manager fixture connection unavailable")
	}
	t.Cleanup(func() { _ = db.Close() })
	return f.Admin, db
}

// An uncertain merge effect quarantines merges but never readiness: failing
// readiness would remove the Service endpoints, so every publish delivery
// would time out instead (battery 39). An uncertain publication effect, and
// every configuration check, still make the release manager unready.
func TestReadinessDoesNotDependOnTheMergeQuarantine(t *testing.T) {
	admin, db := readinessFixture(t)
	ctx := context.Background()
	if e := releaseReadiness(ctx, db); e != nil {
		t.Fatal("idle release manager unready", e)
	}
	// Uncertain effects, as lost forge responses leave them. Only the foreign
	// keys are bypassed (replica role on this admin connection); every check
	// constraint still applies.
	conn, e := admin.Pool.Conn(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	if _, e = conn.ExecContext(ctx, `SET session_replication_role=replica`); e != nil {
		t.Fatal(e)
	}
	// The seeded rows reference nothing; remove them for later packages.
	t.Cleanup(func() {
		_, _ = admin.Pool.Exec(`DELETE FROM merge_effects WHERE request_id='merge-'||repeat('e',64); DELETE FROM merge_requests WHERE id='merge-'||repeat('e',64); DELETE FROM publication_effects WHERE request_id='publication-'||repeat('e',64); DELETE FROM publication_requests WHERE id='publication-'||repeat('e',64)`)
	})
	merge := "merge-" + strings.Repeat("e", 64)
	if _, e = conn.ExecContext(ctx, `INSERT INTO merge_requests(id,publication_id,record_digest,gate_digest,authority_revision,payload_digest,payload,receipt,gate,policy,expires_at,state,claim_token,claim_until,reason)
 VALUES($1,'publication-'||repeat('e',64),repeat('1',64),repeat('2',64),repeat('3',64),repeat('4',64),'{}','{}','{}','{}',clock_timestamp()+interval '1 hour','failed',gen_random_uuid(),clock_timestamp(),'uncertain')`, merge); e != nil {
		t.Fatal(e)
	}
	if _, e = conn.ExecContext(ctx, `INSERT INTO merge_effects(request_id,token,request_digest) VALUES($1,gen_random_uuid(),repeat('f',64))`, merge); e != nil {
		t.Fatal(e)
	}
	journal := store.NewMergeJournal(db, func() (authority.Policy, error) { return authority.Policy{}, nil })
	if blocked, e := journal.MergeBlocked(ctx); e != nil || !blocked {
		t.Fatal("fixture merge quarantine", blocked, e)
	}
	if e := releaseReadiness(ctx, db); e != nil {
		t.Fatal("merge quarantine made the release manager unready", e)
	}
	// The configuration checks still gate readiness, in order.
	stop := errors.New("writer unavailable")
	later := false
	if e := releaseReadiness(ctx, db, func(context.Context) error { return stop }, func(context.Context) error { later = true; return nil }); e != stop || later {
		t.Fatal("failed check did not stop readiness", e, later)
	}
	pub := "publication-" + strings.Repeat("e", 64)
	if _, e = conn.ExecContext(ctx, `INSERT INTO publication_requests(id,review_id,verification_id,candidate_id,repository,payload_digest,payload,receipt,expires_at,state,claim_token,claim_until,plan_digest,commit_time,reason)
 VALUES($1,'review','verification','candidate','example/project',repeat('4',64),'{}','{}',clock_timestamp()+interval '1 hour','failed',gen_random_uuid(),clock_timestamp(),repeat('5',64),clock_timestamp(),'uncertain')`, pub); e != nil {
		t.Fatal(e)
	}
	if _, e = conn.ExecContext(ctx, `INSERT INTO publication_effects(request_id,kind,token,request_digest) VALUES($1,'tree',gen_random_uuid(),repeat('f',64))`, pub); e != nil {
		t.Fatal(e)
	}
	if e := releaseReadiness(ctx, db); !errors.Is(e, publication.ErrPublication) {
		t.Fatal("publication quarantine left the release manager ready", e)
	}
}
