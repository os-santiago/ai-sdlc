package execution

import (
	"context"
	"encoding/json"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type buildFixture struct {
	f            *lifecycleFixture
	journal      *store.BuildJournal
	cfg          *building.Configuration
	command      building.Command
	raw, receipt []byte
	passwords    map[string]string
}

func newBuildFixture(t *testing.T) buildFixture {
	t.Helper()
	ctx := context.Background()
	var outer authority.Policy
	f, merge, r, mc := mergeJournalFixture(t, func(f *fixture) {
		p := f.Material.Policy
		p.Repository = "os-santiago/saferus"
		p.Environment = "ai-sdlc-production"
		p.Autonomy = "auto-merge-low"
		outer = p
		outer.Autonomy = "auto-deploy"
		outer.Keys = map[string]authority.PublicKey{}
		outer.Grants = nil
		keys := map[string]authority.PublicKey{}
		grants := []authority.Grant{}
		for id, k := range p.Keys {
			if k.Role != "build-controller" {
				keys[id] = k
			}
			switch k.Role {
			case "release-manager", "verifier", "deployer", "relay", "build-controller":
				outer.Keys[id] = k
			}
		}
		for _, g := range p.Grants {
			if authority.OuterOperation(g.Operation) {
				outer.Grants = append(outer.Grants, g)
			} else if g.Operation != "record-attempt" && g.Operation != "record-release" {
				grants = append(grants, g)
			}
		}
		p.Keys = keys
		p.Grants = grants
		f.Material.Policy = p
		f.Config.Repository = p.Repository
		f.command.Repository = p.Repository
	})
	claimed := queuedMerge(t, merge, r)
	proof := completeMerge(t, merge, claimed, mc)
	if e := merge.FinishMerge(ctx, claimed, proof); e != nil {
		t.Fatal(e)
	}
	passwords := map[string]string{}
	for role := range f.Material.Identities {
		passwords[role] = strings.ReplaceAll(role, "-", "_") + strings.Repeat("q", 40)
	}
	if e := f.Admin.ProvisionRolesForBuild(ctx, passwords, "verification", true, true, true, true); e != nil {
		t.Fatal(e)
	}
	u, e := url.Parse(os.Getenv("AI_SDLC_TEST_DATABASE_URL"))
	if e != nil {
		t.Fatal(e)
	}
	u.User = url.UserPassword("aisdlc_build_controller", passwords["build-controller"])
	db, e := store.Open(ctx, u.String())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { db.Close() })
	input, e := db.ReadBuilderInput(ctx, mc.ID())
	if e != nil {
		t.Fatal("historical build input", e)
	}
	if input.Source.SourceSHA == mc.HeadSHA || input.Source.SourceSHA == mc.BaseSHA {
		t.Fatal("build projected unmerged source")
	}
	slot := building.Slot{ClusterUID: store.NewID(), NamespaceUID: store.NewID(), PVCUID: store.NewID(), PVUID: store.NewID(), NodeUID: store.NewID(), NodeBootID: store.NewID(), StorageIdentitySHA256: strings.Repeat("a", 64), Namespace: "ai-sdlc-builds", PVC: "build-slot-0", Node: "fixture-node", CapacityBytes: 4 << 30, InodeLimit: 32768}
	cfg := &building.Configuration{Inner: f.Material.Policy, Outer: outer, ProfileSHA256: strings.Repeat("b", 64), IntakeConfigSHA256: f.Config.Digest(), IssueMaxAgeSeconds: 60, Slots: []building.Slot{slot}}
	now := time.Now()
	c := building.Command{Version: building.Version, MergeID: mc.ID(), MergeRecordSHA256: input.Source.MergeRecordSHA256, Repository: mc.Repository, Environment: outer.Environment, IssueNumber: mc.IssueNumber, IssueRevision: input.IssueRevision, Generation: input.Generation, SourceSHA: input.Source.SourceSHA, TreeSHA: input.Source.TreeSHA, Recipe: building.Recipe, ProfileSHA256: cfg.ProfileDigest(), IntakeConfigSHA256: cfg.IntakeConfigSHA256, InnerPolicyRevision: cfg.Inner.Revision(), OuterPolicyRevision: cfg.Outer.Revision(), IssuedAt: now.UnixMilli(), ExpiresAt: now.Add(10 * time.Minute).UnixMilli()}
	raw, _ := json.Marshal(c)
	signed, e := f.Material.Identities["release-manager"].Seal(outer, c.ID(), "build-merged", raw)
	if e != nil {
		t.Fatal(e)
	}
	receipt, _ := json.Marshal(signed)
	return buildFixture{f, store.NewBuildJournal(db, func() (building.Configuration, error) { return *cfg, nil }), cfg, c, raw, receipt, passwords}
}
func TestBuildJournalOwnIdentityCurrentMergeAndOneClaim(t *testing.T) {
	b := newBuildFixture(t)
	ctx := context.Background()
	if store.NewBuildJournal(b.f.db, func() (building.Configuration, error) { return *b.cfg, nil }).QueueBuild(ctx, b.raw, b.receipt) == nil {
		t.Fatal("worker queued build")
	}
	if e := b.journal.QueueBuild(ctx, b.raw, b.receipt); e != nil {
		t.Fatal("queue", e)
	}
	var wg sync.WaitGroup
	claims := make(chan store.BuildClaim, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, e := b.journal.ClaimBuild(ctx, b.command.ID(), b.cfg.Slots[0])
			if e == nil {
				claims <- c
			}
		}()
	}
	wg.Wait()
	close(claims)
	var winner store.BuildClaim
	n := 0
	for c := range claims {
		winner = c
		n++
	}
	if n != 1 {
		t.Fatalf("claims=%d", n)
	}
	if e := b.journal.CheckBuild(ctx, winner); e != nil {
		t.Fatal("loser stopped winning claim", e)
	}
	effect, e := b.journal.BeginBuildEffect(ctx, winner, strings.Repeat("c", 64))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = b.journal.BeginBuildEffect(ctx, winner, strings.Repeat("c", 64)); e == nil {
		t.Fatal("replayed create intent")
	}
	uid := store.NewID()
	if e = b.journal.AcknowledgeBuildCreated(ctx, effect, uid); e != nil {
		t.Fatal(e)
	}
	if e = b.journal.AcknowledgeBuildCreated(ctx, effect, store.NewID()); e == nil {
		t.Fatal("resource UID replaced")
	}
	name, actual, e := b.journal.ReadBuildResource(ctx, winner)
	if e != nil || name != effect.Name || actual != uid {
		t.Fatal("resource custody", e)
	}
	for _, q := range []string{`UPDATE build_requests SET payload=payload WHERE false`, `UPDATE build_slots SET slot=slot WHERE false`, `DELETE FROM build_slots WHERE false`, `UPDATE merge_requests SET state=state WHERE false`, `UPDATE intake_observations SET observed_at=clock_timestamp() WHERE false`} {
		if _, e = b.journal.Pool.Exec(q); e == nil {
			t.Fatal("forbidden builder write", q)
		}
	}
	if e = b.f.Admin.ProvisionRolesForMerge(ctx, b.passwords, "verification", true, true, true); e != nil {
		t.Fatal(e)
	}
	if b.journal.CheckBuild(ctx, winner) == nil {
		t.Fatal("build privileges survived disable")
	}
}
func TestBuildJournalRejectsChangedCurrentAuthority(t *testing.T) {
	for _, change := range []string{"inner-freeze", "outer-freeze", "profile", "slot", "generation", "stale-issue", "stale-scan", "work-blocked", "wrong-source"} {
		t.Run(change, func(t *testing.T) {
			b := newBuildFixture(t)
			ctx := context.Background()
			if e := b.journal.QueueBuild(ctx, b.raw, b.receipt); e != nil {
				t.Fatal(e)
			}
			c, e := b.journal.ClaimBuild(ctx, b.command.ID(), b.cfg.Slots[0])
			if e != nil {
				t.Fatal(e)
			}
			switch change {
			case "inner-freeze":
				b.cfg.Inner.Freeze = true
			case "outer-freeze":
				b.cfg.Outer.Freeze = true
			case "profile":
				b.cfg.ProfileSHA256 = strings.Repeat("f", 64)
			case "slot":
				b.cfg.Slots[0].NodeBootID = store.NewID()
			case "generation":
				_, e = b.f.Admin.Pool.Exec(`UPDATE intake_heads SET generation=generation+1`)
			case "stale-issue":
				_, e = b.f.Admin.Pool.Exec(`UPDATE intake_observations SET observed_at=clock_timestamp()-interval '10 minutes'`)
			case "stale-scan":
				_, e = b.f.Admin.Pool.Exec(`UPDATE intake_cursors SET watermark=clock_timestamp()-interval '10 minutes'`)
			case "work-blocked":
				_, e = b.f.Admin.Pool.Exec(`UPDATE work_items SET status='blocked'`)
			case "wrong-source":
				_, e = b.f.Admin.Pool.Exec(`UPDATE merged_records SET effect_digest=repeat('f',64)`)
			}
			if e != nil {
				t.Fatal(e)
			}
			if b.journal.CheckBuild(ctx, c) == nil {
				t.Fatal("stale build authority remained live")
			}
			if _, e = b.journal.BeginBuildEffect(ctx, c, strings.Repeat("c", 64)); e == nil {
				t.Fatal("revoked authority began external effect")
			}
		})
	}
}
func TestBuildUnknownCreateQuarantinesMergeAndPhysicalSlot(t *testing.T) {
	b := newBuildFixture(t)
	ctx := context.Background()
	if e := b.journal.QueueBuild(ctx, b.raw, b.receipt); e != nil {
		t.Fatal(e)
	}
	c, e := b.journal.ClaimBuild(ctx, b.command.ID(), b.cfg.Slots[0])
	if e != nil {
		t.Fatal(e)
	}
	if _, e = b.journal.BeginBuildEffect(ctx, c, strings.Repeat("c", 64)); e != nil {
		t.Fatal(e)
	}
	if e = b.journal.StopBuild(ctx, c, "uncertain"); e != nil {
		t.Fatal(e)
	}
	if _, e = b.journal.ClaimBuild(ctx, c.ID, b.cfg.Slots[0]); e == nil {
		t.Fatal("quarantined build reclaimed")
	}
	changed := b.command
	changed.IssuedAt++
	changed.ExpiresAt++
	raw, _ := json.Marshal(changed)
	r, e := b.f.Material.Identities["release-manager"].Seal(b.cfg.Outer, changed.ID(), "build-merged", raw)
	if e != nil {
		t.Fatal(e)
	}
	receipt, _ := json.Marshal(r)
	if b.journal.QueueBuild(ctx, raw, receipt) == nil {
		t.Fatal("new envelope replayed same merge")
	}
	if b.f.Count(t, `SELECT count(*) FROM build_slots`) != 1 {
		t.Fatal("physical workspace became reusable")
	}
	if _, e = b.journal.Pool.Exec(`DELETE FROM build_effects WHERE false`); e == nil {
		t.Fatal("unknown effect erased")
	}
}

func TestBuildPhysicalSlotCannotBeReintroducedWithNewKubernetesIDs(t *testing.T) {
	b := newBuildFixture(t)
	ctx := context.Background()
	if e := b.journal.QueueBuild(ctx, b.raw, b.receipt); e != nil {
		t.Fatal(e)
	}
	c, e := b.journal.ClaimBuild(ctx, b.command.ID(), b.cfg.Slots[0])
	if e != nil {
		t.Fatal(e)
	}
	if _, e = b.journal.BeginBuildEffect(ctx, c, strings.Repeat("c", 64)); e != nil {
		t.Fatal(e)
	}
	if e = b.journal.StopBuild(ctx, c, "uncertain"); e != nil {
		t.Fatal(e)
	}
	replacement := b.cfg.Slots[0]
	replacement.ClusterUID = store.NewID()
	replacement.NodeUID = store.NewID()
	replacement.NodeBootID = store.NewID()
	replacement.NamespaceUID = store.NewID()
	replacement.PVCUID = store.NewID()
	replacement.PVUID = store.NewID()
	if replacement.ID() != c.SlotID || replacement.Digest() == c.SlotDigest {
		t.Fatal("physical identity changed or custody digest stayed valid")
	}
	raw, _ := json.Marshal(replacement)
	// A different ownership request still hits the physical primary key before
	// any request FK lookup. With the old NodeUID-based key this would attempt a
	// new allocation (and fail the missing fixture request FK), not return zero.
	result, e := b.journal.Pool.Exec(`INSERT INTO build_slots(slot_id,request_id,slot_digest,slot) VALUES($1,$2,$3,$4) ON CONFLICT(slot_id) DO NOTHING`, replacement.ID(), "build-"+strings.Repeat("e", 64), replacement.Digest(), raw)
	if e != nil {
		t.Fatal("replacement bypassed physical consumption key", e)
	}
	n, e := result.RowsAffected()
	if e != nil || n != 0 {
		t.Fatal("consumed physical backing allocated again")
	}
}
func TestBuildClaimCannotOutliveExpiryWhileWaitingForRowLock(t *testing.T) {
	b := newBuildFixture(t)
	ctx := context.Background()
	cmd := b.command
	cmd.ExpiresAt = time.Now().Add(700 * time.Millisecond).UnixMilli()
	raw, _ := json.Marshal(cmd)
	signed, e := b.f.Material.Identities["release-manager"].Seal(b.cfg.Outer, cmd.ID(), "build-merged", raw)
	if e != nil {
		t.Fatal(e)
	}
	receipt, _ := json.Marshal(signed)
	if e = b.journal.QueueBuild(ctx, raw, receipt); e != nil {
		t.Fatal(e)
	}
	tx, e := b.f.Admin.Pool.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback()
	var id string
	if e = tx.QueryRow(`SELECT id FROM build_requests WHERE id=$1 FOR UPDATE`, cmd.ID()).Scan(&id); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { _, e := b.journal.ClaimBuild(ctx, cmd.ID(), b.cfg.Slots[0]); done <- e }()
	time.Sleep(time.Until(time.UnixMilli(cmd.ExpiresAt).Add(50 * time.Millisecond)))
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	if e = <-done; e == nil {
		t.Fatal("expired blocked claimant acquired authority")
	}
	if b.f.Count(t, `SELECT count(*) FROM build_slots`) != 0 {
		t.Fatal("expired claimant consumed a slot")
	}
	if e = b.journal.ExpireBuilds(ctx); e != nil {
		t.Fatal(e)
	}
	if b.f.Count(t, `SELECT count(*) FROM build_requests WHERE state='failed' AND reason='expired'`) != 1 {
		t.Fatal("expired request remained eligible")
	}
}
