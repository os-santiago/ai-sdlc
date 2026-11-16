package execution

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildworkload"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/delivery"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

func TestBuildInboxAtomicReplayAndOriginalQueue(t *testing.T) {
	b := newBuildFixture(t)
	extendBuildQueueAuthority(t, &b)
	ctx := context.Background()
	var signed authority.Receipt
	if json.Unmarshal(b.receipt, &signed) != nil {
		t.Fatal("fixture receipt")
	}
	envelope := func() delivery.Envelope {
		carried, e := b.f.Material.Identities["relay"].Carry(b.cfg.Outer, signed, http.MethodPost, "/internal/build-merged", time.Now())
		if e != nil {
			t.Fatal(e)
		}
		return delivery.Envelope{Receipt: signed, Transport: carried, Payload: b.raw}
	}
	in := delivery.Inbox{DB: b.journal.DB, Target: "build-controller"}
	effect := buildflow.InboxEffect(b.journal)
	failed := envelope()
	_, _, e := in.Accept(ctx, b.cfg.Outer, failed, "build-merged", http.MethodPost, "/internal/build-merged", time.Now(), func(ctx context.Context, tx *sql.Tx, r authority.Receipt, p []byte) ([]byte, error) {
		if _, e := effect(ctx, tx, r, p); e != nil {
			return nil, e
		}
		return nil, errors.New("fixture crash before commit")
	})
	if e == nil {
		t.Fatal("injected failure accepted")
	}
	if id, e := b.journal.NextBuild(ctx); e != nil || id != "" {
		t.Fatal("queue escaped failed inbox transaction", e)
	}
	// The rolled-back nonce may now commit; later redelivery uses a fresh nonce.
	for n := 0; n < 2; n++ {
		env := failed
		if n > 0 {
			env = envelope()
		}
		_, duplicate, e := in.Accept(ctx, b.cfg.Outer, env, "build-merged", http.MethodPost, "/internal/build-merged", time.Now(), effect)
		if e != nil || duplicate != (n == 1) {
			t.Fatalf("delivery %d duplicate=%v: %v", n, duplicate, e)
		}
	}
	id, e := b.journal.NextBuild(ctx)
	if e != nil || id != b.command.ID() {
		t.Fatal("stored queue identity", e)
	}
	request, e := b.journal.ReadQueuedBuild(ctx, id)
	if e != nil || request.Command != b.command {
		t.Fatal("stored request", e)
	}
	slot, e := b.journal.UnusedBuildSlot(ctx)
	if e != nil || slot != b.cfg.Slots[0] {
		t.Fatal("unused slot", e)
	}
	claim, e := b.journal.ClaimBuild(ctx, id, slot)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = b.journal.ReadQueuedBuild(ctx, id); e == nil {
		t.Fatal("claimed request reread for resume")
	}
	if id, e = b.journal.NextBuild(ctx); e != nil || id != "" {
		t.Fatal("claimed request selected")
	}
	if e = b.journal.StopBuild(ctx, claim, "uncertain"); e != nil {
		t.Fatal(e)
	}
	if _, e = b.journal.UnusedBuildSlot(ctx); e == nil {
		t.Fatal("failed claim released physical slot")
	}
}
func TestBuildQueueReadRequiresFreshAuthority(t *testing.T) {
	for _, change := range []string{"inner-freeze", "outer-freeze", "slot-change", "issue-closed", "foreign-role"} {
		t.Run(change, func(t *testing.T) {
			b := newBuildFixture(t)
			ctx := context.Background()
			if e := b.journal.QueueBuild(ctx, b.raw, b.receipt); e != nil {
				t.Fatal(e)
			}
			j := b.journal
			switch change {
			case "inner-freeze":
				b.cfg.Inner.Freeze = true
			case "outer-freeze":
				b.cfg.Outer.Freeze = true
			case "slot-change":
				b.cfg.Slots[0].PVCUID = store.NewID()
			case "issue-closed":
				if _, e := b.f.Admin.Pool.ExecContext(ctx, `UPDATE work_items SET status='done' WHERE uuid=$1`, b.f.command.WorkItemID); e != nil {
					t.Fatal(e)
				}
			case "foreign-role":
				j = store.NewBuildJournal(b.f.db, func() (building.Configuration, error) { return *b.cfg, nil })
			}
			if _, e := j.ReadQueuedBuild(ctx, b.command.ID()); e == nil {
				t.Fatal("stale or foreign queue accepted")
			}
		})
	}
}

func extendBuildQueueAuthority(t *testing.T, b *buildFixture) {
	t.Helper()
	b.command.ExpiresAt = time.Now().Add(19 * time.Minute).UnixMilli()
	b.raw, _ = json.Marshal(b.command)
	signed, e := b.f.Material.Identities["release-manager"].Seal(b.cfg.Outer, b.command.ID(), "build-merged", b.raw)
	if e != nil {
		t.Fatal(e)
	}
	b.receipt, _ = json.Marshal(signed)
	b.compiled, e = buildworkload.Compile(*b.cfg, b.compiled.Retained.Profile, signed, b.raw, b.cfg.Slots[0], time.Now())
	if e != nil {
		t.Fatal(e)
	}
}
func TestBuildQueueSkipsUnusableBudgetAndRevision(t *testing.T) {
	for _, change := range []string{"budget", "profile", "intake", "inner", "outer", "work-stopped", "issue-generation"} {
		t.Run(change, func(t *testing.T) {
			b := newBuildFixture(t)
			ctx := context.Background()
			extendBuildQueueAuthority(t, &b)
			if e := b.journal.QueueBuild(ctx, b.raw, b.receipt); e != nil {
				t.Fatal(e)
			}
			if id, e := b.journal.NextBuild(ctx); e != nil || id != b.command.ID() {
				t.Fatal("viable queued build not selected", e)
			}
			switch change {
			case "budget":
				if _, e := b.f.Admin.Pool.ExecContext(ctx, `UPDATE build_requests SET expires_at=clock_timestamp()+interval '15 minutes' WHERE id=$1`, b.command.ID()); e != nil {
					t.Fatal(e)
				}
			case "work-stopped":
				if _, e := b.f.Admin.Pool.ExecContext(ctx, `UPDATE work_items SET status='done' WHERE uuid=$1`, b.f.command.WorkItemID); e != nil {
					t.Fatal(e)
				}
			case "issue-generation":
				if _, e := b.f.Admin.Pool.ExecContext(ctx, `UPDATE intake_heads SET generation=generation+1 WHERE repository=$1`, b.command.Repository); e != nil {
					t.Fatal(e)
				}
			case "profile":
				b.cfg.Slots[0].PVCUID = store.NewID()
			case "intake":
				b.cfg.IntakeConfigSHA256 = authority.Digest([]byte("changed intake"))
			case "inner":
				b.cfg.Inner.Grants = b.cfg.Inner.Grants[1:]
			case "outer":
				b.cfg.Outer.Grants = b.cfg.Outer.Grants[1:]
			}
			if id, e := b.journal.NextBuild(ctx); e != nil || id != "" {
				t.Fatal("unusable queued build blocked selection", e)
			}
		})
	}
}

func TestBuildCreateReceiptSurvivesFreezeOnlyForCleanup(t *testing.T) {
	b := newBuildFixture(t)
	ctx := context.Background()
	if e := b.journal.QueueBuild(ctx, b.raw, b.receipt); e != nil {
		t.Fatal(e)
	}
	claim, e := b.journal.ClaimBuild(ctx, b.command.ID(), b.cfg.Slots[0])
	if e != nil {
		t.Fatal(e)
	}
	effect, e := b.journal.BeginBuildEffect(ctx, claim, b.plan(), b.compiled.Retained)
	if e != nil {
		t.Fatal(e)
	}
	b.cfg.Outer.Freeze = true
	uid := store.NewID()
	wrong := effect
	wrong.Token = store.NewID()
	if e = b.journal.RememberBuildCreated(ctx, wrong, uid); e == nil {
		t.Fatal("foreign effect recorded")
	}
	if e = b.journal.RememberBuildCreated(ctx, effect, uid); e != nil {
		t.Fatal("original cleanup identity lost after freeze", e)
	}
	if e = b.journal.RememberBuildCreated(ctx, effect, store.NewID()); e == nil {
		t.Fatal("original UID replaced")
	}
	if e = b.journal.CheckBuild(ctx, claim); e == nil {
		t.Fatal("cleanup receipt authorized execution")
	}
	if _, e = b.journal.BeginPublishEffect(ctx, claim); e == nil {
		t.Fatal("cleanup receipt authorized publication")
	}
	if e = b.journal.StopBuild(ctx, claim, "revoked"); e != nil {
		t.Fatal(e)
	}
	cleanup, e := b.journal.ReadBuildCleanup(ctx, claim)
	if e != nil || cleanup.UnknownCreate || len(cleanup.Resources) != 1 || cleanup.Resources[0].UID != uid {
		t.Fatal("cleanup lost original response", e)
	}
}

func TestBuildLateACKCannotInvalidateFinalCleanup(t *testing.T) {
	for _, order := range []string{"ack-before-cleanup-commit", "ack-after-cleanup-commit"} {
		t.Run(order, func(t *testing.T) {
			b := newBuildFixture(t)
			ctx := context.Background()
			if e := b.journal.QueueBuild(ctx, b.raw, b.receipt); e != nil {
				t.Fatal(e)
			}
			claim, e := b.journal.ClaimBuild(ctx, b.command.ID(), b.cfg.Slots[0])
			if e != nil {
				t.Fatal(e)
			}
			effect, e := b.journal.BeginBuildEffect(ctx, claim, b.plan(), b.compiled.Retained)
			if e != nil {
				t.Fatal(e)
			}
			if e = b.journal.StopBuild(ctx, claim, "uncertain"); e != nil {
				t.Fatal(e)
			}
			original, e := b.journal.ReadBuildCleanup(ctx, claim)
			if e != nil {
				t.Fatal(e)
			}
			proof := store.BuildCleanupProof{Version: "ai-sdlc.build-cleanup/v1", BuildID: claim.ID, SlotDigest: claim.SlotDigest, UnknownCreate: original.UnknownCreate, Resources: original.Resources, ObservedAbsentAt: time.Now().UnixMilli()}
			uid := store.NewID()
			if order == "ack-before-cleanup-commit" {
				if e = b.journal.RememberBuildCreated(ctx, effect, uid); e != nil {
					t.Fatal(e)
				}
				if e = b.journal.RecordBuildCleanup(ctx, claim, proof); e == nil {
					t.Fatal("stale cleanup snapshot committed after ACK")
				}
				next, e := b.journal.NextBuildRecovery(ctx)
				if e != nil || next.ID != claim.ID {
					t.Fatal("late ACK removed cleanup obligation", e)
				}
			} else {
				if e = b.journal.RecordBuildCleanup(ctx, claim, proof); e != nil {
					t.Fatal(e)
				}
				if e = b.journal.RememberBuildCreated(ctx, effect, uid); e == nil {
					t.Fatal("late ACK changed immutable cleanup evidence")
				}
				got, e := b.journal.ReadBuildCleanup(ctx, claim)
				if e != nil || !got.UnknownCreate || len(got.Resources) != 0 {
					t.Fatal("closed quarantine changed", e)
				}
			}
		})
	}
}

func TestBuildACKWaitingOnCleanupLockObservesCommittedClosure(t *testing.T) {
	b := newBuildFixture(t)
	ctx := context.Background()
	if e := b.journal.QueueBuild(ctx, b.raw, b.receipt); e != nil {
		t.Fatal(e)
	}
	claim, e := b.journal.ClaimBuild(ctx, b.command.ID(), b.cfg.Slots[0])
	if e != nil {
		t.Fatal(e)
	}
	effect, e := b.journal.BeginBuildEffect(ctx, claim, b.plan(), b.compiled.Retained)
	if e != nil {
		t.Fatal(e)
	}
	if e = b.journal.StopBuild(ctx, claim, "uncertain"); e != nil {
		t.Fatal(e)
	}
	original, e := b.journal.ReadBuildCleanup(ctx, claim)
	if e != nil {
		t.Fatal(e)
	}
	proof := store.BuildCleanupProof{Version: "ai-sdlc.build-cleanup/v1", BuildID: claim.ID, SlotDigest: claim.SlotDigest, UnknownCreate: original.UnknownCreate, Resources: original.Resources, ObservedAbsentAt: time.Now().UnixMilli()}
	raw, _ := json.Marshal(proof)
	// Isolated administrative fixture holds exactly the row lock used by the
	// cleanup transaction, then commits its immutable record while ACK is waiting.
	tx, e := b.f.Admin.Pool.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, `SELECT id FROM build_requests WHERE id=$1 FOR UPDATE`, claim.ID); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- b.journal.RememberBuildCreated(ctx, effect, store.NewID()) }()
	until := time.Now().Add(2 * time.Second)
	for {
		var waiting bool
		if e = b.f.Admin.Pool.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE usename='aisdlc_build_controller' AND wait_event_type='Lock' AND query LIKE 'SELECT r.payload,r.receipt,r.inner_policy%')`).Scan(&waiting); e != nil {
			t.Fatal(e)
		}
		if waiting {
			break
		}
		if time.Now().After(until) {
			t.Fatal("ACK did not wait for cleanup row lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO build_cleanups(request_id,digest,proof) VALUES($1,$2,$3)`, claim.ID, authority.Digest(raw), raw); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	if e = <-done; e == nil {
		t.Fatal("waiting ACK used a snapshot from before cleanup commit")
	}
	got, e := b.journal.ReadBuildCleanup(ctx, claim)
	if e != nil || !got.UnknownCreate || len(got.Resources) != 0 {
		t.Fatal("immutable cleanup resource set changed", e)
	}
}

func TestBuildInboxRejectsSignedNoncanonicalFieldNames(t *testing.T) {
	b := newBuildFixture(t)
	extendBuildQueueAuthority(t, &b)
	raw := bytes.Replace(b.raw, []byte(`"profileSHA256"`), []byte(`"ProfileSHA256"`), 1)
	signed, e := b.f.Material.Identities["release-manager"].Seal(b.cfg.Outer, b.command.ID(), "build-merged", raw)
	if e != nil {
		t.Fatal(e)
	}
	carried, e := b.f.Material.Identities["relay"].Carry(b.cfg.Outer, signed, http.MethodPost, "/internal/build-merged", time.Now())
	if e != nil {
		t.Fatal(e)
	}
	in := delivery.Inbox{DB: b.journal.DB, Target: "build-controller"}
	_, _, e = in.Accept(context.Background(), b.cfg.Outer, delivery.Envelope{Receipt: signed, Transport: carried, Payload: raw}, "build-merged", http.MethodPost, "/internal/build-merged", time.Now(), buildflow.InboxEffect(b.journal))
	if e == nil {
		t.Fatal("accepted a signed command omitted by the queue selector")
	}
	if b.f.Count(t, `SELECT count(*) FROM build_requests`) != 0 {
		t.Fatal("unselectable request persisted")
	}
}
