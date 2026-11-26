package execution

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/publication"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/publicationrecovery"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/verification"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

// reconcileForge is a fake forge. Observe is its only read; its write methods
// mirror the publication transport and count any call, which must stay zero.
type reconcileForge struct {
	observation publication.Observation
	err         error
	seen        []publication.Expectation
	writes      atomic.Int32
}

func (f *reconcileForge) Observe(_ context.Context, x publication.Expectation) (publication.Observation, error) {
	f.seen = append(f.seen, x)
	return f.observation, f.err
}
func (f *reconcileForge) CreateTree(context.Context, publication.WriteFence, publication.TreePlan) (string, error) {
	f.writes.Add(1)
	return "", errors.New("write")
}
func (f *reconcileForge) CreateCommit(context.Context, publication.WriteFence, publication.TreePlan, string, time.Time) (string, error) {
	f.writes.Add(1)
	return "", errors.New("write")
}
func (f *reconcileForge) CreateBranch(context.Context, publication.WriteFence, publication.TreePlan, string) error {
	f.writes.Add(1)
	return errors.New("write")
}
func (f *reconcileForge) CreatePullRequest(context.Context, publication.WriteFence, publication.TreePlan, string, string, string) (publication.PublishedPullRequest, error) {
	f.writes.Add(1)
	return publication.PublishedPullRequest{}, errors.New("write")
}

type uncertainPublication struct {
	f       *lifecycleFixture
	db      *store.DB
	request store.PublicationRequest
	command publication.Command
	plan    publication.TreePlan
	claim   store.PublicationClaim
	planned publication.Expectation
}

// newUncertainPublication journals every effect before kind exactly as the
// controller would, begins kind, and stops the request uncertain: the
// battery-31 shape when kind is "branch".
func newUncertainPublication(t *testing.T, kind string) uncertainPublication {
	t.Helper()
	return newPlannedUncertainPublication(t, kind, nil)
}

// newPlannedUncertainPublication journals with the plan that build returns
// (nil: a synthetic plan), optionally configuring the fixture first.
func newPlannedUncertainPublication(t *testing.T, kind string, build func(*testing.T, *store.DB, publication.Command) publication.TreePlan, configure ...func(*fixture)) uncertainPublication {
	t.Helper()
	f, db, r := publicationJournalFixture(t, configure...)
	ctx := context.Background()
	var cmd publication.Command
	if authority.DecodeStrict(r.Payload, &cmd) != nil {
		t.Fatal("publication command")
	}
	plan := publication.TreePlan{Repository: cmd.Repository, Request: publication.TreeRequest{BaseTree: strings.Repeat("a", 40), Tree: []publication.TreeEntry{{Path: verification.LocaleSource, Mode: "100644", Type: "blob", Content: "source"}, {Path: verification.LocaleNewTest, Mode: "100644", Type: "blob", Content: "test"}}},
		ExpectedTreeSHA: strings.Repeat("b", 40), Branch: publication.BranchName(cmd), CommitMessage: publication.CommitMessage(cmd.IssueNumber)}
	if build != nil {
		// The candidate projection is part of the installed publication grants.
		passwords := map[string]string{}
		for role := range f.Material.Identities {
			passwords[role] = strings.ReplaceAll(role, "-", "_") + strings.Repeat("q", 40)
		}
		if e := f.Admin.ProvisionRolesForCapabilities(ctx, passwords, "verification", true, true); e != nil {
			t.Fatal(e)
		}
		plan = build(t, db, cmd)
	}
	claim, e := db.ClaimPublication(ctx, r.ID, publication.PlanDigest(cmd, plan))
	if e != nil {
		t.Fatal(e)
	}
	u := uncertainPublication{f: f, db: db, request: r, command: cmd, plan: plan, claim: claim}
	tree, _ := publication.ExpectTree(plan, cmd.SourceSHA)
	commit, _ := publication.ExpectCommit(cmd.Repository, cmd.IssueNumber, plan.ExpectedTreeSHA, cmd.SourceSHA, claim.CommitTime)
	branch, _ := publication.ExpectBranch(cmd.Repository, plan.Branch, commit.SHA, cmd.SourceSHA)
	pr, _ := publication.ExpectPullRequest(cmd.Repository, cmd.IssueNumber, plan.Branch, commit.SHA, claim.PlanDigest, cmd.SourceSHA)
	for _, x := range []publication.Expectation{tree, commit, branch, pr} {
		if x.RequestDigest == "" {
			t.Fatal("planned request", x.Kind)
		}
		effect, e := db.BeginPublicationEffect(ctx, claim, x.Kind, x.RequestDigest)
		if e != nil {
			t.Fatal(e)
		}
		if x.Kind == kind {
			u.planned = x
			break
		}
		receipt := store.PublicationReceipt{Version: "ai-sdlc.publication-receipt/v1", Kind: x.Kind, SHA: x.SHA}
		if x.Kind == "branch" {
			receipt.Branch = x.Branch
		}
		if db.AcknowledgePublicationEffect(ctx, effect, receipt) != nil {
			t.Fatal("receipt", x.Kind)
		}
	}
	if db.StopPublication(ctx, claim, "uncertain") != nil {
		t.Fatal("stop uncertain")
	}
	if blocked, e := db.PublicationBlocked(ctx); e != nil || !blocked {
		t.Fatal("uncertain effect did not quarantine")
	}
	return u
}

// settle moves the original claim's end into the past, as time would.
func (u uncertainPublication) settle(t *testing.T) {
	t.Helper()
	if _, e := u.f.Admin.Pool.Exec(`UPDATE publication_requests SET claim_until=clock_timestamp()-interval '3 minutes' WHERE id=$1`, u.request.ID); e != nil {
		t.Fatal(e)
	}
}

func (u uncertainPublication) reconciler() *publicationrecovery.Reconciler {
	return &publicationrecovery.Reconciler{DB: u.db, Log: func(string, ...any) {},
		Plan: func(context.Context, publication.Command) (publication.TreePlan, error) { return u.plan, nil }}
}

type effectRow struct {
	completed, notSent bool
	receipt            store.PublicationReceipt
}

func (u uncertainPublication) effect(t *testing.T, kind string) effectRow {
	t.Helper()
	var row effectRow
	var raw []byte
	if e := u.f.Admin.Pool.QueryRow(`SELECT completed_at IS NOT NULL,not_sent_at IS NOT NULL,receipt FROM publication_effects WHERE request_id=$1 AND kind=$2`, u.request.ID, kind).Scan(&row.completed, &row.notSent, &raw); e != nil {
		t.Fatal(e)
	}
	if raw != nil && authority.DecodeStrict(raw, &row.receipt) != nil {
		t.Fatal("receipt shape")
	}
	return row
}

func (u uncertainPublication) state(t *testing.T) (string, string) {
	t.Helper()
	var state, reason string
	if e := u.f.Admin.Pool.QueryRow(`SELECT state,reason FROM publication_requests WHERE id=$1`, u.request.ID).Scan(&state, &reason); e != nil {
		t.Fatal(e)
	}
	return state, reason
}

func testReconcileEffect(t *testing.T, kind string) {
	cases := []struct {
		name        string
		observation publication.Observation
		err         error
		outcome     string
		lifted      bool
	}{
		{"exists-exactly", publication.Observation{State: publication.ObservedExact, PullRequest: 1600}, nil, publicationrecovery.Completed, true},
		{"absent", publication.Observation{State: publication.ObservedAbsent}, nil, publicationrecovery.NotSent, true},
		{"exists-differently", publication.Observation{State: publication.ObservedDifferent}, nil, publicationrecovery.Mismatch, false},
		{"observation-error", publication.Observation{}, errors.New("forge 502"), publicationrecovery.Unavailable, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u := newUncertainPublication(t, kind)
			u.settle(t)
			ctx := context.Background()
			forge := &reconcileForge{observation: c.observation, err: c.err}
			var lines []string
			r := u.reconciler()
			r.Log = func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
			step, attempted, e := r.Step(ctx, forge)
			if e != nil || !attempted || step.Kind != kind || step.Outcome != c.outcome {
				t.Fatal("reconciliation step", step, attempted, e)
			}
			if len(forge.seen) != 1 || forge.seen[0] != u.planned || forge.writes.Load() != 0 {
				t.Fatal("observation was not exactly the planned, read-only one")
			}
			if len(lines) != 1 || lines[0] != "publication reconciliation: effect="+kind+" outcome="+c.outcome {
				t.Fatal("bounded log line", lines)
			}
			blocked, e := u.db.PublicationBlocked(ctx)
			if e != nil || blocked == c.lifted {
				t.Fatal("quarantine after reconciliation", blocked, e)
			}
			row := u.effect(t, kind)
			switch c.outcome {
			case publicationrecovery.Completed:
				want := store.PublicationReceipt{Version: "ai-sdlc.publication-receipt/v1", Kind: kind, SHA: u.planned.SHA, Observed: true}
				if kind == "branch" || kind == "pull-request" {
					want.Branch = u.plan.Branch
				}
				if kind == "pull-request" {
					want.PullRequest = 1600
				}
				if !row.completed || row.notSent || row.receipt != want {
					t.Fatal("observed receipt", row)
				}
			case publicationrecovery.NotSent:
				if row.completed || !row.notSent {
					t.Fatal("known no-send", row)
				}
			default:
				if row.completed || row.notSent {
					t.Fatal("quarantined effect changed", row)
				}
			}
			// The original request is closed, never resumed: it stays failed
			// and cannot be claimed again.
			if state, reason := u.state(t); state != "failed" || reason != "uncertain" {
				t.Fatal("request state", state, reason)
			}
			if _, e := u.db.ClaimPublication(ctx, u.request.ID, u.claim.PlanDigest); e == nil {
				t.Fatal("reconciled publication resumed")
			}
			if id, e := u.db.NextPublication(ctx); e != nil || id != "" {
				t.Fatal("reconciled publication requeued")
			}
		})
	}
}

func TestPublicationReconciliationTree(t *testing.T)   { testReconcileEffect(t, "tree") }
func TestPublicationReconciliationCommit(t *testing.T) { testReconcileEffect(t, "commit") }
func TestPublicationReconciliationBranch(t *testing.T) { testReconcileEffect(t, "branch") }
func TestPublicationReconciliationPullRequest(t *testing.T) {
	testReconcileEffect(t, "pull-request")
}

// Nothing is observed until the original attempt can no longer be in flight;
// a quarantined effect is observed again only after backoff, and the
// quarantine lifts once a later observation resolves it. The installed column
// privileges suffice.
func TestPublicationReconciliationSettlesPacesAndLiftsQuarantine(t *testing.T) {
	u := newUncertainPublication(t, "branch")
	ctx := context.Background()
	passwords := map[string]string{}
	for role := range u.f.Material.Identities {
		passwords[role] = strings.ReplaceAll(role, "-", "_") + strings.Repeat("q", 40)
	}
	if e := u.f.Admin.ProvisionRolesForCapabilities(ctx, passwords, "verification", true, true); e != nil {
		t.Fatal(e)
	}
	now := time.Now()
	r := u.reconciler()
	r.Now = func() time.Time { return now }
	forge := &reconcileForge{err: errors.New("forge 503")}
	if pending, e := u.db.PublicationReconciliations(ctx, 16); e != nil || len(pending) != 0 {
		t.Fatal("unsettled effect offered for reconciliation", len(pending), e)
	}
	if _, attempted, e := r.Step(ctx, forge); e != nil || attempted || len(forge.seen) != 0 {
		t.Fatal("observed while the original attempt could be in flight")
	}
	u.settle(t)
	if step, attempted, e := r.Step(ctx, forge); e != nil || !attempted || step.Outcome != publicationrecovery.Unavailable {
		t.Fatal("first observation", step, e)
	}
	if _, attempted, _ := r.Step(ctx, forge); attempted {
		t.Fatal("observation not paced")
	}
	now = now.Add(31 * time.Second)
	forge.err, forge.observation = nil, publication.Observation{State: publication.ObservedAbsent}
	if step, attempted, e := r.Step(ctx, forge); e != nil || !attempted || step.Outcome != publicationrecovery.NotSent {
		t.Fatal("paced observation", step, e)
	}
	if blocked, e := u.db.PublicationBlocked(ctx); e != nil || blocked {
		t.Fatal("quarantine did not lift")
	}
	if _, attempted, e := r.Step(ctx, forge); e != nil || attempted {
		t.Fatal("resolved effect observed again")
	}
	if len(forge.seen) != 2 || forge.writes.Load() != 0 {
		t.Fatal("forge traffic")
	}
}

// The journal accepts only an observed, consistent outcome for the same
// uncertain intent, once.
func TestPublicationReconciliationJournalRejectsUnprovenOutcomes(t *testing.T) {
	u := newUncertainPublication(t, "pull-request")
	ctx := context.Background()
	if e := u.db.ResolvePublicationEffect(ctx, store.PublicationReconciliation{}, nil); e == nil {
		t.Fatal("empty reconciliation resolved")
	}
	u.settle(t)
	pending, e := u.db.PublicationReconciliations(ctx, 16)
	if e != nil || len(pending) != 1 || pending[0].Kind != "pull-request" || len(pending[0].Completed) != 3 || pending[0].RequestDigest != u.planned.RequestDigest {
		t.Fatal("pending reconciliation", e)
	}
	p := pending[0]
	good := store.PublicationReceipt{Version: "ai-sdlc.publication-receipt/v1", Kind: "pull-request", SHA: u.planned.SHA, Branch: u.plan.Branch, PullRequest: 1600, Observed: true}
	for name, mutate := range map[string]func(*store.PublicationReconciliation, *store.PublicationReceipt){
		"unobserved":   func(_ *store.PublicationReconciliation, r *store.PublicationReceipt) { r.Observed = false },
		"other-commit": func(_ *store.PublicationReconciliation, r *store.PublicationReceipt) { r.SHA = strings.Repeat("7", 40) },
		"other-branch": func(_ *store.PublicationReconciliation, r *store.PublicationReceipt) {
			r.Branch = "ai-sdlc/issue-9-" + strings.Repeat("c", 32)
		},
		"other-kind": func(_ *store.PublicationReconciliation, r *store.PublicationReceipt) {
			r.Kind = "branch"
			r.PullRequest = 0
		},
		"other-digest": func(c *store.PublicationReconciliation, _ *store.PublicationReceipt) {
			c.RequestDigest = strings.Repeat("0", 64)
		},
		"other-intent": func(c *store.PublicationReconciliation, _ *store.PublicationReceipt) { c.EffectToken = store.NewID() },
		"other-plan": func(c *store.PublicationReconciliation, _ *store.PublicationReceipt) {
			c.PlanDigest = strings.Repeat("0", 64)
		},
		"other-time": func(c *store.PublicationReconciliation, _ *store.PublicationReceipt) {
			c.CommitTime = c.CommitTime.Add(time.Second)
		},
		"completed-kind": func(c *store.PublicationReconciliation, _ *store.PublicationReceipt) { c.Kind = "branch" },
	} {
		c, receipt := p, good
		mutate(&c, &receipt)
		if u.db.ResolvePublicationEffect(ctx, c, &receipt) == nil {
			t.Fatal("unproven outcome recorded", name)
		}
	}
	// A live write can never present an observed receipt.
	effect := store.PublicationEffect{Claim: u.claim, Kind: "pull-request", Token: p.EffectToken, RequestDigest: p.RequestDigest}
	if u.db.AcknowledgePublicationEffect(ctx, effect, good) == nil {
		t.Fatal("observed receipt acknowledged as a write response")
	}
	if u.db.ResolvePublicationEffect(ctx, p, &good) != nil {
		t.Fatal("observed outcome")
	}
	if u.db.ResolvePublicationEffect(ctx, p, nil) == nil || u.db.ResolvePublicationEffect(ctx, p, &good) == nil {
		t.Fatal("resolved effect changed")
	}
	if blocked, e := u.db.PublicationBlocked(ctx); e != nil || blocked {
		t.Fatal("quarantine")
	}
	// The generator identity cannot reconcile publication.
	if _, e := u.f.db.PublicationReconciliations(ctx, 16); e == nil {
		t.Fatal("foreign role listed reconciliations")
	}
}

// A journal whose facts do not re-derive the journaled request stays
// quarantined, and so does a tree whose plan cannot be rebuilt.
func TestPublicationReconciliationUnplannedStaysQuarantined(t *testing.T) {
	ctx := context.Background()
	u := newUncertainPublication(t, "tree")
	u.settle(t)
	forge := &reconcileForge{observation: publication.Observation{State: publication.ObservedAbsent}}
	r := u.reconciler()
	other := u.plan
	other.ExpectedTreeSHA = strings.Repeat("c", 40)
	r.Plan = func(context.Context, publication.Command) (publication.TreePlan, error) { return other, nil }
	if step, _, e := r.Step(ctx, forge); e != nil || step.Outcome != publicationrecovery.Unplanned || len(forge.seen) != 0 {
		t.Fatal("unplanned tree observed", step, e)
	}
	r = u.reconciler()
	r.Plan = nil
	r.Fetch = func(context.Context, string, string) (workspacebundle.Bundle, error) {
		return workspacebundle.Bundle{}, errors.New("source unavailable")
	}
	if step, _, e := r.Step(ctx, forge); e != nil || step.Outcome != publicationrecovery.Unavailable || len(forge.seen) != 0 {
		t.Fatal("tree without source", step, e)
	}
	if _, e := u.f.Admin.Pool.Exec(`UPDATE publication_effects SET request_digest=repeat('0',64) WHERE request_id=$1 AND kind='tree'`, u.request.ID); e != nil {
		t.Fatal(e)
	}
	r = u.reconciler()
	if step, _, e := r.Step(ctx, forge); e != nil || step.Outcome != publicationrecovery.Unplanned || len(forge.seen) != 0 {
		t.Fatal("foreign request digest observed", step, e)
	}
	if blocked, e := u.db.PublicationBlocked(ctx); e != nil || !blocked {
		t.Fatal("unplanned effect lifted quarantine")
	}
	if row := u.effect(t, "tree"); row.completed || row.notSent {
		t.Fatal("unplanned effect changed")
	}
}

// One effect whose journaled predecessors no longer verify is reported as
// unplanned and stays quarantined; it never turns the listing into an error
// that would stop reconciliation of every other effect.
func TestPublicationReconciliationInconsistentEffectDoesNotBlockListing(t *testing.T) {
	u := newUncertainPublication(t, "branch")
	u.settle(t)
	ctx := context.Background()
	if _, e := u.f.Admin.Pool.Exec(`UPDATE publication_effects SET receipt_digest=repeat('0',64) WHERE request_id=$1 AND kind='commit'`, u.request.ID); e != nil {
		t.Fatal(e)
	}
	pending, e := u.db.PublicationReconciliations(ctx, 16)
	if e != nil || len(pending) != 1 || pending[0].Consistent || pending[0].Completed != nil {
		t.Fatal("inconsistent effect listing", len(pending), e)
	}
	forge := &reconcileForge{observation: publication.Observation{State: publication.ObservedAbsent}}
	r := u.reconciler()
	step, attempted, e := r.Step(ctx, forge)
	if e != nil || !attempted || step.Outcome != publicationrecovery.Unplanned || len(forge.seen) != 0 {
		t.Fatal("inconsistent effect", step, attempted, e)
	}
	if u.db.ResolvePublicationEffect(ctx, pending[0], nil) == nil {
		t.Fatal("inconsistent effect resolved")
	}
	if blocked, e := u.db.PublicationBlocked(ctx); e != nil || !blocked {
		t.Fatal("inconsistent effect lifted quarantine")
	}
	// Once its journal verifies again it is reconciled like any other.
	var raw []byte
	if e := u.f.Admin.Pool.QueryRow(`SELECT receipt FROM publication_effects WHERE request_id=$1 AND kind='commit'`, u.request.ID).Scan(&raw); e != nil {
		t.Fatal(e)
	}
	if _, e := u.f.Admin.Pool.Exec(`UPDATE publication_effects SET receipt_digest=$2 WHERE request_id=$1 AND kind='commit'`, u.request.ID, authority.Digest(raw)); e != nil {
		t.Fatal(e)
	}
	if step, _, e := u.reconciler().Step(ctx, forge); e != nil || step.Outcome != publicationrecovery.NotSent {
		t.Fatal("restored effect", step, e)
	}
}

// The default tree plan: the immutable candidate delta read from the journal,
// applied to the fetched pinned source and rebuilt by PrepareTree, must match
// the plan digest the claim journaled, exactly as the controller computes it.
func TestPublicationReconciliationRebuildsTreePlanFromCandidate(t *testing.T) {
	source := workspacebundle.Bundle{Version: workspacebundle.Version, Files: []workspacebundle.File{
		{Path: "README.md", Mode: 0644, Data: []byte("fixture source\n")},
		{Path: verification.LocaleSource, Mode: 0644, Data: []byte("old source\n")},
		{Path: verification.LocaleExistingTest, Mode: 0644, Data: []byte("existing test\n")},
	}}
	configure := func(f *fixture) {
		source.SourceSHA = f.command.SourceSHA
		f.sourceOverride = &source
		f.candidateChanges = map[string]string{verification.LocaleSource: "new source\n", verification.LocaleNewTest: "new test\n"}
	}
	controllerPlan := func(t *testing.T, db *store.DB, cmd publication.Command) publication.TreePlan {
		candidate, e := db.ReadPublicationCandidate(context.Background(), cmd.CandidateID)
		if e != nil {
			t.Fatal(e)
		}
		plan, e := publication.PrepareTree(cmd, source, candidate.Delta)
		if e != nil {
			t.Fatal("controller plan", e)
		}
		return plan
	}
	for _, c := range []struct {
		state, outcome string
	}{{publication.ObservedExact, publicationrecovery.Completed}, {publication.ObservedAbsent, publicationrecovery.NotSent}} {
		t.Run(c.outcome, func(t *testing.T) {
			u := newPlannedUncertainPublication(t, "tree", controllerPlan, configure)
			u.settle(t)
			ctx := context.Background()
			fetches := 0
			r := &publicationrecovery.Reconciler{DB: u.db, Log: func(string, ...any) {},
				Fetch: func(_ context.Context, repo, sha string) (workspacebundle.Bundle, error) {
					fetches++
					if repo != u.command.Repository || sha != u.command.SourceSHA {
						return workspacebundle.Bundle{}, errors.New("unplanned source")
					}
					return source, nil
				}}
			forge := &reconcileForge{observation: publication.Observation{State: c.state}}
			step, attempted, e := r.Step(ctx, forge)
			if e != nil || !attempted || step.Outcome != c.outcome || fetches != 1 {
				t.Fatal("default tree plan", step, e, fetches)
			}
			if len(forge.seen) != 1 || forge.seen[0].SHA != u.plan.ExpectedTreeSHA || forge.seen[0].RequestDigest != u.planned.RequestDigest || forge.writes.Load() != 0 {
				t.Fatal("observed tree differs from the controller plan")
			}
			if blocked, e := u.db.PublicationBlocked(ctx); e != nil || blocked {
				t.Fatal("quarantine")
			}
		})
	}
}

// A branch receipt must name the branch the signed command plans, even when
// it points at the acknowledged commit.
func TestPublicationReconciliationRejectsBranchReceiptWithWrongName(t *testing.T) {
	u := newUncertainPublication(t, "branch")
	u.settle(t)
	ctx := context.Background()
	pending, e := u.db.PublicationReconciliations(ctx, 16)
	if e != nil || len(pending) != 1 || !pending[0].Consistent {
		t.Fatal("pending branch", e)
	}
	wrong := store.PublicationReceipt{Version: "ai-sdlc.publication-receipt/v1", Kind: "branch", SHA: u.planned.SHA, Branch: "ai-sdlc/issue-9-" + strings.Repeat("c", 32), Observed: true}
	if u.db.ResolvePublicationEffect(ctx, pending[0], &wrong) == nil {
		t.Fatal("branch receipt with another name recorded")
	}
	if row := u.effect(t, "branch"); row.completed || row.notSent {
		t.Fatal("rejected receipt changed the effect")
	}
	right := wrong
	right.Branch = u.plan.Branch
	if u.db.ResolvePublicationEffect(ctx, pending[0], &right) != nil {
		t.Fatal("planned branch receipt")
	}
}

// An inconsistent effect listed before a consistent one does not stop the
// consistent one from being reconciled. The fixture builds one real request;
// the second, older one is inserted with foreign keys disabled and has an
// uncertain branch effect without its predecessors.
func TestPublicationReconciliationInconsistentRowDoesNotHideConsistentRow(t *testing.T) {
	u := newUncertainPublication(t, "branch")
	u.settle(t)
	ctx := context.Background()
	other := "publication-" + strings.Repeat("e", 64)
	tx, e := u.f.Admin.Pool.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback()
	for _, q := range []string{
		`SET LOCAL session_replication_role=replica`,
		`INSERT INTO publication_requests(id,review_id,verification_id,candidate_id,repository,payload_digest,payload,receipt,expires_at,state,claim_token,claim_until,plan_digest,commit_time,reason,created_at)
 SELECT '` + other + `','review-other','verification-other','execution-other',repository,payload_digest,payload,receipt,expires_at,'failed',gen_random_uuid(),claim_until,plan_digest,commit_time,'uncertain',created_at-interval '1 minute' FROM publication_requests WHERE id='` + u.request.ID + `'`,
		`INSERT INTO publication_effects(request_id,kind,token,request_digest,created_at) VALUES('` + other + `','branch',gen_random_uuid(),repeat('1',64),clock_timestamp()-interval '1 hour')`,
	} {
		if _, e = tx.Exec(q); e != nil {
			t.Fatal(e)
		}
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	pending, e := u.db.PublicationReconciliations(ctx, 16)
	if e != nil || len(pending) != 2 || pending[0].RequestID != other || pending[0].Consistent || pending[1].RequestID != u.request.ID || !pending[1].Consistent {
		t.Fatal("listing", len(pending), e)
	}
	forge := &reconcileForge{observation: publication.Observation{State: publication.ObservedAbsent}}
	r := u.reconciler()
	if step, _, e := r.Step(ctx, forge); e != nil || step.Outcome != publicationrecovery.Unplanned || len(forge.seen) != 0 {
		t.Fatal("inconsistent row", step, e)
	}
	// The inconsistent row now paces; the next poll reconciles the other.
	if step, _, e := r.Step(ctx, forge); e != nil || step.Outcome != publicationrecovery.NotSent || len(forge.seen) != 1 {
		t.Fatal("consistent row", step, e)
	}
	if row := u.effect(t, "branch"); !row.notSent {
		t.Fatal("consistent row not reconciled")
	}
	if blocked, e := u.db.PublicationBlocked(ctx); e != nil || !blocked {
		t.Fatal("inconsistent row must keep the quarantine")
	}
}
