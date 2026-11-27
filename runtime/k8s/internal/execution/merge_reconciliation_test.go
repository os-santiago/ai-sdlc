package execution_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/execution"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/intake"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/mergeflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/mergegate"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/mergerecovery"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/mergewriter"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/publicationflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

// mergeForge is a fake read-only forge. Observe is its only read; Merge
// mirrors the transport and counts any call, which must stay zero while
// reconciling.
type mergeForge struct {
	observation mergewriter.Observation
	err         error
	seen        []mergewriter.Plan
	writes      atomic.Int32
	// during, when set, runs while the forge is being read.
	during func()
}

func (f *mergeForge) Observe(_ context.Context, p mergewriter.Plan) (mergewriter.Observation, error) {
	f.seen = append(f.seen, p)
	if f.during != nil {
		f.during()
	}
	return f.observation, f.err
}
func (f *mergeForge) Merge(context.Context, mergegate.Expected, int, mergewriter.Fence) (mergewriter.Receipt, error) {
	f.writes.Add(1)
	return mergewriter.Receipt{}, errors.New("write")
}

type uncertainMerge struct {
	f       execution.PublishedMergeTestFixture
	runtime mergeflow.Runtime
	journal *store.MergeJournal
	id      string
	planned mergewriter.Plan
}

// newUncertainMerge runs the merge controller against a transport that sends
// (journaling the planned request digest) and then loses the response: the
// battery-39 shape. digest, when set, replaces the planned digest the fence
// journals.
func newUncertainMerge(t *testing.T, digest string) uncertainMerge {
	t.Helper()
	f := execution.NewPublishedMergeTestFixture(t)
	ctx := context.Background()
	u := uncertainMerge{f: f, journal: store.NewMergeJournal(f.DB, func() (authority.Policy, error) { return f.Policy, nil })}
	r := mergeflow.Runtime{DB: f.DB, Policy: func() (authority.Policy, intake.Config, string, error) { return f.Policy, f.Intake, f.Profile, nil }, Review: func() (publicationflow.ReviewPolicy, error) {
		return publicationflow.ReviewPolicy{Profile: f.ReviewProfile, Configuration: f.ReviewConfiguration}, nil
	}, Identity: func() (authority.Identity, error) { return f.Identity, nil }, GatePolicy: func() (mergegate.Policy, error) { return mergegate.PilotPolicy(), nil }}
	r.Gates = mergeGatesFunc(func(_ context.Context, e mergegate.Expected, _ mergegate.Policy) (mergegate.Observation, error) {
		return mergegate.Observation{Expected: e, PolicySHA256: strings.Repeat("a", 64), ChecksSHA256: strings.Repeat("b", 64), ReviewsSHA256: strings.Repeat("c", 64), Ready: true, Reasons: []string{}, ObservedAt: time.Now()}, nil
	})
	r.Writer = mergeWriterFunc(func(ctx context.Context, e mergegate.Expected, issue int, fence mergewriter.Fence) (mergewriter.Receipt, error) {
		p, err := mergewriter.PlanMerge(e, issue)
		if err != nil {
			t.Fatal(err)
		}
		u.planned = p
		sent := p.RequestDigest
		if digest != "" {
			sent = digest
		}
		if fence(ctx, sent) != nil {
			return mergewriter.Receipt{}, errors.New("presend denied")
		}
		// GitHub merged the pull request, but the response arrived after the deadline.
		return mergewriter.Receipt{}, errors.New("lost response")
	})
	if state, e := r.PollOnce(ctx); e == nil || state != "merge-stopped" {
		t.Fatal("lost merge response not stopped", state, e)
	}
	if blocked, e := u.journal.MergeBlocked(ctx); e != nil || !blocked {
		t.Fatal("uncertain merge did not quarantine", blocked, e)
	}
	if f.Admin.Pool.QueryRow(`SELECT id FROM merge_requests WHERE state='failed' AND reason='uncertain'`).Scan(&u.id) != nil {
		t.Fatal("uncertain merge request")
	}
	u.runtime = r
	return u
}

// settle moves the original claim's end into the past, as time would.
func (u uncertainMerge) settle(t *testing.T) {
	t.Helper()
	if _, e := u.f.Admin.Pool.Exec(`UPDATE merge_requests SET claim_until=clock_timestamp()-interval '3 minutes' WHERE id=$1`, u.id); e != nil {
		t.Fatal(e)
	}
}

func (u uncertainMerge) count(t *testing.T, query string) int {
	t.Helper()
	var n int
	if e := u.f.Admin.Pool.QueryRow(query).Scan(&n); e != nil {
		t.Fatal(e)
	}
	return n
}

func quietReconciler(db *store.DB) *mergerecovery.Reconciler {
	return &mergerecovery.Reconciler{DB: db, Log: func(string, ...any) {}}
}

// The reconciler reads nothing while the original claim may still be in
// flight, and the journal refuses an outcome before the settle window.
func TestMergeReconciliationWaitsForTheClaimToSettle(t *testing.T) {
	u := newUncertainMerge(t, "")
	ctx := context.Background()
	forge := &mergeForge{observation: mergewriter.Observation{State: mergewriter.ObservedMerged, CommitSHA: strings.Repeat("d", 40)}}
	if _, attempted, e := quietReconciler(u.f.DB).Step(ctx, forge); e != nil || attempted || len(forge.seen) != 0 {
		t.Fatal("unsettled merge observed", attempted, e, len(forge.seen))
	}
	// Only the window ending 120 s after the claim counts as settled.
	if _, e := u.f.Admin.Pool.Exec(`UPDATE merge_requests SET claim_until=clock_timestamp()-interval '100 seconds' WHERE id=$1`, u.id); e != nil {
		t.Fatal(e)
	}
	if _, attempted, _ := quietReconciler(u.f.DB).Step(ctx, forge); attempted || len(forge.seen) != 0 {
		t.Fatal("merge observed inside the settle window")
	}
	var token, digest, payloadDigest string
	var payload []byte
	if u.f.Admin.Pool.QueryRow(`SELECT e.token::text,e.request_digest,r.payload_digest,r.payload FROM merge_effects e JOIN merge_requests r ON r.id=e.request_id`).Scan(&token, &digest, &payloadDigest, &payload) != nil {
		t.Fatal("effect")
	}
	c := store.MergeReconciliation{RequestID: u.id, PayloadDigest: payloadDigest, Payload: payload, EffectToken: token, RequestDigest: digest}
	if u.f.DB.ResolveMergeEffect(ctx, c, nil) == nil {
		t.Fatal("unsettled no-send recorded")
	}
	if state, e := u.runtime.PollOnce(ctx); e == nil || state != "merge-quarantined" {
		t.Fatal("quarantine without a reconciler", state, e)
	}
}

func TestMergeReconciliationOutcomes(t *testing.T) {
	merged := mergewriter.Observation{State: mergewriter.ObservedMerged, CommitSHA: strings.Repeat("d", 40)}
	for name, c := range map[string]struct {
		forge   *mergeForge
		digest  string
		outcome string
	}{
		"merged-exactly":   {forge: &mergeForge{observation: merged}, outcome: mergerecovery.Completed},
		"still-open":       {forge: &mergeForge{observation: mergewriter.Observation{State: mergewriter.ObservedUnmerged}}, outcome: mergerecovery.NotSent},
		"different":        {forge: &mergeForge{observation: mergewriter.Observation{State: mergewriter.ObservedDifferent}}, outcome: mergerecovery.Mismatch},
		"read-unavailable": {forge: &mergeForge{err: errors.New("403")}, outcome: mergerecovery.Unavailable},
		"unknown-state":    {forge: &mergeForge{observation: mergewriter.Observation{State: "pending"}}, outcome: mergerecovery.Unavailable},
		"other-request":    {forge: &mergeForge{observation: merged}, digest: strings.Repeat("f", 64), outcome: mergerecovery.Unplanned},
	} {
		t.Run(name, func(t *testing.T) {
			u := newUncertainMerge(t, c.digest)
			u.settle(t)
			ctx := context.Background()
			forge := c.forge
			reconciler := quietReconciler(u.f.DB)
			u.runtime.Reconciler, u.runtime.Observer = reconciler, forge
			state, e := u.runtime.PollOnce(ctx)
			if forge.writes.Load() != 0 {
				t.Fatal("reconciliation wrote to the forge")
			}
			resolved := c.outcome == mergerecovery.Completed || c.outcome == mergerecovery.NotSent
			if resolved != (e == nil && state == "merge-reconciled") || !resolved && state != "merge-quarantined" {
				t.Fatal("poll", state, e)
			}
			if c.outcome == mergerecovery.Unplanned {
				if len(forge.seen) != 0 {
					t.Fatal("unplanned request observed")
				}
			} else if len(forge.seen) != 1 || forge.seen[0].RequestDigest != u.planned.RequestDigest || forge.seen[0].Expected != u.planned.Expected {
				t.Fatal("observed something other than the planned merge")
			}
			blocked, e := u.journal.MergeBlocked(ctx)
			if e != nil || blocked == resolved {
				t.Fatal("quarantine", blocked, e)
			}
			// The request stays failed; no merged record is manufactured.
			if u.count(t, `SELECT count(*) FROM merge_requests WHERE state='failed' AND reason='uncertain'`) != 1 || u.count(t, `SELECT count(*) FROM merged_records`) != 0 {
				t.Fatal("reconciliation changed the request or created a merged record")
			}
			var receipt *[]byte
			var receiptDigest *string
			var notSent, completed *time.Time
			if u.f.Admin.Pool.QueryRow(`SELECT receipt,receipt_digest,not_sent_at,completed_at FROM merge_effects WHERE request_id=$1`, u.id).Scan(&receipt, &receiptDigest, &notSent, &completed) != nil {
				t.Fatal("effect row")
			}
			switch c.outcome {
			case mergerecovery.Completed:
				// The receipt the live acknowledgement stores, marked observed.
				want, _ := json.Marshal(store.MergeReceipt{Version: "ai-sdlc.merge-receipt/v1", CommitSHA: merged.CommitSHA, HeadSHA: u.planned.Expected.HeadSHA, BaseSHA: u.planned.Expected.BaseSHA, TreeSHA: u.planned.Expected.TreeSHA, PullRequest: u.planned.Expected.PullRequest, Observed: true})
				if !strings.HasSuffix(string(want), `,"observed":true}`) {
					t.Fatal("observed marker", string(want))
				}
				if receipt == nil || string(*receipt) != string(want) || receiptDigest == nil || *receiptDigest != authority.Digest(want) || completed == nil || notSent != nil {
					t.Fatal("observed receipt", receipt)
				}
			case mergerecovery.NotSent:
				if receipt != nil || completed != nil || notSent == nil {
					t.Fatal("no-send")
				}
			default:
				if receipt != nil || completed != nil || notSent != nil {
					t.Fatal("unresolved outcome changed the effect")
				}
				// Paced: the next poll does not observe again before the backoff.
				if _, attempted, _ := reconciler.Step(ctx, forge); attempted {
					t.Fatal("quarantined effect observed again without pacing")
				}
			}
			if resolved {
				// The merge flow continues: the next poll selects work as usual
				// (the fixture holds no other publication) and never replays.
				before := len(forge.seen)
				if state, e := u.runtime.PollOnce(ctx); e != nil || state != "" || len(forge.seen) != before || forge.writes.Load() != 0 {
					t.Fatal("merge flow did not continue", state, e)
				}
			}
		})
	}
}

// The journal accepts only the planned receipt, once, while nothing is claimed.
func TestResolveMergeEffectBindsTheJournaledCommand(t *testing.T) {
	u := newUncertainMerge(t, "")
	u.settle(t)
	ctx := context.Background()
	list, e := u.f.DB.MergeReconciliations(ctx, 16)
	if e != nil || len(list) != 1 || list[0].RequestID != u.id || list[0].RequestDigest != u.planned.RequestDigest {
		t.Fatal("reconciliation list", list, e)
	}
	c := list[0]
	if p, ok := mergerecovery.Plan(c); !ok || p.RequestDigest != u.planned.RequestDigest {
		t.Fatal("plan not re-derived from the journal")
	}
	good := store.MergeReceipt{Version: "ai-sdlc.merge-receipt/v1", CommitSHA: strings.Repeat("d", 40), HeadSHA: u.planned.Expected.HeadSHA, BaseSHA: u.planned.Expected.BaseSHA, TreeSHA: u.planned.Expected.TreeSHA, PullRequest: u.planned.Expected.PullRequest, Observed: true}
	for _, mutate := range []func(*store.MergeReceipt){
		// A live receipt never comes from reconciliation.
		func(r *store.MergeReceipt) { r.Observed = false },
		func(r *store.MergeReceipt) { r.HeadSHA = strings.Repeat("7", 40) },
		func(r *store.MergeReceipt) { r.TreeSHA = strings.Repeat("7", 40) },
		func(r *store.MergeReceipt) { r.BaseSHA = strings.Repeat("7", 40) },
		func(r *store.MergeReceipt) { r.PullRequest++ },
		func(r *store.MergeReceipt) { r.Version = "ai-sdlc.merge-receipt/v2" },
		func(r *store.MergeReceipt) { r.CommitSHA = r.HeadSHA },
	} {
		bad := good
		mutate(&bad)
		if u.f.DB.ResolveMergeEffect(ctx, c, &bad) == nil {
			t.Fatal("unplanned receipt recorded", bad)
		}
	}
	other := c
	other.RequestDigest = strings.Repeat("f", 64)
	if u.f.DB.ResolveMergeEffect(ctx, other, &good) == nil {
		t.Fatal("receipt for another request digest")
	}
	other = c
	other.PayloadDigest = strings.Repeat("f", 64)
	if u.f.DB.ResolveMergeEffect(ctx, other, &good) == nil {
		t.Fatal("receipt for another payload")
	}
	// The resolution takes the merge claim lock: while another session holds
	// it (a claim starting), nothing is recorded.
	conn, e := u.f.Admin.Pool.Conn(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	if _, e = conn.ExecContext(ctx, `SELECT pg_advisory_lock(73421026)`); e != nil {
		t.Fatal(e)
	}
	if u.f.DB.ResolveMergeEffect(ctx, c, &good) == nil {
		t.Fatal("resolved while the merge claim lock was held")
	}
	if _, e = conn.ExecContext(ctx, `SELECT pg_advisory_unlock(73421026)`); e != nil {
		t.Fatal(e)
	}
	if u.count(t, `SELECT count(*) FROM merge_effects WHERE completed_at IS NULL AND not_sent_at IS NULL`) != 1 {
		t.Fatal("effect changed under the lock")
	}
	if e := u.f.DB.ResolveMergeEffect(ctx, c, &good); e != nil {
		t.Fatal(e)
	}
	for _, again := range []*store.MergeReceipt{nil, &good} {
		if e := u.f.DB.ResolveMergeEffect(ctx, c, again); !errors.Is(e, store.ErrMergeEffectResolved) {
			t.Fatal("a resolved effect changed again or was not reported as resolved", e)
		}
	}
	if list, e := u.f.DB.MergeReconciliations(ctx, 16); e != nil || len(list) != 0 {
		t.Fatal("resolved effect still listed", list, e)
	}
	// A reconciled request never finishes as merged: only a live claim can
	// write a merged record, and a failed request is never claimed again.
	if _, e := u.journal.ClaimMerge(ctx, u.id); e == nil {
		t.Fatal("reconciled request claimed again")
	}
}

// Another replica (or an operator) resolving the effect while this one reads
// the forge is a bounded, quiet outcome, not an unavailable journal.
func TestMergeReconciliationLosingTheRaceIsResolved(t *testing.T) {
	u := newUncertainMerge(t, "")
	u.settle(t)
	ctx := context.Background()
	forge := &mergeForge{observation: mergewriter.Observation{State: mergewriter.ObservedMerged, CommitSHA: strings.Repeat("d", 40)}}
	forge.during = func() {
		if _, e := u.f.Admin.Pool.Exec(`UPDATE merge_effects SET not_sent_at=clock_timestamp() WHERE request_id=$1`, u.id); e != nil {
			t.Error(e)
		}
	}
	step, attempted, e := quietReconciler(u.f.DB).Step(ctx, forge)
	if e != nil || !attempted || step.Outcome != mergerecovery.Resolved {
		t.Fatal("lost race", step, attempted, e)
	}
	if u.count(t, `SELECT count(*) FROM merge_effects WHERE completed_at IS NOT NULL`) != 0 {
		t.Fatal("the other resolution was overwritten")
	}
}
