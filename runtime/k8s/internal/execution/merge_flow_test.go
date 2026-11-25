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

type mergeGatesFunc func(context.Context, mergegate.Expected, mergegate.Policy) (mergegate.Observation, error)

func (f mergeGatesFunc) Collect(c context.Context, e mergegate.Expected, p mergegate.Policy) (mergegate.Observation, error) {
	return f(c, e, p)
}

type mergeWriterFunc func(context.Context, mergegate.Expected, int, mergewriter.Fence) (mergewriter.Receipt, error)

func (f mergeWriterFunc) Merge(c context.Context, e mergegate.Expected, n int, g mergewriter.Fence) (mergewriter.Receipt, error) {
	return f(c, e, n, g)
}
func TestMergeControllerActualJournalAndSyntheticForge(t *testing.T) {
	for _, scenario := range []string{"success", "checks-pending", "changed-checks", "changed-profile", "revoked", "lost-write", "wrong-tree", "wrong-head", "issue-revoked", "wrong-profile", "expired-publication"} {
		t.Run(scenario, func(t *testing.T) {
			var lifetime []time.Duration
			if scenario == "expired-publication" {
				lifetime = []time.Duration{6 * time.Second}
			}
			f := execution.NewPublishedMergeTestFixture(t, lifetime...)
			ctx := context.Background()
			if scenario == "issue-revoked" {
				if _, e := f.Admin.Pool.Exec(`UPDATE work_items SET status='blocked'`); e != nil {
					t.Fatal(e)
				}
			}
			if scenario == "wrong-profile" {
				f.Profile = strings.Repeat("f", 64)
			}
			if scenario == "expired-publication" {
				fact, e := f.DB.ReadPublishedRecord(ctx, f.PublicationID)
				if e != nil {
					t.Fatal(e)
				}
				time.Sleep(time.Until(fact.AuthorityExpiresAt.Add(20 * time.Millisecond)))
			}
			writes, observations := 0, 0
			r := mergeflow.Runtime{DB: f.DB, Policy: func() (authority.Policy, intake.Config, string, error) { return f.Policy, f.Intake, f.Profile, nil }, Review: func() (publicationflow.ReviewPolicy, error) {
				return publicationflow.ReviewPolicy{Profile: f.ReviewProfile, Configuration: f.ReviewConfiguration}, nil
			}, Identity: func() (authority.Identity, error) { return f.Identity, nil }, GatePolicy: func() (mergegate.Policy, error) { return mergegate.PilotPolicy(), nil }}
			r.Gates = mergeGatesFunc(func(_ context.Context, e mergegate.Expected, _ mergegate.Policy) (mergegate.Observation, error) {
				observations++
				if scenario == "changed-profile" {
					f.Profile = strings.Repeat("f", 64)
				}
				o := mergegate.Observation{Expected: e, PolicySHA256: strings.Repeat("a", 64), ChecksSHA256: strings.Repeat("b", 64), ReviewsSHA256: strings.Repeat("c", 64), Ready: true, Reasons: []string{}, ObservedAt: time.Now()}
				if scenario == "checks-pending" {
					o.Ready = false
					o.Reasons = []string{"checks-not-passed"}
				}
				if scenario == "changed-checks" && observations > 1 {
					o.ChecksSHA256 = strings.Repeat("d", 64)
				}
				return o, nil
			})
			r.Writer = mergeWriterFunc(func(ctx context.Context, e mergegate.Expected, issue int, fence mergewriter.Fence) (mergewriter.Receipt, error) {
				if issue != 3 {
					t.Error("wrong issue")
				}
				if fence(ctx, strings.Repeat("f", 64)) != nil {
					return mergewriter.Receipt{}, errors.New("presend denied")
				}
				writes++
				if scenario == "lost-write" {
					return mergewriter.Receipt{}, errors.New("lost response")
				}
				if scenario == "revoked" {
					f.Policy.Freeze = true
				}
				if fence(ctx, strings.Repeat("f", 64)) != nil {
					return mergewriter.Receipt{}, errors.New("postsend denied")
				}
				receipt := mergewriter.Receipt{Version: "ai-sdlc.merge-receipt/v1", CommitSHA: strings.Repeat("d", 40), HeadSHA: e.HeadSHA, BaseSHA: e.BaseSHA, TreeSHA: e.TreeSHA, PullRequest: e.PullRequest}
				if scenario == "wrong-head" {
					receipt.HeadSHA = strings.Repeat("f", 40)
				}
				if scenario == "wrong-tree" {
					receipt.TreeSHA = strings.Repeat("e", 40)
				}
				return receipt, nil
			})
			state, e := r.PollOnce(ctx)
			if scenario == "success" || scenario == "expired-publication" {
				if e != nil || state != "merge-complete" || writes != 1 {
					t.Fatal(state, e, writes)
				}
				var id string
				if f.Admin.Pool.QueryRow(`SELECT merge_id FROM merged_records`).Scan(&id) != nil {
					t.Fatal("missing merge fact")
				}
				if _, e = f.DB.ReadMergedRecord(ctx, id); e != nil {
					t.Fatal(e)
				}
			} else if scenario == "issue-revoked" || scenario == "wrong-profile" {
				if e != nil || state != "" || writes != 0 || observations != 0 {
					t.Fatal("obsolete fact acquired authority", state, e, writes, observations)
				}
			} else if scenario == "checks-pending" {
				if e != nil || state != "merge-waiting-for-checks" || writes != 0 {
					t.Fatal(state, e, writes)
				}
				var n int
				if f.Admin.Pool.QueryRow(`SELECT count(*) FROM merge_requests`).Scan(&n) != nil || n != 0 {
					t.Fatal("CI wait acquired authority")
				}
			} else {
				if e == nil {
					t.Fatal("unsafe merge accepted")
				}
				var n int
				if f.Admin.Pool.QueryRow(`SELECT count(*) FROM merged_records`).Scan(&n) != nil || n != 0 {
					t.Fatal("unsafe terminal fact")
				}
			}
			if (scenario == "changed-checks" || scenario == "changed-profile" || scenario == "issue-revoked" || scenario == "wrong-profile") && writes != 0 {
				t.Fatal("rejected authority caused a write")
			}
			before := writes
			_, _ = r.PollOnce(ctx)
			if writes != before {
				t.Fatal("merge replayed")
			}
		})
	}
}

func TestInstalledMergePrivilegesAndAuthoritySelection(t *testing.T) {
	f := execution.NewPublishedMergeTestFixture(t)
	ctx := context.Background()
	if id, e := f.DB.NextMergePublication(ctx, mergeSelection(f)); e != nil || id != f.PublicationID {
		t.Fatal("installed selection", id, e)
	}
	changed := f.Policy
	changed.Freeze = true
	changedSelection := mergeSelection(f)
	changedSelection.AuthorityRevision = changed.Revision()
	if id, e := f.DB.NextMergePublication(ctx, changedSelection); e != nil || id != "" {
		t.Fatal("old policy publication remained selectable", id, e)
	}
	for _, query := range []string{
		`UPDATE merge_requests SET payload='changed' WHERE false`,
		`UPDATE merge_requests SET expires_at=clock_timestamp() WHERE false`,
		`UPDATE merge_effects SET request_digest='changed' WHERE false`,
		`UPDATE merged_records SET proof='changed' WHERE false`,
		`DELETE FROM merged_records WHERE false`,
		`INSERT INTO merged_records(merged_at) SELECT clock_timestamp() WHERE false`,
	} {
		if _, e := f.DB.Pool.Exec(query); e == nil {
			t.Fatal("forbidden merge mutation", query)
		}
	}
	passwords := map[string]string{}
	for _, role := range []string{"intake", "admission-controller", "orchestrator", "worker", "verifier", "release-manager", "relay", "deployer", "build-controller"} {
		passwords[role] = strings.ReplaceAll(role, "-", "_") + strings.Repeat("q", 40)
	}
	if e := f.Admin.ProvisionRolesForCapabilities(ctx, passwords, "verification", true, true); e != nil {
		t.Fatal(e)
	}
	for _, query := range []string{`SELECT * FROM merge_requests`, `UPDATE merge_requests SET state='failed' WHERE false`, `INSERT INTO merge_effects(request_id,token,request_digest) SELECT 'a','b','c' WHERE false`} {
		if _, e := f.DB.Pool.Exec(query); e == nil {
			t.Fatal("merge permission survived disable", query)
		}
	}
	if e := f.Admin.ProvisionRolesForMerge(ctx, passwords, "verification", true, true, true); e != nil {
		t.Fatal(e)
	}
	if id, e := f.DB.NextMergePublication(ctx, mergeSelection(f)); e != nil || id != f.PublicationID {
		t.Fatal("fact lost during reactivation", e)
	}
}

func mergeSelection(f execution.PublishedMergeTestFixture) store.MergeSelection {
	return store.MergeSelection{AuthorityRevision: f.Policy.Revision(), IntakeConfiguration: f.Intake.Digest(), ExecutionProfile: f.Profile, ReviewProfile: f.ReviewProfile, ReviewConfiguration: f.ReviewConfiguration}
}

func TestMergeSelectionSkipsChangedConfigurationAndIssue(t *testing.T) {
	for _, change := range []string{"intake", "execution-profile", "review-profile", "review-configuration", "generation", "work-state"} {
		t.Run(change, func(t *testing.T) {
			f := execution.NewPublishedMergeTestFixture(t)
			ctx := context.Background()
			selection := mergeSelection(f)
			if id, e := f.DB.NextMergePublication(ctx, selection); e != nil || id != f.PublicationID {
				t.Fatal("current fact", id, e)
			}
			stale := strings.Repeat("f", 64)
			switch change {
			case "intake":
				selection.IntakeConfiguration = stale
			case "execution-profile":
				selection.ExecutionProfile = stale
			case "review-profile":
				selection.ReviewProfile = stale
			case "review-configuration":
				selection.ReviewConfiguration = stale
			case "generation":
				if _, e := f.Admin.Pool.Exec(`UPDATE intake_heads SET generation=generation+1`); e != nil {
					t.Fatal(e)
				}
			case "work-state":
				if _, e := f.Admin.Pool.Exec(`UPDATE work_items SET status='blocked'`); e != nil {
					t.Fatal(e)
				}
			}
			if id, e := f.DB.NextMergePublication(ctx, selection); e != nil || id != "" {
				t.Fatal("obsolete fact remained selectable", id, e)
			}
			var facts, attempts int
			if f.Admin.Pool.QueryRow(`SELECT count(*) FROM published_records`).Scan(&facts) != nil || facts != 1 {
				t.Fatal("historical fact lost")
			}
			if f.Admin.Pool.QueryRow(`SELECT count(*) FROM merge_requests`).Scan(&attempts) != nil || attempts != 0 {
				t.Fatal("selection created authority")
			}
		})
	}
}

// A manifest repository's pull request says "Closes #N", so GitHub closes the
// issue as part of the merge and the intake records the closure as a new
// generation. Before the merge request is sent that observation still stops
// the run; after it may have been sent it must neither misreport the merge as
// uncertain nor leave an incomplete effect that refuses every later claim.
func TestMergeClosingItsIssueFinishesOnlyAfterTheSend(t *testing.T) {
	for _, scenario := range []string{"before-send", "during-presend-gate", "after-send", "before-acknowledge", "before-finish"} {
		t.Run(scenario, func(t *testing.T) {
			f := execution.NewPublishedMergeTestFixture(t)
			ctx := context.Background()
			closed := false
			closeIssue := func() {
				if !closed {
					closed = true
					f.RecordClosedIssueGeneration(t)
				}
			}
			writes, observations := 0, 0
			r := mergeflow.Runtime{DB: f.DB, Policy: func() (authority.Policy, intake.Config, string, error) {
				if scenario == "before-finish" && !closed {
					var acknowledged bool
					if e := f.Admin.Pool.QueryRow(`SELECT EXISTS(SELECT 1 FROM merge_effects WHERE completed_at IS NOT NULL)`).Scan(&acknowledged); e != nil {
						t.Error(e)
					}
					if acknowledged {
						closeIssue()
					}
				}
				return f.Policy, f.Intake, f.Profile, nil
			}, Review: func() (publicationflow.ReviewPolicy, error) {
				return publicationflow.ReviewPolicy{Profile: f.ReviewProfile, Configuration: f.ReviewConfiguration}, nil
			}, Identity: func() (authority.Identity, error) { return f.Identity, nil }, GatePolicy: func() (mergegate.Policy, error) { return mergegate.PilotPolicy(), nil }}
			r.Gates = mergeGatesFunc(func(_ context.Context, e mergegate.Expected, _ mergegate.Policy) (mergegate.Observation, error) {
				observations++
				if scenario == "during-presend-gate" && observations == 2 {
					closeIssue()
				}
				return mergegate.Observation{Expected: e, PolicySHA256: strings.Repeat("a", 64), ChecksSHA256: strings.Repeat("b", 64), ReviewsSHA256: strings.Repeat("c", 64), Ready: true, Reasons: []string{}, ObservedAt: time.Now()}, nil
			})
			r.Writer = mergeWriterFunc(func(ctx context.Context, e mergegate.Expected, _ int, fence mergewriter.Fence) (mergewriter.Receipt, error) {
				if scenario == "before-send" {
					closeIssue()
				}
				if fence(ctx, strings.Repeat("f", 64)) != nil {
					return mergewriter.Receipt{}, errors.New("presend denied")
				}
				writes++
				if scenario == "after-send" {
					closeIssue()
				}
				if fence(ctx, strings.Repeat("f", 64)) != nil {
					return mergewriter.Receipt{}, errors.New("postsend denied")
				}
				if scenario == "before-acknowledge" {
					closeIssue()
				}
				return mergewriter.Receipt{Version: "ai-sdlc.merge-receipt/v1", CommitSHA: strings.Repeat("d", 40), HeadSHA: e.HeadSHA, BaseSHA: e.BaseSHA, TreeSHA: e.TreeSHA, PullRequest: e.PullRequest}, nil
			})
			state, e := r.PollOnce(ctx)
			if !closed {
				t.Fatal("the closure was never recorded")
			}
			count := func(query string) int {
				t.Helper()
				var n int
				if err := f.Admin.Pool.QueryRow(query).Scan(&n); err != nil {
					t.Fatal(err)
				}
				return n
			}
			if scenario == "before-send" || scenario == "during-presend-gate" {
				if e == nil || state != "merge-stopped" || writes != 0 {
					t.Fatal("a closed issue was merged", state, e, writes)
				}
				if count(`SELECT count(*) FROM merged_records`) != 0 || count(`SELECT count(*) FROM merge_requests WHERE state='failed'`) != 1 || count(`SELECT count(*) FROM merge_effects WHERE completed_at IS NULL AND not_sent_at IS NULL`) != 0 {
					t.Fatal("pre-send stop left an unsafe journal")
				}
			} else {
				if e != nil || state != "merge-complete" || writes != 1 {
					t.Fatal("the merge closing its issue was not recorded", state, e, writes)
				}
				var id string
				if f.Admin.Pool.QueryRow(`SELECT merge_id FROM merged_records`).Scan(&id) != nil {
					t.Fatal("missing merge fact")
				}
				if _, e = f.DB.ReadMergedRecord(ctx, id); e != nil {
					t.Fatal(e)
				}
				if count(`SELECT count(*) FROM merge_requests WHERE state='merged'`) != 1 || count(`SELECT count(*) FROM merge_effects WHERE completed_at IS NOT NULL`) != 1 {
					t.Fatal("merge not terminal")
				}
			}
			// ClaimMerge refuses while any request is claimed or any failed request
			// holds an effect with no receipt and no not-sent seal.
			if count(`SELECT count(*) FROM merge_requests WHERE state='claimed'`) != 0 {
				t.Fatal("claim left open")
			}
			blocked, e := store.NewMergeJournal(f.DB, func() (authority.Policy, error) { return f.Policy, nil }).MergeBlocked(ctx)
			if e != nil || blocked {
				t.Fatal("later merges blocked", blocked, e)
			}
			before := writes
			if state, e = r.PollOnce(ctx); e != nil || state != "" || writes != before {
				t.Fatal("the next merge poll was refused or replayed", state, e, writes)
			}
		})
	}
}
