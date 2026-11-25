package execution

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelbroker"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelmailbox"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
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

// Only the worker's reader returns stale evidence (to refuse a changed snapshot
// at once). The verifier, publication and repair readers return no evidence on
// a stale read, and every reader still refuses with ErrExecution.
func TestOtherEvidenceReadersReturnNothingWhenStale(t *testing.T) {
	f := newFixture(t)
	connect := func(component string) *store.DB {
		role, _ := store.ComponentRole(component)
		u, err := url.Parse(os.Getenv("AI_SDLC_TEST_DATABASE_URL"))
		if err != nil {
			t.Fatal(err)
		}
		u.User = url.UserPassword(role, strings.ReplaceAll(component, "-", "_")+strings.Repeat("q", 40))
		db, err := store.Open(context.Background(), u.String())
		if err != nil {
			t.Fatal("fixture role connection failed")
		}
		t.Cleanup(func() { db.Close() })
		// Fixture-only grants, the same columns the worker fixture reads.
		if _, err = f.Admin.Pool.Exec(`
        GRANT SELECT (id,repository,correlation_id) ON plans TO ` + role + `;
        GRANT SELECT (uuid,plan_id,status,issue_numbers,acceptance_criteria,depends_on) ON work_items TO ` + role + `;
        GRANT SELECT (repository,issue_number,generation,content_key) ON intake_heads TO ` + role + `;
        GRANT SELECT (repository,issue_number,generation,event_id,revision,config_digest,payload) ON intake_revisions TO ` + role + `;
        GRANT SELECT (repository,config_digest,watermark) ON intake_cursors TO ` + role + `;
        GRANT SELECT (repository,issue_number,generation,config_digest,authority_revision,content_key,observed_at) ON intake_observations TO ` + role + `;
        GRANT SELECT (repository,issue_number,generation,event_id,payload) ON admission_heads,planning_heads TO ` + role + `;
        GRANT SELECT (repository,issue_number,issue_revision,decision,risk,policy_version,correlation_id,reasons,required_gates) ON admissions TO ` + role); err != nil {
			t.Fatal(err)
		}
		return db
	}
	verifier, releaseManager, orchestrator := connect("verifier"), connect("release-manager"), connect("orchestrator")
	c := f.command
	inTx := func(db *store.DB, read func(*sql.Tx) (store.ExecutionEvidence, error)) (e store.ExecutionEvidence, err error) {
		_ = db.WithTx(context.Background(), func(tx *sql.Tx) error { e, err = read(tx); return nil })
		return e, err
	}
	readers := map[string]func() (store.ExecutionEvidence, error){
		"verifier": func() (store.ExecutionEvidence, error) {
			return verifier.ReadVerificationEvidence(context.Background(), c.WorkItemID, c.Repository, c.IssueNumber, 60)
		},
		"verifier-tx": func() (store.ExecutionEvidence, error) {
			return inTx(verifier, func(tx *sql.Tx) (store.ExecutionEvidence, error) {
				return store.ReadVerificationEvidenceTx(context.Background(), tx, c.WorkItemID, c.Repository, c.IssueNumber, 60)
			})
		},
		"publication": func() (store.ExecutionEvidence, error) {
			return releaseManager.ReadPublicationEvidence(context.Background(), c.WorkItemID, c.Repository, c.IssueNumber, 60)
		},
		"repair": func() (store.ExecutionEvidence, error) {
			return inTx(orchestrator, func(tx *sql.Tx) (store.ExecutionEvidence, error) {
				return store.ReadRepairEvidence(context.Background(), tx, c.WorkItemID, c.Repository, c.IssueNumber, 60)
			})
		},
	}
	for name, read := range readers {
		if e, err := read(); err != nil || e.IntakeEvent == "" {
			t.Fatalf("%s: fresh evidence unreadable: %v", name, err)
		}
	}
	if _, err := f.Admin.Pool.Exec(`UPDATE intake_cursors SET watermark=clock_timestamp()-interval '10 minutes'`); err != nil {
		t.Fatal(err)
	}
	for name, read := range readers {
		e, err := read()
		if !errors.Is(err, store.ErrEvidenceStale) || !errors.Is(err, store.ErrExecution) || !reflect.DeepEqual(e, store.ExecutionEvidence{}) {
			t.Fatalf("%s: stale read returned evidence or another error: %v", name, err)
		}
	}
	e, err := f.db.ReadExecutionEvidence(context.Background(), c.WorkItemID, c.Repository, c.IssueNumber, 60)
	if !errors.Is(err, store.ErrEvidenceStale) || e.IntakeEvent == "" {
		t.Fatalf("worker: stale read must return the evidence with ErrEvidenceStale: %v", err)
	}
}

// staleThen refuses as stale for the given number of checks (forever when
// negative), then returns after: nil for fresh evidence, or another refusal.
func staleThen(wait time.Duration, stale int, after error, checks *int) func(context.Context) error {
	return func(context.Context) error {
		*checks++
		if stale < 0 || *checks <= stale {
			return staleEvidence{wait: wait}
		}
		return after
	}
}

// The generation loop waits, within the stale bound, for stale evidence to
// become fresh, without reading or delivering requests meanwhile.
func TestDriveModelWaitsForStaleEvidenceToBecomeFresh(t *testing.T) {
	s := &driverSession{requests: []modelmailbox.Envelope{req(1), req(1)}, exitAfter: 2}
	checks, calls := 0, 0
	err := driveModel(context.Background(), s, staleThen(time.Second, 3, nil, &checks), func(context.Context, modelmailbox.Envelope) error {
		if checks <= 3 {
			t.Error("delivered while evidence was stale")
		}
		calls++
		return nil
	})
	if err != nil || calls != 1 || checks < 4 {
		t.Fatalf("fresh evidence within the bound did not continue: err=%v calls=%d checks=%d", err, calls, checks)
	}
}

func TestDriveModelStaleEvidenceThatStaysStaleStopsByName(t *testing.T) {
	s := &driverSession{exitAfter: -1}
	checks := 0
	started := time.Now()
	err := driveModel(context.Background(), s, staleThen(300*time.Millisecond, -1, nil, &checks), func(context.Context, modelmailbox.Envelope) error {
		t.Error("delivered while evidence was stale")
		return nil
	})
	var stopped driveStop
	if !errors.Is(err, ErrAuthority) || !errors.As(err, &stopped) || stopped.stage != "execution-authority-snapshot-stale" || DiagnosticStage(ControllerError{stopped.stage}) != stopped.stage {
		t.Fatalf("stale evidence not stopped by name: %v", err)
	}
	if took := time.Since(started); took < 250*time.Millisecond || took > 3*time.Second || checks < 2 || s.read != 0 {
		t.Fatalf("wait not bounded by the refusal: took=%v checks=%d reads=%d", took, checks, s.read)
	}
}

func TestDriveModelOtherRefusalsDoNotWait(t *testing.T) {
	cases := map[string]struct {
		authorize func(*int) func(context.Context) error
		checks    int
	}{
		"snapshot": {func(c *int) func(context.Context) error {
			return staleThen(time.Minute, 0, authorityStop("snapshot"), c)
		}, 1},
		"binding": {func(c *int) func(context.Context) error {
			return staleThen(time.Minute, 0, authorityStop("binding"), c)
		}, 1},
		// Stale that turns into a revocation partway through the wait.
		"revoked-during-wait": {func(c *int) func(context.Context) error {
			return staleThen(time.Minute, 2, authorityStop("model-policy"), c)
		}, 3},
		"stale-without-a-bound": {func(c *int) func(context.Context) error { return staleThen(0, -1, nil, c) }, 1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			checks := 0
			started := time.Now()
			err := driveModel(context.Background(), &driverSession{exitAfter: -1}, tc.authorize(&checks), nil)
			var stopped driveStop
			if !errors.As(err, &stopped) || stopped.stage != "execution-authority" || checks != tc.checks || time.Since(started) > 2*time.Second {
				t.Fatalf("err=%v checks=%d took=%v", err, checks, time.Since(started))
			}
		})
	}
	// The claim (ctx) ends the wait at once, whatever the stale bound.
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	checks := 0
	err := driveModel(ctx, &driverSession{exitAfter: -1}, staleThen(time.Hour, -1, nil, &checks), nil)
	var stopped driveStop
	if !errors.As(err, &stopped) || stopped.stage != "execution-authority" || ctx.Err() == nil {
		t.Fatalf("claim end did not stop the stale wait: %v", err)
	}
}

// A stale result does not mask the checks after it: a changed binding or a
// changed reload refuses at once by its own step, not as snapshot-stale.
func TestStaleEvidenceDoesNotMaskLaterChecks(t *testing.T) {
	for _, which := range []string{"binding", "reload"} {
		t.Run(which, func(t *testing.T) {
			f, l, load, pod := modelLifecycleFixture(t)
			scope, err := l.ActivateModel(context.Background(), load)
			if err != nil {
				t.Fatal(err)
			}
			startModelLifecycle(t, l, pod)
			if _, err = f.Admin.Pool.Exec(`UPDATE intake_cursors SET watermark=clock_timestamp()-interval '10 minutes'`); err != nil {
				t.Fatal(err)
			}
			switch which {
			case "binding":
				// A consistently changed placement passes every load, but its
				// compiled workload is not the durable binding.
				original := l.load
				l.placement.Namespace = "other"
				l.load = func(ctx context.Context) (PreparationInputs, Placement, error) {
					in, placement, err := original(ctx)
					placement.Namespace = "other"
					return in, placement, err
				}
			case "reload":
				original, loads := l.load, 0
				l.load = func(ctx context.Context) (PreparationInputs, Placement, error) {
					in, placement, err := original(ctx)
					if loads++; loads > 1 {
						placement.Namespace = "other"
					}
					return in, placement, err
				}
			}
			_, err = l.Authorize(context.Background(), scope)
			var named interface{ AuthorityStep() string }
			var waits interface{ EvidenceWait() time.Duration }
			if !errors.Is(err, ErrAuthority) || !errors.As(err, &named) || named.AuthorityStep() != which || errors.As(err, &waits) {
				t.Fatalf("stale masked %s: %v", which, err)
			}
		})
	}
}
