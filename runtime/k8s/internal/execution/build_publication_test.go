package execution

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

func (b buildFixture) plan() building.Plan {
	return building.Plan{Version: "ai-sdlc.build-plan/v1", BuildID: b.command.ID(), SourceSHA: b.command.SourceSHA, TreeSHA: b.command.TreeSHA, ProfileSHA256: b.command.ProfileSHA256, SlotDigest: b.cfg.Slots[0].Digest(), DefinitionSHA256: strings.Repeat("d", 64), BuildRequestSHA256: strings.Repeat("c", 64), PublishRequestSHA256: strings.Repeat("e", 64), PublisherImage: "quay.io/os-santiago/ai-sdlc@sha256:" + strings.Repeat("f", 64), Output: building.Registry + ":" + strings.TrimPrefix(b.command.ID(), "build-")[:32]}
}
func buildClaim(t *testing.T, b buildFixture) store.BuildClaim {
	t.Helper()
	ctx := context.Background()
	if e := b.journal.QueueBuild(ctx, b.raw, b.receipt); e != nil {
		t.Fatal(e)
	}
	c, e := b.journal.ClaimBuild(ctx, b.command.ID(), b.cfg.Slots[0])
	if e != nil {
		t.Fatal(e)
	}
	return c
}
func buildObservation(b buildFixture, phase, uid string) building.Termination {
	p := b.plan()
	s := b.cfg.Slots[0]
	digest := p.BuildRequestSHA256
	id := "a"
	if phase == "publish" {
		digest = p.PublishRequestSHA256
		id = "b"
	}
	now := time.Now().UnixMilli()
	return building.Termination{Version: "ai-sdlc.build-termination/v1", Phase: phase, RequestSHA256: digest, PipelineRunUID: uid, TaskRunUID: store.NewID(), PodUID: store.NewID(), NodeUID: s.NodeUID, NodeBootID: s.NodeBootID, SlotDigest: s.Digest(), PodSpecSHA256: strings.Repeat("1", 64), ObservedAt: now, Containers: []building.ContainerExit{
		{Name: "prepare", Kind: "init", ImageDigest: "sha256:" + strings.Repeat("2", 64), ContainerID: "containerd://" + strings.Repeat(id, 63) + "1", StartedAt: now, FinishedAt: now},
		{Name: "step-verify", Kind: "normal", ImageDigest: "sha256:" + strings.Repeat("3", 64), ContainerID: "containerd://" + strings.Repeat(id, 63) + "2", StartedAt: now, FinishedAt: now},
	}}
}
func buildWriterDone(t *testing.T, b buildFixture, c store.BuildClaim) building.Termination {
	t.Helper()
	ctx := context.Background()
	effect, e := b.journal.BeginBuildEffect(ctx, c, b.plan())
	if e != nil {
		t.Fatal(e)
	}
	uid := store.NewID()
	if e = b.journal.AcknowledgeBuildCreated(ctx, effect, uid); e != nil {
		t.Fatal(e)
	}
	o := buildObservation(b, "build", uid)
	if e = b.journal.RecordBuildTermination(ctx, c, o); e != nil {
		t.Fatal(e)
	}
	return o
}
func buildPublisherDone(t *testing.T, b buildFixture, c store.BuildClaim) building.Termination {
	t.Helper()
	ctx := context.Background()
	effect, e := b.journal.BeginPublishEffect(ctx, c)
	if e != nil {
		t.Fatal(e)
	}
	uid := store.NewID()
	if e = b.journal.AcknowledgeBuildCreated(ctx, effect, uid); e != nil {
		t.Fatal(e)
	}
	o := buildObservation(b, "publish", uid)
	if e = b.journal.RecordBuildTermination(ctx, c, o); e != nil {
		t.Fatal(e)
	}
	return o
}
func buildResult(b buildFixture, w, pub building.Termination) building.Result {
	p := b.plan()
	return building.Result{Version: "ai-sdlc.build-result/v1", BuildID: b.command.ID(), PlanSHA256: p.Digest(), ImageURL: building.Registry, ImageDigest: "sha256:" + strings.Repeat("4", 64), SourceSHA: b.command.SourceSHA, TreeSHA: b.command.TreeSHA, DefinitionSHA256: p.DefinitionSHA256, PublisherImage: p.PublisherImage, BuildRunUID: w.PipelineRunUID, PublishRunUID: pub.PipelineRunUID, BuildObservationSHA256: w.Digest(), PublishObservationSHA256: pub.Digest()}
}
func TestBuildPublicationOrderedImmutableHistoricalEvidence(t *testing.T) {
	b := newBuildFixture(t)
	ctx := context.Background()
	c := buildClaim(t, b)
	if _, e := b.journal.BeginPublishEffect(ctx, c); e == nil {
		t.Fatal("publish before writer")
	}
	w := buildWriterDone(t, b, c)
	pub := buildPublisherDone(t, b, c)
	if _, e := b.journal.BeginPublishEffect(ctx, c); e == nil {
		t.Fatal("publish replay")
	}
	if e := b.journal.RecordBuildTermination(ctx, c, pub); e == nil {
		t.Fatal("observation rewrite")
	}
	r := buildResult(b, w, pub)
	if e := b.journal.FinishBuild(ctx, c, r); e != nil {
		t.Fatal(e)
	}
	if e := b.journal.FinishBuild(ctx, c, r); e == nil {
		t.Fatal("completion replay")
	}
	b.cfg.Outer.Freeze = true
	fact, e := b.journal.ReadOwnBuiltRecord(ctx, c.ID)
	if e != nil || fact.Result != r {
		t.Fatal("historical record", e)
	}
	u, e := url.Parse(os.Getenv("AI_SDLC_TEST_DATABASE_URL"))
	if e != nil {
		t.Fatal(e)
	}
	u.User = url.UserPassword("aisdlc_verifier", b.passwords["verifier"])
	verifier, e := store.Open(ctx, u.String())
	if e != nil {
		t.Fatal(e)
	}
	defer verifier.Close()
	verified, e := verifier.ReadBuiltRecord(ctx, c.ID)
	if e != nil || verified.Result != r {
		t.Fatal("independent reader", e)
	}
	if _, e = verifier.Pool.Exec(`INSERT INTO built_records(request_id,digest,result) SELECT request_id,digest,result FROM built_records`); e == nil {
		t.Fatal("verifier wrote build record")
	}
	if _, e = b.f.db.ReadBuiltRecord(ctx, c.ID); e == nil {
		t.Fatal("worker read verifier surface")
	}
	cleanup, e := b.journal.ReadBuildCleanup(ctx, c)
	if e != nil || len(cleanup.Resources) != 2 || cleanup.UnknownCreate {
		t.Fatal("cleanup", e)
	}
	for _, q := range []string{`UPDATE built_records SET result=result`, `DELETE FROM build_observations`, `UPDATE build_plans SET plan=plan`} {
		if _, e = b.journal.Pool.Exec(q); e == nil {
			t.Fatal("rewrote immutable fact", q)
		}
	}
}
func TestBuildPublicationRejectsWriterSubstitutionAndIncompleteEvidence(t *testing.T) {
	for _, name := range []string{"unknown-create", "wrong-uid", "active-container", "restart", "missing-init", "wrong-node", "wrong-boot", "wrong-spec-digest", "wrong-request", "stale"} {
		t.Run(name, func(t *testing.T) {
			b := newBuildFixture(t)
			ctx := context.Background()
			c := buildClaim(t, b)
			effect, e := b.journal.BeginBuildEffect(ctx, c, b.plan())
			if e != nil {
				t.Fatal(e)
			}
			uid := store.NewID()
			if name != "unknown-create" {
				if e = b.journal.AcknowledgeBuildCreated(ctx, effect, uid); e != nil {
					t.Fatal(e)
				}
			}
			o := buildObservation(b, "build", uid)
			switch name {
			case "wrong-uid":
				o.PipelineRunUID = store.NewID()
			case "active-container":
				o.Containers[1].FinishedAt = 0
			case "restart":
				o.Containers[1].Restarts = 1
			case "missing-init":
				o.Containers = o.Containers[1:]
			case "wrong-node":
				o.NodeUID = store.NewID()
			case "wrong-boot":
				o.NodeBootID = store.NewID()
			case "wrong-spec-digest":
				o.PodSpecSHA256 = "not-a-digest"
			case "wrong-request":
				o.RequestSHA256 = strings.Repeat("9", 64)
			case "stale":
				o.ObservedAt = time.Now().Add(-time.Minute).UnixMilli()
			}
			if e = b.journal.RecordBuildTermination(ctx, c, o); e == nil {
				t.Fatal("bad writer accepted")
			}
			if _, e = b.journal.BeginPublishEffect(ctx, c); e == nil {
				t.Fatal("bad writer released publication")
			}
		})
	}
}
func TestBuildCleanupAfterRevocationNeverRenewsAuthority(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{true: "lost-create-reply", false: "known-uid"}[unknown], func(t *testing.T) {
			b := newBuildFixture(t)
			ctx := context.Background()
			c := buildClaim(t, b)
			effect, e := b.journal.BeginBuildEffect(ctx, c, b.plan())
			if e != nil {
				t.Fatal(e)
			}
			uid := store.NewID()
			if !unknown {
				if e = b.journal.AcknowledgeBuildCreated(ctx, effect, uid); e != nil {
					t.Fatal(e)
				}
			}
			b.cfg.Inner.Freeze = true
			b.cfg.Outer.Freeze = true
			if e = b.journal.CheckBuild(ctx, c); e == nil {
				t.Fatal("authority retained")
			}
			if e = b.journal.StopBuild(ctx, c, "revoked"); e != nil {
				t.Fatal(e)
			}
			next, e := b.journal.NextBuildRecovery(ctx)
			if e != nil || next.ID != c.ID || next.Token != c.Token {
				t.Fatal("recovery", e)
			}
			recovery, e := b.journal.ReadBuildCleanup(ctx, c)
			if e != nil || recovery.UnknownCreate != unknown {
				t.Fatal("cleanup denied", e)
			}
			if !unknown && (len(recovery.Resources) != 1 || recovery.Resources[0].UID != uid) {
				t.Fatal("original UID lost")
			}
			if unknown && len(recovery.Resources) != 0 {
				t.Fatal("adopted unknown resource")
			}
			if _, e = b.journal.BeginPublishEffect(ctx, c); e == nil {
				t.Fatal("cleanup granted publish")
			}
			other := c
			other.Token = store.NewID()
			if _, e = b.journal.ReadBuildCleanup(ctx, other); e == nil {
				t.Fatal("foreign recovery token")
			}
			if b.f.Count(t, `SELECT count(*) FROM build_slots`) != 1 {
				t.Fatal("cleanup freed physical slot")
			}
		})
	}
}
func TestBuildResultRejectsUnboundArtifactOrReleaseClaim(t *testing.T) {
	b := newBuildFixture(t)
	ctx := context.Background()
	c := buildClaim(t, b)
	w := buildWriterDone(t, b, c)
	pub := buildPublisherDone(t, b, c)
	good := buildResult(b, w, pub)
	for _, name := range []string{"source", "tree", "definition", "image", "publisher", "release", "build-uid", "publish-uid", "plan", "writer"} {
		r := good
		switch name {
		case "source":
			r.SourceSHA = strings.Repeat("a", 40)
		case "tree":
			r.TreeSHA = strings.Repeat("0", 40)
		case "definition":
			r.DefinitionSHA256 = strings.Repeat("a", 64)
		case "image":
			r.ImageURL = "attacker.invalid/saferus"
		case "publisher":
			r.PublisherImage = "untrusted:latest"
		case "release":
			r.ReleaseAuthorized = true
		case "build-uid":
			r.BuildRunUID = store.NewID()
		case "publish-uid":
			r.PublishRunUID = store.NewID()
		case "plan":
			r.PlanSHA256 = strings.Repeat("a", 64)
		case "writer":
			r.BuildObservationSHA256 = strings.Repeat("a", 64)
		}
		if e := b.journal.FinishBuild(ctx, c, r); e == nil {
			raw, _ := json.Marshal(r)
			t.Fatalf("unbound %s accepted: %s", name, raw)
		}
	}
	if e := b.journal.FinishBuild(ctx, c, good); e != nil {
		t.Fatal(e)
	}
}

func TestBuildLostPostgresAcknowledgementsNeverAuthorizeReplay(t *testing.T) {
	for _, phase := range []string{"claim", "build-intent", "build-uid", "publish-intent", "finish"} {
		t.Run(phase, func(t *testing.T) {
			b := newBuildFixture(t)
			ctx := context.Background()
			if e := b.journal.QueueBuild(ctx, b.raw, b.receipt); e != nil {
				t.Fatal(e)
			}
			var c store.BuildClaim
			var effect store.BuildEffect
			var result building.Result
			var e error
			if phase != "claim" {
				c, e = b.journal.ClaimBuild(ctx, b.command.ID(), b.cfg.Slots[0])
				if e != nil {
					t.Fatal(e)
				}
			}
			if phase == "build-uid" {
				effect, e = b.journal.BeginBuildEffect(ctx, c, b.plan())
				if e != nil {
					t.Fatal(e)
				}
			}
			if phase == "publish-intent" || phase == "finish" {
				w := buildWriterDone(t, b, c)
				if phase == "finish" {
					pub := buildPublisherDone(t, b, c)
					result = buildResult(b, w, pub)
				}
			}
			fault, dropped := executionCommitFault(t, 1, "build-controller")
			j := store.NewBuildJournal(fault, func() (building.Configuration, error) { return *b.cfg, nil })
			uid := store.NewID()
			switch phase {
			case "claim":
				returned, err := j.ClaimBuild(ctx, b.command.ID(), b.cfg.Slots[0])
				if err == nil || returned.Token != "" {
					t.Fatal("uncertain claim exposed capability")
				}
			case "build-intent":
				returned, err := j.BeginBuildEffect(ctx, c, b.plan())
				if err == nil || returned.Token != "" {
					t.Fatal("uncertain intention exposed capability")
				}
			case "build-uid":
				if j.AcknowledgeBuildCreated(ctx, effect, uid) == nil {
					t.Fatal("lost ACK reported success")
				}
			case "publish-intent":
				returned, err := j.BeginPublishEffect(ctx, c)
				if err == nil || returned.Token != "" {
					t.Fatal("uncertain publisher exposed capability")
				}
			case "finish":
				if j.FinishBuild(ctx, c, result) == nil {
					t.Fatal("lost finish reported success")
				}
			}
			select {
			case <-dropped:
			default:
				t.Fatal("actual COMMIT acknowledgement was not dropped")
			}
			if phase == "finish" {
				fact, e := b.journal.ReadOwnBuiltRecord(ctx, c.ID)
				if e != nil || fact.Result != result {
					t.Fatal("durable result lost", e)
				}
				if b.journal.FinishBuild(ctx, c, result) == nil {
					t.Fatal("repeated finish")
				}
				recovery, e := b.journal.NextBuildRecovery(ctx)
				if e != nil || recovery.ID != c.ID {
					t.Fatal("lost finish ACK lost restart cleanup", e)
				}
				return
			}
			if phase == "claim" {
				c, e = b.journal.NextBuildRecovery(ctx)
				if e != nil {
					t.Fatal(e)
				}
			} else if e = b.journal.StopBuild(ctx, c, "uncertain"); e != nil {
				t.Fatal(e)
			}
			recovery, e := b.journal.ReadBuildCleanup(ctx, c)
			if e != nil {
				t.Fatal(e)
			}
			if phase == "build-uid" && (len(recovery.Resources) != 1 || recovery.Resources[0].UID != uid) {
				t.Fatal("lost ACK lost original resource UID")
			}
			if (phase == "build-intent" || phase == "publish-intent") && !recovery.UnknownCreate {
				t.Fatal("uncertain create lost")
			}
			if _, e = b.journal.ClaimBuild(ctx, b.command.ID(), b.cfg.Slots[0]); e == nil {
				t.Fatal("reclaimed uncertain execution")
			}
			if _, e = b.journal.BeginPublishEffect(ctx, c); e == nil {
				t.Fatal("uncertain execution granted publication")
			}
		})
	}
}

func cleanupProof(c store.BuildClaim, r store.BuildCleanup) store.BuildCleanupProof {
	return store.BuildCleanupProof{Version: "ai-sdlc.build-cleanup/v1", BuildID: c.ID, SlotDigest: c.SlotDigest, Resources: r.Resources, UnknownCreate: r.UnknownCreate, ObservedAbsentAt: time.Now().UnixMilli()}
}
func TestBuildRecoveryTerminatesLiveClaimBeforeReturningIdentity(t *testing.T) {
	b := newBuildFixture(t)
	ctx := context.Background()
	c := buildClaim(t, b)
	if _, e := b.journal.ReadBuildCleanup(ctx, c); e == nil {
		t.Fatal("active claim exposed cleanup")
	}
	recovery, e := b.journal.NextBuildRecovery(ctx)
	if e != nil || recovery.ID != c.ID {
		t.Fatal(e)
	}
	if e = b.journal.CheckBuild(ctx, recovery); e == nil {
		t.Fatal("recovery retained live authority")
	}
	if _, e = b.journal.BeginBuildEffect(ctx, recovery, b.plan()); e == nil {
		t.Fatal("recovery adopted execution")
	}
	r, e := b.journal.ReadBuildCleanup(ctx, recovery)
	if e != nil {
		t.Fatal(e)
	}
	if e = b.journal.RecordBuildCleanup(ctx, recovery, cleanupProof(recovery, r)); e != nil {
		t.Fatal(e)
	}
	next, e := b.journal.NextBuildRecovery(ctx)
	if e != nil || next.ID != "" {
		t.Fatal("cleaned request blocks recovery", e)
	}
	if b.f.Count(t, `SELECT count(*) FROM build_slots`) != 1 {
		t.Fatal("recovery freed slot")
	}
}
func TestBuiltRecordRestartDiscoveryAndCleanupCommitLoss(t *testing.T) {
	b := newBuildFixture(t)
	ctx := context.Background()
	c := buildClaim(t, b)
	w := buildWriterDone(t, b, c)
	p := buildPublisherDone(t, b, c)
	if e := b.journal.FinishBuild(ctx, c, buildResult(b, w, p)); e != nil {
		t.Fatal(e)
	}
	next, e := b.journal.NextBuildRecovery(ctx)
	if e != nil || next.ID != c.ID {
		t.Fatal("completed build lost after restart", e)
	}
	r, e := b.journal.ReadBuildCleanup(ctx, next)
	if e != nil || len(r.Resources) != 2 {
		t.Fatal(e)
	}
	proof := cleanupProof(next, r)
	bad := proof
	bad.Resources = append([]store.CleanupResource{}, proof.Resources...)
	bad.Resources[0].UID = store.NewID()
	if b.journal.RecordBuildCleanup(ctx, next, bad) == nil {
		t.Fatal("cleanup adopted replacement UID")
	}
	fault, dropped := executionCommitFault(t, 1, "build-controller")
	j := store.NewBuildJournal(fault, func() (building.Configuration, error) { return *b.cfg, nil })
	if j.RecordBuildCleanup(ctx, next, proof) == nil {
		t.Fatal("lost cleanup ACK reported success")
	}
	select {
	case <-dropped:
	default:
		t.Fatal("cleanup COMMIT not dropped")
	}
	next, e = b.journal.NextBuildRecovery(ctx)
	if e != nil || next.ID != "" {
		t.Fatal("durable cleanup was replayed", e)
	}
	if b.f.Count(t, `SELECT count(*) FROM built_records`) != 1 || b.f.Count(t, `SELECT count(*) FROM build_slots`) != 1 {
		t.Fatal("cleanup erased provenance or freed slot")
	}
}
func TestBuildHistoricalRecordRejectsExtendedSnapshotDeadline(t *testing.T) {
	b := newBuildFixture(t)
	ctx := context.Background()
	c := buildClaim(t, b)
	w := buildWriterDone(t, b, c)
	p := buildPublisherDone(t, b, c)
	if e := b.journal.FinishBuild(ctx, c, buildResult(b, w, p)); e != nil {
		t.Fatal(e)
	}
	if _, e := b.f.Admin.Pool.Exec(`UPDATE build_requests SET expires_at=expires_at+interval '1 hour' WHERE id=$1`, c.ID); e != nil {
		t.Fatal(e)
	}
	if _, e := b.journal.ReadOwnBuiltRecord(ctx, c.ID); e == nil {
		t.Fatal("mutable expiry overrode signed deadline")
	}
	if _, e := b.f.Admin.Pool.Exec(`UPDATE build_requests SET expires_at=$2 WHERE id=$1`, c.ID, time.UnixMilli(b.command.ExpiresAt)); e != nil {
		t.Fatal(e)
	}
	if _, e := b.f.Admin.Pool.Exec(`UPDATE built_records SET recorded_at=$2 WHERE request_id=$1`, c.ID, time.UnixMilli(b.command.IssuedAt).Add(-time.Millisecond)); e != nil {
		t.Fatal(e)
	}
	if _, e := b.journal.ReadOwnBuiltRecord(ctx, c.ID); e == nil {
		t.Fatal("result predates signed authority")
	}
}
