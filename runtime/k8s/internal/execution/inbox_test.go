package execution

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/delivery"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/intake"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/testsupport"
)

type fixture struct {
	testsupport.Fixture
	db               *store.DB
	command          Command
	buildFixtureTree string
}

func newFixture(t *testing.T, configure ...func(*fixture)) *fixture {
	t.Helper()
	f := &fixture{Fixture: testsupport.New(t)}
	f.Material.Policy.Autonomy = "auto-pr"
	u, err := url.Parse(os.Getenv("AI_SDLC_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword("aisdlc_worker", "worker"+strings.Repeat("q", 40))
	f.db, err = store.Open(context.Background(), u.String())
	if err != nil {
		t.Fatal("worker fixture connection unavailable")
	}
	t.Cleanup(func() { f.db.Close() })
	// Fixture-only grants. The operational provisioner still grants none.
	_, err = f.Admin.Pool.Exec(`GRANT SELECT ON execution_requests TO aisdlc_worker;
        GRANT INSERT (id,work_item_uuid,attempt,repository,authority_revision,payload_digest,payload,receipt,expires_at) ON execution_requests TO aisdlc_worker;
        GRANT UPDATE (state,claim_token,claim_until,stop_reason,updated_at) ON execution_requests TO aisdlc_worker`)
	if err != nil {
		t.Fatal(err)
	}
	plan, work := store.NewID(), store.NewID()
	if err = store.UpsertPlan(context.Background(), f.Admin.Pool, store.PlanRecord{ID: plan, Repository: f.Material.Policy.Repository, CorrelationID: store.NewID()}); err != nil {
		t.Fatal(err)
	}
	if err = store.UpsertWorkItem(context.Background(), f.Admin.Pool, store.WorkItemRecord{PlanID: plan, ID: "fixture", UUID: work, IssueNumbers: []int{3}, AcceptanceCriteria: []string{"fixture"}, Status: "ready"}); err != nil {
		t.Fatal(err)
	}
	f.command = Command{Version: Version, WorkItemID: work, Attempt: 1, Repository: f.Material.Policy.Repository, IssueNumber: 3, Generation: 1,
		AdmissionID: "admission-issue-" + strings.Repeat("a", 64), SourceSHA: strings.Repeat("b", 40), ProfileSHA256: strings.Repeat("c", 64),
		AuthorityRevision: f.Material.Policy.Revision(), ExpiresAt: time.Now().Add(5 * time.Minute).UnixMilli()}
	f.seedReadiness(t)
	for _, setup := range configure {
		setup(f)
	}
	return f
}
func (f *fixture) policy() (authority.Policy, error) { return f.Material.Policy, nil }
func (f *fixture) envelope(t *testing.T, c Command) delivery.Envelope {
	return f.Envelope(t, "orchestrator", c.ID(), "execute-work", "/internal/execute-work", c)
}
func (f *fixture) accept(e delivery.Envelope) (bool, error) {
	_, duplicate, err := (delivery.Inbox{DB: f.db, Target: "worker"}).Accept(context.Background(), f.Material.Policy, e, "execute-work", http.MethodPost, "/internal/execute-work", time.Now(), Effect(f.policy))
	return duplicate, err
}

func TestSignedExecutionDuplicateDeliveryAndSingleClaim(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 2; i++ {
		duplicate, err := f.accept(f.envelope(t, f.command))
		if err != nil || duplicate != (i == 1) {
			t.Fatalf("delivery %d duplicate=%v error=%v", i, duplicate, err)
		}
	}
	var wg sync.WaitGroup
	winners := make(chan store.ExecutionClaim, 20)
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			claim, err := Claim(context.Background(), f.db, f.readyPolicy, f.command.ID())
			if err == nil {
				winners <- claim
			}
		}()
	}
	wg.Wait()
	close(winners)
	count := 0
	for claim := range winners {
		count++
		if claim.Token == "" || !claim.Until.After(time.Now()) || claim.Until.After(time.Now().Add(time.Minute)) {
			t.Fatal("invalid bounded claim")
		}
	}
	if count != 1 {
		t.Fatalf("claims=%d", count)
	}
	if f.Count(t, `SELECT count(*) FROM execution_requests WHERE state='claimed'`) != 1 {
		t.Fatal("loser changed winning claim")
	}
	if f.Count(t, `SELECT count(*) FROM authorized_inbox WHERE target='worker'`) != 1 {
		t.Fatal("duplicate durable inbox row")
	}
}

func TestSignedExecutionRejectsForgedAndInvalidCommands(t *testing.T) {
	f := newFixture(t)
	for _, mutate := range []func(*delivery.Envelope){
		func(e *delivery.Envelope) { e.Payload = append(e.Payload, ' ') },
		func(e *delivery.Envelope) { e.Receipt.Producer = "intake" },
		func(e *delivery.Envelope) { e.Receipt.Repository = "other/product" },
		func(e *delivery.Envelope) { e.Receipt.Signature = "invalid" },
		func(e *delivery.Envelope) { e.Transport.Path = "/other" },
	} {
		e := f.envelope(t, f.command)
		mutate(&e)
		if _, err := f.accept(e); err == nil {
			t.Fatal("forged delivery accepted")
		}
	}
	for _, mutate := range []func(*Command){
		func(c *Command) { c.Generation = 0 }, func(c *Command) { c.Attempt = 17 },
		func(c *Command) { c.SourceSHA = "main" }, func(c *Command) { c.ProfileSHA256 = "mutable" },
		func(c *Command) { c.AuthorityRevision = strings.Repeat("d", 64) },
		func(c *Command) { c.ExpiresAt = time.Now().Add(-time.Second).UnixMilli() },
		func(c *Command) { c.AdmissionID = "claimed-in-issue-text" },
	} {
		c := f.command
		mutate(&c)
		if _, err := f.accept(f.envelope(t, c)); err == nil {
			t.Fatal("invalid command accepted")
		}
	}
	if f.Count(t, `SELECT count(*) FROM execution_requests`) != 0 || f.Count(t, `SELECT count(*) FROM authorized_inbox WHERE target='worker'`) != 0 {
		t.Fatal("rejection left durable effect")
	}
}

func TestSignedExecutionConflictAndImmutablePrivileges(t *testing.T) {
	f := newFixture(t)
	if _, err := f.accept(f.envelope(t, f.command)); err != nil {
		t.Fatal(err)
	}
	c := f.command
	c.SourceSHA = strings.Repeat("d", 40)
	if _, err := f.accept(f.envelope(t, c)); err == nil {
		t.Fatal("same attempt accepted changed identity")
	}
	c.Attempt = 2
	if _, err := f.accept(f.envelope(t, c)); err == nil {
		t.Fatal("second active attempt accepted")
	}
	if _, err := f.db.Pool.Exec(`UPDATE execution_requests SET payload='tampered'`); err == nil {
		t.Fatal("worker can mutate immutable payload")
	}
	if _, err := f.Components["orchestrator"].DB.Pool.Exec(`UPDATE execution_requests SET state='claimed'`); err == nil {
		t.Fatal("other role can claim")
	}
	if _, err := f.Admin.ClaimExecution(context.Background(), f.command.ID(), "wrong"); err == nil {
		t.Fatal("admin credential accepted as worker")
	}
	if f.Count(t, `SELECT count(*) FROM execution_requests`) != 1 {
		t.Fatal("conflict changed queue")
	}
}

func TestExecutionClaimRechecksAuthority(t *testing.T) {
	f := newFixture(t)
	if _, err := f.accept(f.envelope(t, f.command)); err != nil {
		t.Fatal(err)
	}
	original := f.Material.Policy
	f.Material.Policy.Freeze = true
	if _, err := Claim(context.Background(), f.db, f.readyPolicy, f.command.ID()); err == nil {
		t.Fatal("freeze ignored")
	}
	f.Material.Policy = original
	f.Material.Policy.Autonomy = "shadow"
	if _, err := Claim(context.Background(), f.db, f.readyPolicy, f.command.ID()); err == nil {
		t.Fatal("lowered autonomy ignored")
	}
	f.Material.Policy = original
	delete(f.Material.Policy.Keys, "orchestrator-1")
	if _, err := Claim(context.Background(), f.db, f.readyPolicy, f.command.ID()); err == nil {
		t.Fatal("revoked signing key ignored")
	}
	if f.Count(t, `SELECT count(*) FROM execution_requests WHERE state='queued'`) != 1 {
		t.Fatal("denied claim advanced state")
	}
}

func TestExecutionExpiryAfterRowLockNeverGrantsClaim(t *testing.T) {
	f := newFixture(t)
	f.command.ExpiresAt = time.Now().Add(250 * time.Millisecond).UnixMilli()
	if _, err := f.accept(f.envelope(t, f.command)); err != nil {
		t.Fatal(err)
	}
	lock, err := f.Admin.Pool.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback()
	if _, err = lock.Exec(`SELECT id FROM execution_requests WHERE id=$1 FOR UPDATE`, f.command.ID()); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(f.command)
	result := make(chan error, 1)
	go func() {
		_, err := f.db.ClaimExecution(context.Background(), f.command.ID(), authority.Digest(raw))
		result <- err
	}()
	time.Sleep(350 * time.Millisecond)
	if err = lock.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = <-result; err == nil {
		t.Fatal("expired request obtained claim after lock wait")
	}
	if f.Count(t, `SELECT count(*) FROM execution_requests WHERE state='queued'`) != 1 {
		t.Fatal("expired request advanced")
	}
}

func TestExecutionCommandRejectsAmbiguousJSON(t *testing.T) {
	f := newFixture(t)
	e := f.envelope(t, f.command)
	raw := append([]byte(`{"attempt":1,`), e.Payload[1:]...)
	receipt, err := f.Material.Identities["orchestrator"].Seal(f.Material.Policy, f.command.ID(), "execute-work", raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Verify(f.Material.Policy, receipt, raw, time.Now()); err == nil {
		t.Fatal("duplicate command key accepted")
	}
}

func TestExecutionRevocationDuringRowLockStopsOwnClaim(t *testing.T) {
	f := newFixture(t)
	if _, err := f.accept(f.envelope(t, f.command)); err != nil {
		t.Fatal(err)
	}
	lock, err := f.Admin.Pool.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback()
	if _, err = lock.Exec(`SELECT id FROM execution_requests WHERE id=$1 FOR UPDATE`, f.command.ID()); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	policy := f.Material.Policy
	load := func() (authority.Policy, intake.Config, error) {
		mu.Lock()
		defer mu.Unlock()
		return policy, f.Config, nil
	}
	result := make(chan error, 1)
	go func() {
		claim, err := Claim(context.Background(), f.db, load, f.command.ID())
		if err != nil && claim.Token != "" {
			err = nil
		}
		result <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		var waiting int
		if err = f.Admin.Pool.QueryRow(`SELECT count(*) FROM pg_stat_activity WHERE usename='aisdlc_worker' AND wait_event_type='Lock'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("claim did not wait on actual row lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	policy.Freeze = true
	mu.Unlock()
	if err = lock.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = <-result; err == nil {
		t.Fatal("revoked request returned a claim")
	}
	if f.Count(t, `SELECT count(*) FROM execution_requests WHERE state='stopped' AND stop_reason='authority-revoked'`) != 1 {
		t.Fatal("revoked claim not stopped")
	}
}

func TestExecutionStopCannotAffectAnotherClaim(t *testing.T) {
	f := newFixture(t)
	if _, err := f.accept(f.envelope(t, f.command)); err != nil {
		t.Fatal(err)
	}
	claim, err := Claim(context.Background(), f.db, f.readyPolicy, f.command.ID())
	if err != nil {
		t.Fatal(err)
	}
	wrong := claim
	wrong.Token = store.NewID()
	if err = f.db.StopExecutionClaim(context.Background(), wrong, "aborted"); err == nil {
		t.Fatal("wrong token stopped claim")
	}
	if f.Count(t, `SELECT count(*) FROM execution_requests WHERE state='claimed'`) != 1 {
		t.Fatal("wrong token changed claim")
	}
	if err = f.db.StopExecutionClaim(context.Background(), claim, "aborted"); err != nil {
		t.Fatal(err)
	}
	if _, err = Claim(context.Background(), f.db, f.readyPolicy, f.command.ID()); err == nil {
		t.Fatal("stopped request reopened")
	}
}

func TestExecutionRoleProvisionerKeepsQueueHidden(t *testing.T) {
	f := newFixture(t)
	passwords := map[string]string{}
	for role := range f.Material.Identities {
		passwords[role] = strings.ReplaceAll(role, "-", "_") + strings.Repeat("q", 40)
	}
	if err := f.Admin.ProvisionRoles(context.Background(), passwords); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ReadExecution(context.Background(), f.command.ID()); err == nil {
		t.Fatal("operational worker can read hidden queue")
	}
	var allowed bool
	if err := f.Admin.Pool.QueryRow(`SELECT has_table_privilege('aisdlc_worker','execution_requests','SELECT,INSERT,UPDATE') OR has_any_column_privilege('aisdlc_worker','execution_requests','INSERT,UPDATE')`).Scan(&allowed); err != nil {
		t.Fatal(err)
	}
	if allowed {
		t.Fatal("fixture grants survived operational privilege reconciliation")
	}
	for _, table := range []string{"plans", "work_items", "intake_heads", "intake_revisions", "intake_cursors", "intake_observations", "admission_heads", "planning_heads", "admissions"} {
		if err := f.Admin.Pool.QueryRow(`SELECT has_any_column_privilege('aisdlc_worker',$1,'SELECT,INSERT,UPDATE,REFERENCES')`, table).Scan(&allowed); err != nil {
			t.Fatal(err)
		}
		if allowed {
			t.Fatalf("operational worker retained proof privileges on %s", table)
		}
	}
}

func TestExecutionProofReaderCannotRefreshIssueObservation(t *testing.T) {
	f := newFixture(t)
	if _, err := f.db.Pool.Exec(`UPDATE intake_observations SET observed_at=clock_timestamp()`); err == nil {
		t.Fatal("worker refreshed source proof")
	}
	if _, err := f.db.Pool.Exec(`DELETE FROM intake_observations`); err == nil {
		t.Fatal("worker removed source proof")
	}
	// Even an accidental UPDATE grant cannot bypass the table's role policy.
	if _, err := f.Admin.Pool.Exec(`GRANT UPDATE(observed_at) ON intake_observations TO aisdlc_worker`); err != nil {
		t.Fatal(err)
	}
	result, err := f.db.Pool.Exec(`UPDATE intake_observations SET observed_at=clock_timestamp()`)
	if err != nil {
		t.Fatal(err)
	}
	n, err := result.RowsAffected()
	if err != nil || n != 0 {
		t.Fatal("row policy allowed worker to manufacture source freshness")
	}
}
