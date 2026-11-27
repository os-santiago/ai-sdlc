package execution_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/execution"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/intake"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/mergeflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/mergegate"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/mergewriter"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/publicationflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

// retryHarness is a merge controller on the actual journal whose pre-send gate
// re-observation can be made to differ, as in battery 42: the queued gate was
// ready, and the run stopped before any merge effect was begun.
type retryHarness struct {
	f                    execution.PublishedMergeTestFixture
	r                    mergeflow.Runtime
	writes, observations int
	// changing makes every second gate observation (the pre-send one) differ.
	changing bool
	// beforeQueue, when set, runs during each queue-time gate observation:
	// after the publication was selected and before QueueMerge.
	beforeQueue func()
}

func newRetryHarness(t *testing.T) *retryHarness {
	t.Helper()
	h := &retryHarness{f: execution.NewPublishedMergeTestFixture(t), changing: true}
	f := h.f
	h.r = mergeflow.Runtime{DB: f.DB, Policy: func() (authority.Policy, intake.Config, string, error) { return f.Policy, f.Intake, f.Profile, nil }, Review: func() (publicationflow.ReviewPolicy, error) {
		return publicationflow.ReviewPolicy{Profile: f.ReviewProfile, Configuration: f.ReviewConfiguration}, nil
	}, Identity: func() (authority.Identity, error) { return f.Identity, nil }, GatePolicy: func() (mergegate.Policy, error) { return mergegate.PilotPolicy(), nil }}
	h.r.Gates = mergeGatesFunc(func(_ context.Context, e mergegate.Expected, _ mergegate.Policy) (mergegate.Observation, error) {
		h.observations++
		if h.observations%2 == 1 && h.beforeQueue != nil {
			h.beforeQueue()
		}
		o := mergegate.Observation{Expected: e, PolicySHA256: strings.Repeat("a", 64), ChecksSHA256: strings.Repeat("b", 64), ReviewsSHA256: strings.Repeat("c", 64), Ready: true, Reasons: []string{}, ObservedAt: time.Now()}
		if h.changing && h.observations%2 == 0 {
			o.ChecksSHA256 = strings.Repeat("d", 64)
		}
		return o, nil
	})
	h.r.Writer = mergeWriterFunc(func(ctx context.Context, e mergegate.Expected, _ int, fence mergewriter.Fence) (mergewriter.Receipt, error) {
		if fence(ctx, strings.Repeat("f", 64)) != nil {
			return mergewriter.Receipt{}, errors.New("presend denied")
		}
		h.writes++
		if fence(ctx, strings.Repeat("f", 64)) != nil {
			return mergewriter.Receipt{}, errors.New("postsend denied")
		}
		return mergewriter.Receipt{Version: "ai-sdlc.merge-receipt/v1", CommitSHA: strings.Repeat("d", 40), HeadSHA: e.HeadSHA, BaseSHA: e.BaseSHA, TreeSHA: e.TreeSHA, PullRequest: e.PullRequest}, nil
	})
	return h
}

func (h *retryHarness) count(t *testing.T, query string) int {
	t.Helper()
	var n int
	if e := h.f.Admin.Pool.QueryRow(query).Scan(&n); e != nil {
		t.Fatal(e)
	}
	return n
}

// age moves every failed request's command and claim end into the past, as
// time would.
func (h *retryHarness) age(t *testing.T) {
	t.Helper()
	if _, e := h.f.Admin.Pool.Exec(`UPDATE merge_requests SET expires_at=t.v,claim_until=CASE WHEN claim_until IS NULL THEN NULL ELSE t.v END FROM (SELECT clock_timestamp()-interval '10 minutes' AS v) t WHERE state='failed'`); e != nil {
		t.Fatal(e)
	}
}

func (h *retryHarness) journal() *store.MergeJournal {
	return store.NewMergeJournal(h.f.DB, func() (authority.Policy, error) { return h.f.Policy, nil })
}

// stopBeforeSend runs one poll that must stop before any merge effect, with
// the given stage, and leave a not-sent request that quarantines nothing.
func (h *retryHarness) stopBeforeSend(t *testing.T, stage string) error {
	t.Helper()
	failed := h.count(t, `SELECT count(*) FROM merge_requests WHERE state='failed'`)
	state, e := h.r.PollOnce(context.Background())
	if e == nil || state != "merge-stopped" || mergeflow.DiagnosticStage(e) != stage || h.writes != 0 {
		t.Fatal("pre-send stop", state, e, mergeflow.DiagnosticStage(e), h.writes)
	}
	if h.count(t, `SELECT count(*) FROM merge_requests WHERE state='failed' AND reason='not-sent'`) != failed+1 || h.count(t, `SELECT count(*) FROM merge_requests WHERE reason='uncertain'`) != 0 {
		t.Fatal("a stop that sent nothing was not recorded as not-sent")
	}
	if blocked, e := h.journal().MergeBlocked(context.Background()); e != nil || blocked {
		t.Fatal("a stop that sent nothing quarantined merges", blocked, e)
	}
	return e
}

// Battery 42: the pre-send gate re-observation refused a ready queued gate and
// the run stopped before any merge effect. The publication gets a fresh
// attempt, with a new gate observation and signed command, once the stopped
// command has expired; it merges exactly once.
func TestMergeStoppedBeforeSendGetsAFreshAttempt(t *testing.T) {
	h := newRetryHarness(t)
	ctx := context.Background()
	if mergeflow.AttemptsExhausted(h.stopBeforeSend(t, "gate-changed-checks")) {
		t.Fatal("first attempt reported exhaustion")
	}
	if h.count(t, `SELECT count(*) FROM merge_effects`) != 0 {
		t.Fatal("a merge effect was begun")
	}
	var first string
	if h.f.Admin.Pool.QueryRow(`SELECT id FROM merge_requests`).Scan(&first) != nil {
		t.Fatal("missing stopped request")
	}
	// The stopped command has not expired long enough: no new authority yet.
	if state, e := h.r.PollOnce(ctx); e != nil || state != "" || h.observations != 2 || h.count(t, `SELECT count(*) FROM merge_requests`) != 1 {
		t.Fatal("retry before the backoff", state, e, h.observations)
	}
	h.age(t)
	h.changing = false
	if state, e := h.r.PollOnce(ctx); e != nil || state != "merge-complete" || h.writes != 1 || h.observations != 4 {
		t.Fatal("fresh attempt did not merge", state, e, h.writes, h.observations)
	}
	var merged string
	if h.f.Admin.Pool.QueryRow(`SELECT merge_id FROM merged_records`).Scan(&merged) != nil || merged == first {
		t.Fatal("the fresh attempt reused the stopped request", merged)
	}
	if _, e := h.f.DB.ReadMergedRecord(ctx, merged); e != nil {
		t.Fatal(e)
	}
	if h.count(t, `SELECT count(*) FROM merge_requests`) != 2 || h.count(t, `SELECT count(DISTINCT gate_digest) FROM merge_requests`) != 2 || h.count(t, `SELECT count(DISTINCT payload_digest) FROM merge_requests`) != 2 {
		t.Fatal("the fresh attempt did not carry its own gate and signed command")
	}
	// A merged publication is never selected again, even after time passes.
	h.age(t)
	if state, e := h.r.PollOnce(ctx); e != nil || state != "" || h.writes != 1 {
		t.Fatal("merged publication attempted again", state, e, h.writes)
	}
	// The schema itself admits no second request beside one that may have sent.
	if _, e := h.f.Admin.Pool.Exec(`INSERT INTO merge_requests(id,publication_id,record_digest,gate_digest,authority_revision,payload_digest,payload,receipt,gate,policy,expires_at)
 SELECT 'merge-'||repeat('e',64),publication_id,record_digest,gate_digest,authority_revision,payload_digest,payload,receipt,gate,policy,expires_at FROM merge_requests WHERE id=$1`, merged); e == nil {
		t.Fatal("a second request beside a merged one was admitted")
	}
}

// Attempts are bounded: after store.MergeAttempts pre-send stops the
// publication is no longer selected and nothing is sent.
func TestMergeStoppedBeforeSendAttemptsAreBounded(t *testing.T) {
	h := newRetryHarness(t)
	ctx := context.Background()
	for i := 0; i < store.MergeAttempts; i++ {
		e := h.stopBeforeSend(t, "gate-changed-checks")
		// Only the last allowed attempt reports exhaustion.
		if mergeflow.AttemptsExhausted(e) != (i == store.MergeAttempts-1) {
			t.Fatal("exhaustion", i, e)
		}
		h.age(t)
	}
	if state, e := h.r.PollOnce(ctx); e != nil || state != "" || h.writes != 0 || h.observations != 2*store.MergeAttempts {
		t.Fatal("attempts not bounded", state, e, h.writes, h.observations)
	}
	if h.count(t, `SELECT count(*) FROM merge_requests WHERE reason='not-sent'`) != store.MergeAttempts {
		t.Fatal("attempt history")
	}
	if id, e := h.f.DB.NextMergePublication(ctx, mergeSelection(h.f)); e != nil || id != "" {
		t.Fatal("exhausted publication selected", id, e)
	}
}

// An intent sealed not-sent by the pre-send fence also proves nothing was sent:
// the stop is not-sent and the publication merges on a fresh attempt.
func TestMergeSealedNotSentGetsAFreshAttempt(t *testing.T) {
	h := newRetryHarness(t)
	h.changing = false
	ctx := context.Background()
	failed := false
	f := h.f
	h.r.Review = func() (publicationflow.ReviewPolicy, error) {
		var intent bool
		if e := f.Admin.Pool.QueryRow(`SELECT EXISTS(SELECT 1 FROM merge_effects WHERE completed_at IS NULL AND not_sent_at IS NULL)`).Scan(&intent); e != nil {
			t.Error(e)
		}
		if intent && !failed {
			failed = true
			return publicationflow.ReviewPolicy{}, errors.New("review configuration unavailable")
		}
		return publicationflow.ReviewPolicy{Profile: f.ReviewProfile, Configuration: f.ReviewConfiguration}, nil
	}
	h.stopBeforeSend(t, "publication-fence")
	if h.count(t, `SELECT count(*) FROM merge_effects WHERE not_sent_at IS NOT NULL`) != 1 {
		t.Fatal("intent not sealed")
	}
	h.age(t)
	if state, e := h.r.PollOnce(ctx); e != nil || state != "merge-complete" || h.writes != 1 {
		t.Fatal("fresh attempt did not merge", state, e, h.writes)
	}
}

// A request that may have been sent is never retried: it stays uncertain,
// quarantines merges and its publication is never selected again.
func TestMergeUncertainSendIsNeverRetried(t *testing.T) {
	u := newUncertainMerge(t, "")
	u.settle(t)
	if _, e := u.f.Admin.Pool.Exec(`UPDATE merge_requests SET expires_at=clock_timestamp()-interval '3 minutes' WHERE id=$1`, u.id); e != nil {
		t.Fatal(e)
	}
	if id, e := u.f.DB.NextMergePublication(context.Background(), mergeSelection(u.f)); e != nil || id != "" {
		t.Fatal("uncertain publication selected", id, e)
	}
	if u.count(t, `SELECT count(*) FROM merge_requests WHERE reason='not-sent'`) != 0 {
		t.Fatal("an uncertain send was recorded as not-sent")
	}
}

// Expiry classifies like a stop: an expired request with no merge effect sent
// nothing; one with an unacknowledged intent stays expired and quarantines.
func TestExpiredMergeIsNotSentOnlyWithoutAnIntent(t *testing.T) {
	h := newRetryHarness(t)
	ctx := context.Background()
	h.stopBeforeSend(t, "gate-changed-checks")
	if _, e := h.f.Admin.Pool.Exec(`UPDATE merge_requests SET state='queued',reason='',claim_token=NULL,claim_until=NULL,expires_at=clock_timestamp()-interval '1 second'`); e != nil {
		t.Fatal(e)
	}
	if e := h.journal().ExpireMerges(ctx); e != nil || h.count(t, `SELECT count(*) FROM merge_requests WHERE state='failed' AND reason='not-sent'`) != 1 {
		t.Fatal("expired request without an intent", e)
	}
	u := newUncertainMerge(t, "")
	if _, e := u.f.Admin.Pool.Exec(`UPDATE merge_requests SET state='claimed',reason='',expires_at=t.v,claim_until=t.v FROM (SELECT clock_timestamp()-interval '1 second' AS v) t WHERE id=$1`, u.id); e != nil {
		t.Fatal(e)
	}
	if e := u.journal.ExpireMerges(ctx); e != nil || u.count(t, `SELECT count(*) FROM merge_requests WHERE state='failed' AND reason='expired'`) != 1 {
		t.Fatal("expired request with an intent", e)
	}
	if blocked, e := u.journal.MergeBlocked(ctx); e != nil || !blocked {
		t.Fatal("expired intent did not quarantine", blocked, e)
	}
}

// setLatestExpiry moves every request's command (and claim) expiry to the
// given offset from now, so the newest command expired then.
func (h *retryHarness) setLatestExpiry(t *testing.T, offset string) {
	t.Helper()
	if _, e := h.f.Admin.Pool.Exec(`UPDATE merge_requests SET expires_at=t.v,claim_until=CASE WHEN claim_until IS NULL THEN NULL ELSE t.v END FROM (SELECT clock_timestamp()+$1::interval AS v) t`, offset); e != nil {
		t.Fatal(e)
	}
}

// The wait before a fresh attempt grows: 30 s after the first stopped command
// expired, 2 min after the second.
func TestMergeRetryWaitGrows(t *testing.T) {
	h := newRetryHarness(t)
	ctx := context.Background()
	selectable := func() bool {
		t.Helper()
		id, e := h.f.DB.NextMergePublication(ctx, mergeSelection(h.f))
		if e != nil {
			t.Fatal(e)
		}
		return id != ""
	}
	h.stopBeforeSend(t, "gate-changed-checks")
	h.setLatestExpiry(t, "-20 seconds")
	if selectable() {
		t.Fatal("second attempt before 30 s")
	}
	h.setLatestExpiry(t, "-40 seconds")
	if !selectable() {
		t.Fatal("second attempt not admitted after 30 s")
	}
	h.stopBeforeSend(t, "gate-changed-checks")
	h.setLatestExpiry(t, "-40 seconds")
	if selectable() {
		t.Fatal("third attempt after only 30 s")
	}
	h.setLatestExpiry(t, "-130 seconds")
	if !selectable() {
		t.Fatal("third attempt not admitted after 2 min")
	}
}

// QueueMerge enforces the wait itself, not only the selection: a stopped
// command whose wait has not elapsed by the time the fresh request is queued
// refuses the request.
func TestQueueMergeEnforcesTheRetryWait(t *testing.T) {
	h := newRetryHarness(t)
	ctx := context.Background()
	h.stopBeforeSend(t, "gate-changed-checks")
	h.age(t)
	h.beforeQueue = func() { h.setLatestExpiry(t, "-1 second") }
	if state, e := h.r.PollOnce(ctx); e == nil || state != "merge-queue-denied" || h.writes != 0 || h.count(t, `SELECT count(*) FROM merge_requests`) != 1 {
		t.Fatal("queue ignored the retry wait", state, e, h.writes)
	}
	h.beforeQueue = nil
	h.changing = false
	h.age(t)
	if state, e := h.r.PollOnce(ctx); e != nil || state != "merge-complete" || h.writes != 1 {
		t.Fatal("attempt after the wait", state, e, h.writes)
	}
}

// QueueMerge enforces the attempt limit itself: a request that would exceed
// store.MergeAttempts is refused even after the publication was selected.
func TestQueueMergeEnforcesTheAttemptLimit(t *testing.T) {
	h := newRetryHarness(t)
	ctx := context.Background()
	for i := 0; i < store.MergeAttempts-1; i++ {
		h.stopBeforeSend(t, "gate-changed-checks")
		h.age(t)
	}
	h.beforeQueue = func() {
		// Another attempt lands between selection and queueing.
		if _, e := h.f.Admin.Pool.Exec(`INSERT INTO merge_requests(id,publication_id,record_digest,gate_digest,authority_revision,payload_digest,payload,receipt,gate,policy,expires_at,state,claim_token,claim_until,reason)
 SELECT 'merge-'||repeat('a',64),publication_id,record_digest,gate_digest,authority_revision,payload_digest,payload,receipt,gate,policy,expires_at,state,claim_token,claim_until,reason FROM merge_requests ORDER BY created_at DESC LIMIT 1`); e != nil {
			t.Error(e)
		}
	}
	h.changing = false
	if state, e := h.r.PollOnce(ctx); e == nil || state != "merge-queue-denied" || h.writes != 0 || h.count(t, `SELECT count(*) FROM merge_requests`) != store.MergeAttempts {
		t.Fatal("queue exceeded the attempt limit", state, e, h.writes)
	}
}

// A reconciler "no-send" outcome resolves the quarantine but never retries
// the publication: the request may have been sent, so it stays uncertain.
func TestMergeReconciledNoSendIsNeverRetried(t *testing.T) {
	u := newUncertainMerge(t, "")
	u.settle(t)
	ctx := context.Background()
	forge := &mergeForge{observation: mergewriter.Observation{State: mergewriter.ObservedUnmerged}}
	u.runtime.Reconciler, u.runtime.Observer = quietReconciler(u.f.DB), forge
	if state, e := u.runtime.PollOnce(ctx); e != nil || state != "merge-reconciled" {
		t.Fatal("no-send reconciliation", state, e)
	}
	if u.count(t, `SELECT count(*) FROM merge_effects WHERE not_sent_at IS NOT NULL`) != 1 {
		t.Fatal("no-send not recorded")
	}
	if _, e := u.f.Admin.Pool.Exec(`UPDATE merge_requests SET expires_at=t.v,claim_until=t.v FROM (SELECT clock_timestamp()-interval '1 hour' AS v) t WHERE id=$1`, u.id); e != nil {
		t.Fatal(e)
	}
	if id, e := u.f.DB.NextMergePublication(ctx, mergeSelection(u.f)); e != nil || id != "" {
		t.Fatal("reconciled no-send publication selected", id, e)
	}
	if state, e := u.runtime.PollOnce(ctx); e != nil || state != "" || forge.writes.Load() != 0 {
		t.Fatal("reconciled no-send retried", state, e)
	}
	if u.count(t, `SELECT count(*) FROM merge_requests`) != 1 || u.count(t, `SELECT count(*) FROM merge_requests WHERE reason='uncertain'`) != 1 {
		t.Fatal("reconciled no-send changed the request or queued another")
	}
}
