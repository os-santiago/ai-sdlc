package execution

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildworkload"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

func TestBuildContextAtomicRetentionAndRestrictedRoles(t *testing.T) {
	b := newBuildFixture(t)
	ctx := context.Background()
	c := buildClaim(t, b)
	// A failure after context insertion rolls back context, plan and intention.
	if _, e := b.f.Admin.Pool.Exec(`REVOKE INSERT(request_id,kind,token,request_digest,resource_name) ON build_effects FROM aisdlc_build_controller`); e != nil {
		t.Fatal(e)
	}
	if _, e := b.journal.BeginBuildEffect(ctx, c, b.plan(), b.compiled.Retained); e == nil {
		t.Fatal("missing grant accepted")
	}
	for _, table := range []string{"build_contexts", "build_plans", "build_effects"} {
		if b.f.Count(t, `SELECT count(*) FROM `+table) != 0 {
			t.Fatal("partial retention", table)
		}
	}
	if _, e := b.f.Admin.Pool.Exec(`GRANT INSERT(request_id,kind,token,request_digest,resource_name) ON build_effects TO aisdlc_build_controller`); e != nil {
		t.Fatal(e)
	}
	w := buildWriterDone(t, b, c)
	pub := buildPublisherDone(t, b, c)
	if e := b.journal.FinishBuild(ctx, c, buildResult(b, w, pub)); e != nil {
		t.Fatal(e)
	}
	u, e := url.Parse(os.Getenv("AI_SDLC_TEST_DATABASE_URL"))
	if e != nil {
		t.Fatal(e)
	}
	u.User = url.UserPassword("aisdlc_verifier", b.passwords["verifier"])
	reader, e := store.Open(ctx, u.String())
	if e != nil {
		t.Fatal(e)
	}
	defer reader.Close()
	record, e := reader.ReadBuiltRecord(ctx, c.ID)
	if e != nil || record.Context.ValidateHistorical(record.Command, record.Plan) != nil {
		t.Fatal("independent context read", e)
	}
	oldDigest := record.Digest()
	record.Context.Build[0] = 'X'
	record.Context.Configuration.Slots[0].Node = "changed"
	b.cfg.Outer.Freeze = true
	again, e := reader.ReadBuiltRecord(ctx, c.ID)
	if e != nil || again.Digest() != oldDigest {
		t.Fatal("historical value aliased caller or current policy", e)
	}
	for _, db := range []*store.DB{reader, b.journal.DB, b.f.db} {
		for _, q := range []string{`UPDATE build_contexts SET context=context`, `DELETE FROM build_contexts`, `INSERT INTO build_contexts(request_id,digest,context) SELECT request_id,digest,context FROM build_contexts`} {
			if _, e := db.Pool.Exec(q); e == nil {
				t.Fatal("context mutation accepted")
			}
		}
	}
	if _, e := b.f.db.Pool.QueryContext(ctx, `SELECT context FROM build_contexts`); e == nil {
		t.Fatal("worker read context")
	}
}

func TestBuildContextRejectsMissingAndAlteredHistoricalBytes(t *testing.T) {
	for _, name := range []string{"missing", "digest", "compiler", "self-hashed-request", "late-retention"} {
		t.Run(name, func(t *testing.T) {
			b := newBuildFixture(t)
			ctx := context.Background()
			c := buildClaim(t, b)
			w := buildWriterDone(t, b, c)
			pub := buildPublisherDone(t, b, c)
			if e := b.journal.FinishBuild(ctx, c, buildResult(b, w, pub)); e != nil {
				t.Fatal(e)
			}
			var e error
			switch name {
			case "missing":
				_, e = b.f.Admin.Pool.Exec(`DELETE FROM build_contexts WHERE request_id=$1`, c.ID)
			case "digest":
				_, e = b.f.Admin.Pool.Exec(`UPDATE build_contexts SET digest=repeat('f',64) WHERE request_id=$1`, c.ID)
			case "late-retention":
				_, e = b.f.Admin.Pool.Exec(`UPDATE build_contexts SET recorded_at=clock_timestamp() WHERE request_id=$1`, c.ID)
			default:
				r := b.compiled.Retained
				if name == "compiler" {
					r.Compiler = "future"
				} else {
					r.Build = append(r.Build, ' ')
				}
				raw, err := r.Encode()
				if err != nil {
					t.Fatal(err)
				}
				_, e = b.f.Admin.Pool.Exec(`UPDATE build_contexts SET context=$2,digest=$3 WHERE request_id=$1`, c.ID, raw, authority.Digest(raw))
			}
			if e != nil {
				t.Fatal(e)
			}
			if _, e = b.journal.ReadOwnBuiltRecord(ctx, c.ID); e == nil {
				t.Fatal("unverifiable historical record accepted")
			}
		})
	}
}

func TestBuildContextRejectsBadInputBeforeAnyIntent(t *testing.T) {
	for _, name := range []string{"missing", "wrong-slot", "changed-config", "wrong-plan"} {
		t.Run(name, func(t *testing.T) {
			b := newBuildFixture(t)
			ctx := context.Background()
			c := buildClaim(t, b)
			r := b.compiled.Retained
			p := b.plan()
			switch name {
			case "missing":
				r = buildworkload.RetainedContext{}
			case "wrong-slot":
				r.Slot.PVCUID = store.NewID()
			case "changed-config":
				r.Configuration.IssueMaxAgeSeconds++
			case "wrong-plan":
				p.DefinitionSHA256 = strings.Repeat("e", 64)
			}
			if _, e := b.journal.BeginBuildEffect(ctx, c, p, r); e == nil {
				t.Fatal("invalid context accepted")
			}
			for _, table := range []string{"build_contexts", "build_plans", "build_effects"} {
				if b.f.Count(t, `SELECT count(*) FROM `+table) != 0 {
					t.Fatal("partial invalid intent", table)
				}
			}
		})
	}
}

func historicalEndingBuild(t *testing.T) buildFixture {
	t.Helper()
	b := newBuildFixture(t)
	b.command.IssuedAt = time.Now().Add(-18 * time.Minute).UnixMilli()
	b.command.ExpiresAt = time.Now().Add(3 * time.Second).UnixMilli()
	b.raw, _ = json.Marshal(b.command)
	signed, e := b.f.Material.Identities["release-manager"].Seal(b.cfg.Outer, b.command.ID(), "build-merged", b.raw)
	if e != nil {
		t.Fatal(e)
	}
	b.receipt, _ = json.Marshal(signed)
	// Synthetic isolated historical workload; this does not invoke a transport.
	b.compiled, e = buildworkload.Compile(*b.cfg, b.compiled.Retained.Profile, signed, b.raw, b.cfg.Slots[0], time.UnixMilli(b.command.IssuedAt))
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func TestBuildContextHistorySurvivesRealExpiryWithoutNewAuthority(t *testing.T) {
	b := historicalEndingBuild(t)
	ctx := context.Background()
	c := buildClaim(t, b)
	w := buildWriterDone(t, b, c)
	p := buildPublisherDone(t, b, c)
	if e := b.journal.FinishBuild(ctx, c, buildResult(b, w, p)); e != nil {
		t.Fatal(e)
	}
	time.Sleep(time.Until(time.UnixMilli(b.command.ExpiresAt)) + 50*time.Millisecond)
	b.cfg.Outer.Freeze = true
	if _, e := b.journal.ReadOwnBuiltRecord(ctx, c.ID); e != nil {
		t.Fatal("valid history lost after expiry", e)
	}
	if _, e := b.journal.BeginBuildEffect(ctx, c, b.plan(), b.compiled.Retained); e == nil {
		t.Fatal("history granted new create authority")
	}
}

func TestBuildContextExpiryWhileWaitingForLockLeavesNoIntent(t *testing.T) {
	b := historicalEndingBuild(t)
	ctx := context.Background()
	c := buildClaim(t, b)
	tx, e := b.f.Admin.Pool.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback()
	if _, e = tx.Exec(`SELECT id FROM build_requests WHERE id=$1 FOR UPDATE`, c.ID); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { _, e := b.journal.BeginBuildEffect(ctx, c, b.plan(), b.compiled.Retained); done <- e }()
	var blocker int
	if e = tx.QueryRow(`SELECT pg_backend_pid()`).Scan(&blocker); e != nil {
		t.Fatal(e)
	}
	blocked := false
	for time.Now().Before(time.UnixMilli(b.command.ExpiresAt)) {
		if e = b.f.Admin.Pool.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE usename='aisdlc_build_controller' AND wait_event_type='Lock' AND $1=ANY(pg_blocking_pids(pid)))`, blocker).Scan(&blocked); e != nil {
			t.Fatal(e)
		}
		if blocked {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !blocked {
		t.Fatal("build intention did not observably wait for the original row lock before expiry")
	}
	time.Sleep(time.Until(time.UnixMilli(b.command.ExpiresAt)) + 50*time.Millisecond)
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	if e = <-done; e == nil {
		t.Fatal("expiry while blocked authorized an effect")
	}
	for _, table := range []string{"build_contexts", "build_plans", "build_effects"} {
		if b.f.Count(t, `SELECT count(*) FROM `+table) != 0 {
			t.Fatal("expired lock waiter left partial intent", table)
		}
	}
}
