package execution

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelbroker"
)

// Unchanged evidence that is only older than its window is named apart from a
// missing or changed snapshot. Both still refuse: neither authorizes anything.
func TestExecutionReadinessNamesStaleEvidenceApartFromAChangedSnapshot(t *testing.T) {
	cases := map[string]struct {
		mutation string
		stale    bool
	}{
		"stale-poller":                {`UPDATE intake_cursors SET watermark=clock_timestamp()-interval '10 minutes'`, true},
		"stale-observation":           {`UPDATE intake_observations SET observed_at=clock_timestamp()-interval '10 minutes'`, true},
		"stale-and-newer-intake":      {`UPDATE intake_cursors SET watermark=clock_timestamp()-interval '10 minutes'; UPDATE intake_heads SET generation=2`, false},
		"stale-and-newer-plan":        {`UPDATE intake_cursors SET watermark=clock_timestamp()-interval '10 minutes'; UPDATE planning_heads SET generation=2`, false},
		"stale-and-withdrawn":         {`UPDATE intake_cursors SET watermark=clock_timestamp()-interval '10 minutes'; UPDATE admissions SET decision='rejected'`, false},
		"stale-and-changed-criteria":  {`UPDATE intake_observations SET observed_at=clock_timestamp()-interval '10 minutes'; UPDATE work_items SET acceptance_criteria='["Different scope"]'`, false},
		"stale-and-observation-other": {`UPDATE intake_cursors SET watermark=clock_timestamp()-interval '10 minutes'; UPDATE intake_observations SET authority_revision=repeat('d',64)`, false},
		"missing-poll-proof":          {`UPDATE intake_cursors SET watermark=NULL`, false},
		"future-poll-proof":           {`UPDATE intake_cursors SET watermark=clock_timestamp()+interval '1 hour'`, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			if _, err := verifiedSnapshotStatus(context.Background(), f.db, f.Material.Policy, f.Config, f.command, "ready"); err != nil {
				t.Fatal("fresh fixture evidence refused", err)
			}
			if _, err := f.Admin.Pool.Exec(tc.mutation); err != nil {
				t.Fatal(err)
			}
			_, err := verifiedSnapshotStatus(context.Background(), f.db, f.Material.Policy, f.Config, f.command, "ready")
			if !errors.Is(err, ErrAuthority) {
				t.Fatal("stale or changed evidence did not refuse", err)
			}
			if errors.Is(err, errSnapshotStale) != tc.stale {
				t.Fatalf("stale=%v, want %v", errors.Is(err, errSnapshotStale), tc.stale)
			}
		})
	}
}

// The model authorization names stale evidence snapshot-stale, with a bounded
// wait, and the same lifecycle authorizes again once a complete scan refreshes
// it. A changed snapshot is still the step snapshot, without a wait.
func TestLifecycleAuthorizeNamesStaleEvidenceAndRecovers(t *testing.T) {
	f, l, load, pod := modelLifecycleFixture(t)
	scope, err := l.ActivateModel(context.Background(), load)
	if err != nil {
		t.Fatal(err)
	}
	startModelLifecycle(t, l, pod)
	if _, err = l.Authorize(context.Background(), scope); err != nil {
		t.Fatal("current owner denied", err)
	}
	if _, err = f.Admin.Pool.Exec(`UPDATE intake_cursors SET watermark=clock_timestamp()-interval '10 minutes'`); err != nil {
		t.Fatal(err)
	}
	_, err = l.Authorize(context.Background(), scope)
	var stale interface {
		AuthorityStep() string
		EvidenceWait() time.Duration
	}
	if !errors.Is(err, ErrAuthority) || !errors.As(err, &stale) || stale.AuthorityStep() != "snapshot-stale" || stale.EvidenceWait() != 90*time.Second {
		t.Fatal("stale evidence not named with its wait", err)
	}
	if _, err = f.Admin.Pool.Exec(`UPDATE intake_cursors SET watermark=clock_timestamp()`); err != nil {
		t.Fatal(err)
	}
	if _, err = l.Authorize(context.Background(), scope); err != nil {
		t.Fatal("refreshed evidence denied", err)
	}
	if _, err = f.Admin.Pool.Exec(`UPDATE intake_cursors SET watermark=clock_timestamp()-interval '10 minutes'; UPDATE intake_heads SET generation=2`); err != nil {
		t.Fatal(err)
	}
	_, err = l.Authorize(context.Background(), scope)
	var named interface{ AuthorityStep() string }
	var waits interface{ EvidenceWait() time.Duration }
	if !errors.Is(err, ErrAuthority) || !errors.As(err, &named) || named.AuthorityStep() != "snapshot" || errors.As(err, &waits) {
		t.Fatal("changed snapshot not refused at once by its own step", err)
	}
}

func TestStaleEvidenceStopIsBoundedAndReported(t *testing.T) {
	err := error(staleEvidence{wait: staleEvidenceWait(30)})
	var named interface{ AuthorityStep() string }
	if !errors.Is(err, ErrAuthority) || !errors.As(err, &named) || named.AuthorityStep() != "snapshot-stale" {
		t.Fatal(err)
	}
	for poll, want := range map[int]time.Duration{30: 90 * time.Second, 60: 90 * time.Second, 75: 105 * time.Second, 90: 120 * time.Second, 3600: 120 * time.Second} {
		if got := staleEvidenceWait(poll); got != want {
			t.Fatalf("poll %d: wait %v, want %v", poll, got, want)
		}
	}
	for _, at := range []string{"reservation", "dispatch", "publication"} {
		stage := "authorization-" + at + "-snapshot-stale"
		if modelbroker.SafeFailureStage(stage) != stage {
			t.Fatalf("broker does not report %q", stage)
		}
	}
	if DiagnosticStage(ControllerError{"model-authorization-publication-snapshot-stale"}) != "model-authorization-publication-snapshot-stale" {
		t.Fatal("execution diagnostic drops snapshot-stale")
	}
}
