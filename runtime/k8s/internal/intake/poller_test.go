package intake

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/forge"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

func config() Config {
	return Config{Version: 1, Repository: "example/project", BaseBranch: "main", TriggerLabel: "ready", AuthorizedActorIDs: []int64{42}, PollSeconds: 30, MaxPages: 3}
}

type source struct {
	issue    forge.Issue
	actor    forge.Actor
	event    int64
	failure  error
	calls    int
	changed  bool
	fullPage bool
	pageHook func(time.Time, int) []forge.Issue
}

func fakeSource() *source {
	return &source{issue: forge.Issue{ID: 7, Number: 3, Title: "feat: bounded fixture", Body: "Fixture body", State: "open", User: forge.Actor{ID: 42, Login: "fixture"}, Labels: []forge.Label{{Name: "ready"}}, UpdatedAt: time.Now().UTC()}, actor: forge.Actor{ID: 42, Login: "fixture"}, event: 9}
}
func (s *source) Issues(ctx context.Context, since time.Time, page int) ([]forge.Issue, error) {
	s.calls++
	if s.pageHook != nil {
		return s.pageHook(since, page), nil
	}
	if s.failure != nil {
		return nil, s.failure
	}
	if s.fullPage {
		items := make([]forge.Issue, 100)
		for i := range items {
			items[i] = s.issue
		}
		return items, nil
	}
	return []forge.Issue{s.issue}, nil
}
func (s *source) Issue(ctx context.Context, n int) (forge.Issue, error) {
	s.calls++
	issue := s.issue
	if s.changed && s.calls%2 == 0 {
		issue.Body += "changed"
	}
	return issue, nil
}
func (s *source) LabelActor(ctx context.Context, n int, label string) (forge.Actor, int64, error) {
	return s.actor, s.event, nil
}
func (s *source) BaseSHA(context.Context, string) (string, error) {
	return strings.Repeat("a", 40), nil
}

func TestSnapshotRevisionAndScope(t *testing.T) {
	s := fakeSource()
	p := Poller{Config: config(), Source: s}
	snapshot, err := p.snapshot(context.Background(), 3)
	if err != nil {
		t.Fatal(err)
	}
	before := snapshot.Revision
	snapshot.Generation = 1
	snapshot.AuthorityRevision = strings.Repeat("a", 64)
	snapshot.Issue.UpdatedAt = snapshot.Issue.UpdatedAt.Add(time.Minute)
	snapshot.Issue.User.Login = "renamed"
	if snapshot.ContentRevision() != before {
		t.Fatal("non-request metadata created a new revision")
	}
	snapshot.Issue.Body += "new requirement"
	if snapshot.ContentRevision() == before {
		t.Fatal("body change did not change revision")
	}
	if snapshot.Validate(config()) == nil {
		t.Fatal("content changed under old revision")
	}
	snapshot.Revision = snapshot.ContentRevision()
	snapshot.Repository = "other/project"
	if snapshot.Validate(config()) == nil {
		t.Fatal("wrong repository accepted")
	}
	s.changed = true
	if _, err := p.snapshot(context.Background(), 3); err == nil {
		t.Fatal("concurrent issue change accepted")
	}
}

type fixture struct {
	poller Poller
	admin  *store.DB
	source *source
	policy *authority.Policy
}

func setup(t *testing.T) fixture {
	t.Helper()
	connection := os.Getenv("AI_SDLC_TEST_DATABASE_URL")
	if connection == "" {
		t.Skip("PostgreSQL required")
	}
	ctx := context.Background()
	admin, err := store.Open(ctx, connection)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	if err := admin.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Pool.ExecContext(ctx, `TRUNCATE intake_observations,intake_cursors,intake_revisions,intake_heads,authorized_outbox,authorized_inbox,authorized_nonces`); err != nil {
		t.Fatal(err)
	}
	material, err := authority.Bootstrap("example/project", "production-test")
	if err != nil {
		t.Fatal(err)
	}
	material.Policy.Freeze = false
	passwords := map[string]string{}
	for role := range material.Identities {
		passwords[role] = strings.ReplaceAll(role, "-", "_") + strings.Repeat("z", 40)
	}
	if err := admin.ProvisionRoles(ctx, passwords); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(connection)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword("aisdlc_intake", passwords["intake"])
	db, err := store.Open(ctx, u.String())
	if err != nil {
		t.Fatal("component database connection failed")
	}
	t.Cleanup(func() { db.Close() })
	s := fakeSource()
	p := Poller{DB: db, Config: config(), Source: s, Authority: func() (authority.Policy, authority.Identity, error) {
		return material.Policy, material.Identities["intake"], nil
	}}
	return fixture{poller: p, admin: admin, source: s, policy: &material.Policy}
}
func (f fixture) due(t *testing.T) {
	t.Helper()
	if _, err := f.admin.Pool.ExecContext(context.Background(), `UPDATE intake_cursors SET next_poll=now()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
}
func (f fixture) counts(t *testing.T) (int, int) {
	t.Helper()
	var records, events int
	if err := f.admin.Pool.QueryRow(`SELECT (SELECT count(*) FROM intake_revisions),(SELECT count(*) FROM authorized_outbox WHERE producer='intake')`).Scan(&records, &events); err != nil {
		t.Fatal(err)
	}
	return records, events
}

func TestDurableDedupeAndWithdrawal(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if n, err := f.poller.PollOnce(ctx, time.Now()); err != nil || n != 1 {
		t.Fatalf("first scan: %d %v", n, err)
	}
	var raw, receipt []byte
	if err := f.admin.Pool.QueryRow(`SELECT payload,receipt FROM authorized_outbox WHERE producer='intake'`).Scan(&raw, &receipt); err != nil {
		t.Fatal(err)
	}
	var signed authority.Receipt
	if err := json.Unmarshal(receipt, &signed); err != nil {
		t.Fatal(err)
	}
	if err := f.policy.VerifyReceipt(signed, "admission-controller", "admit-issue", raw); err != nil {
		t.Fatal(err)
	}
	f.due(t)
	restarted := f.poller
	if n, err := restarted.PollOnce(ctx, time.Now()); err != nil || n != 0 {
		t.Fatalf("restart duplicate: %d %v", n, err)
	}
	f.source.issue.UpdatedAt = f.source.issue.UpdatedAt.Add(time.Second)
	f.due(t)
	if n, err := restarted.PollOnce(ctx, time.Now()); err != nil || n != 0 {
		t.Fatal("comment timestamp created work")
	}
	f.source.issue.State = "closed"
	f.due(t)
	if n, err := restarted.PollOnce(ctx, time.Now()); err != nil || n != 1 {
		t.Fatalf("withdrawal: %d %v", n, err)
	}
	if records, events := f.counts(t); records != 2 || events != 2 {
		t.Fatalf("ledger/outbox disagree: %d %d", records, events)
	}
	f.source.issue.State = "open"
	f.due(t)
	if n, err := restarted.PollOnce(ctx, time.Now()); err != nil || n != 1 {
		t.Fatalf("reopen: %d %v", n, err)
	}
	body := f.source.issue.Body
	f.source.issue.Body += " updated"
	f.due(t)
	if n, err := restarted.PollOnce(ctx, time.Now()); err != nil || n != 1 {
		t.Fatalf("edit: %d %v", n, err)
	}
	f.source.issue.Body = body
	f.due(t)
	if n, err := restarted.PollOnce(ctx, time.Now()); err != nil || n != 1 {
		t.Fatalf("revert: %d %v", n, err)
	}
	if records, events := f.counts(t); records != 5 || events != 5 {
		t.Fatal("meaningful transition lost")
	}
	var max, distinct int
	if err := f.admin.Pool.QueryRow(`SELECT max(generation),count(DISTINCT generation) FROM intake_revisions`).Scan(&max, &distinct); err != nil || max != 5 || distinct != 5 {
		t.Fatal("generation was not monotonic")
	}
}

func TestDeniedActorFreezeAndWrongDatabaseHaveNoAdmissionEffects(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.source.actor.ID = 99
	if n, err := f.poller.PollOnce(ctx, time.Now()); err != nil || n != 0 {
		t.Fatalf("denied actor: %d %v", n, err)
	}
	if records, events := f.counts(t); records != 0 || events != 0 {
		t.Fatal("unauthorized actor emitted work")
	}
	f.policy.Freeze = true
	calls := f.source.calls
	if _, err := f.poller.PollOnce(ctx, time.Now()); err == nil {
		t.Fatal("freeze accepted")
	}
	if calls != f.source.calls {
		t.Fatal("freeze reached GitHub")
	}
	f.policy.Freeze = false
	f.poller.DB = f.admin
	if _, err := f.poller.PollOnce(ctx, time.Now()); err == nil {
		t.Fatal("bootstrap identity accepted")
	}
}

func TestRateLimitAndPartialScanRetainCursor(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.source.failure = &forge.APIError{Status: 429, RetryAfter: 10 * time.Minute}
	if _, err := f.poller.PollOnce(ctx, time.Now()); err == nil {
		t.Fatal("rate failure hidden")
	}
	var watermark sql.NullTime
	var next time.Time
	var failures int
	if err := f.admin.Pool.QueryRow(`SELECT watermark,next_poll,failures FROM intake_cursors`).Scan(&watermark, &next, &failures); err != nil {
		t.Fatal(err)
	}
	if watermark.Valid || failures != 1 || time.Until(next) < 9*time.Minute {
		t.Fatal("rate limit advanced cursor or lost backoff")
	}
	f.due(t)
	f.source.failure = nil
	f.source.fullPage = true
	f.poller.Config.MaxPages = 1
	if _, err := f.poller.PollOnce(ctx, time.Now()); err == nil {
		t.Fatal("truncated pagination accepted")
	}
	if err := f.admin.Pool.QueryRow(`SELECT watermark FROM intake_cursors WHERE config_digest=$1`, f.poller.Config.Digest()).Scan(&watermark); err != nil {
		t.Fatal(err)
	}
	if watermark.Valid {
		t.Fatal("partial scan advanced watermark")
	}
	if records, events := f.counts(t); records != 1 || events != 1 {
		t.Fatal("reordered page duplicated admission")
	}
}

func TestOutboxFailureRollsBackRevision(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	snapshot, err := f.poller.snapshot(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	// Disposable-fixture fault injection: application role remains constrained.
	if _, err := f.admin.Pool.ExecContext(ctx, `REVOKE INSERT (producer,event_id,target,operation,receipt,payload) ON authorized_outbox FROM aisdlc_intake`); err != nil {
		t.Fatal(err)
	}
	reserved, err := f.poller.reserve(ctx, time.Now())
	if err != nil || reserved == nil {
		t.Fatal("fixture lease unavailable")
	}
	if _, err := f.poller.record(ctx, snapshot, reserved.token, time.Now()); err == nil {
		t.Fatal("outbox failure hidden")
	}
	if records, events := f.counts(t); records != 0 || events != 0 {
		t.Fatal("revision committed without signed event")
	}
}

func TestFullReconciliationRecoversOldIssueDisplacedBetweenPages(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	pass := 1
	f.source.issue.UpdatedAt = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	prefix := f.source.issue
	prefix.Number = 4
	prefix.ID = 8
	prefix.Labels = nil
	f.source.pageHook = func(since time.Time, page int) []forge.Issue {
		if !since.IsZero() {
			t.Fatal("watermark excluded old issue from reconciliation")
		}
		if page == 1 {
			items := make([]forge.Issue, 100)
			for i := range items {
				items[i] = prefix
			}
			return items
		}
		if pass == 1 {
			return nil
		} // Concurrent deletion displaced the target past this offset.
		return []forge.Issue{f.source.issue}
	}
	if n, err := f.poller.PollOnce(ctx, time.Now()); err != nil || n != 0 {
		t.Fatalf("first scan: %d %v", n, err)
	}
	pass = 2
	f.due(t)
	if n, err := f.poller.PollOnce(ctx, time.Now()); err != nil || n != 1 {
		t.Fatalf("old issue not recovered: %d %v", n, err)
	}
}

func TestMissingAuthorityRejectsBeforeAnySourceCall(t *testing.T) {
	s := fakeSource()
	p := Poller{Source: s, DB: &store.DB{}, Config: config(), Authority: func() (authority.Policy, authority.Identity, error) {
		return authority.Policy{}, authority.Identity{}, errors.New("missing")
	}}
	if _, err := p.PollOnce(context.Background(), time.Now()); err == nil || s.calls != 0 {
		t.Fatal("missing policy did not fail closed")
	}
}

func TestLedgerPrivilegesRemainExclusiveToIntake(t *testing.T) {
	f := setup(t)
	for _, table := range []string{"intake_heads", "intake_cursors", "intake_revisions"} {
		for _, role := range []string{"aisdlc_intake", "aisdlc_worker", "aisdlc_relay", "aisdlc_admission_controller"} {
			var readable, writable bool
			if err := f.admin.Pool.QueryRow(`SELECT has_table_privilege($1,$2,'SELECT'),has_table_privilege($1,$2,'UPDATE')`, role, table).Scan(&readable, &writable); err != nil {
				t.Fatal(err)
			}
			if readable != (role == "aisdlc_intake") || writable != (role == "aisdlc_intake") {
				t.Fatal("ledger component isolation violated")
			}
		}
	}
}

func TestReviewedBindingChangesProduceFreshGeneration(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if n, err := f.poller.PollOnce(ctx, time.Now()); err != nil || n != 1 {
		t.Fatal("initial observation failed")
	}
	f.poller.Config.BaseBranch = "develop"
	if n, err := f.poller.PollOnce(ctx, time.Now()); err != nil || n != 1 {
		t.Fatalf("configuration change suppressed: %d %v", n, err)
	}
	f.policy.Keys["intake-next"] = f.policy.Keys["intake-1"]
	f.due(t)
	if n, err := f.poller.PollOnce(ctx, time.Now()); err != nil || n != 1 {
		t.Fatalf("authority change suppressed: %d %v", n, err)
	}
	f.due(t)
	if n, err := f.poller.PollOnce(ctx, time.Now()); err != nil || n != 0 {
		t.Fatal("identical binding duplicated")
	}
	var raw []byte
	if err := f.admin.Pool.QueryRow(`SELECT payload FROM intake_revisions ORDER BY generation DESC LIMIT 1`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var snapshot Snapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Generation != 3 || snapshot.ConfigDigest != f.poller.Config.Digest() || snapshot.AuthorityRevision != f.policy.Revision() {
		t.Fatal("new observation retained stale binding")
	}
}

func TestUnchangedIneligibleTrackedIssueIsNotReread(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if n, err := f.poller.PollOnce(ctx, time.Now()); err != nil || n != 1 {
		t.Fatalf("first scan: %d %v", n, err)
	}
	// Withdrawing the label advances updated_at and is recorded once.
	f.source.issue.Labels = nil
	f.source.issue.UpdatedAt = f.source.issue.UpdatedAt.Add(time.Second)
	f.due(t)
	if n, err := f.poller.PollOnce(ctx, time.Now()); err != nil || n != 1 {
		t.Fatalf("withdrawal: %d %v", n, err)
	}
	observed := func() time.Time {
		var at time.Time
		if err := f.admin.Pool.QueryRow(`SELECT max(observed_at) FROM intake_observations`).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	before, calls := observed(), f.source.calls
	f.due(t)
	if n, err := f.poller.PollOnce(ctx, time.Now()); err != nil || n != 0 {
		t.Fatalf("unchanged scan: %d %v", n, err)
	}
	if f.source.calls-calls != 1 || !observed().Equal(before) {
		t.Fatalf("an unchanged ineligible issue was re-read: %d source calls", f.source.calls-calls)
	}
	// A comment advances updated_at: the issue is read again, but creates no work.
	f.source.issue.UpdatedAt = f.source.issue.UpdatedAt.Add(time.Second)
	calls = f.source.calls
	f.due(t)
	if n, err := f.poller.PollOnce(ctx, time.Now()); err != nil || n != 0 || f.source.calls-calls < 3 {
		t.Fatalf("changed ineligible issue not re-read: %d %v calls=%d", n, err, f.source.calls-calls)
	}
	// Re-adding the label is always read and creates work.
	f.source.issue.Labels = []forge.Label{{Name: "ready"}}
	f.source.issue.UpdatedAt = f.source.issue.UpdatedAt.Add(time.Second)
	f.due(t)
	if n, err := f.poller.PollOnce(ctx, time.Now()); err != nil || n != 1 {
		t.Fatalf("relabel: %d %v", n, err)
	}
}
