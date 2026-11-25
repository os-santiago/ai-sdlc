package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/candidate"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

// fakeEnd is the original worker's end as the controller observes it.
type fakeEnd struct {
	pod, container string
	code           int32
	observeErr     error
	raw            []byte
	exports        int
}

func (e *fakeEnd) WorkerFailure(context.Context) (string, string, int32, error) {
	return e.pod, e.container, e.code, e.observeErr
}

func (e *fakeEnd) Export(context.Context) ([]byte, int32, error) {
	e.exports++
	return e.raw, e.code, nil
}

func exportReceiptCode(t *testing.T, f *lifecycleFixture) int32 {
	t.Helper()
	var value []byte
	if err := f.Admin.Pool.QueryRow(`SELECT result FROM execution_effects WHERE kind='export'`).Scan(&value); err != nil {
		t.Fatal(err)
	}
	var receipt store.LifecycleResult
	if json.Unmarshal(value, &receipt) != nil || receipt.ExitCode == nil {
		t.Fatal("export receipt")
	}
	return *receipt.ExitCode
}

// A generation that ended on its tool-step budget with a change is exported
// and captured like a clean exit, and carries the bounded marker in both the
// returned record and the durable worker-exit observation.
func TestStepBudgetExitWithAChangeIsCapturedWithItsMarker(t *testing.T) {
	f, l, _, raw, pod := exitedWorkerFixture(t)
	candidatePrivileges(t, f.preparationFixture.fixture)
	end := &fakeEnd{pod: pod, container: "containerd://" + strings.Repeat("b", 64), code: candidate.StepBudgetExitCode, raw: raw}
	stage := "execution"
	record, err := finishGeneration(context.Background(), f.db, l, end, nonzeroWorkerExit{candidate.StepBudgetExitCode}, &stage)
	if err != nil || stage != "capture" || record.Changes != 1 || record.ExportSHA256 != workspacebundle.Digest(raw) {
		t.Fatal("step-budget candidate not captured", err, stage)
	}
	if record.GenerationEnded != candidate.GenerationEndedStepBudget || end.exports != 1 {
		t.Fatal("marker", record.GenerationEnded, end.exports)
	}
	if exportReceiptCode(t, f) != candidate.StepBudgetExitCode {
		t.Fatal("export receipt lost the observed exit")
	}
	if f.Count(t, `SELECT count(*) FROM execution_failures WHERE exit_code=25`) != 1 || f.Count(t, `SELECT count(*) FROM execution_candidates`) != 1 || f.Count(t, `SELECT count(*) FROM execution_lifecycles WHERE phase='candidate-captured'`) != 1 {
		t.Fatal("durable state")
	}
	if f.Count(t, `SELECT count(*) FROM model_budgets WHERE state='stopped' AND stop_reason='candidate-captured'`) != 1 {
		t.Fatal("model budget stayed open")
	}
}

// A clean exit is unchanged: captured without a marker and without a
// worker-exit observation.
func TestCleanExitIsCapturedWithoutAMarker(t *testing.T) {
	f, l, _, raw, pod := exitedWorkerFixture(t)
	candidatePrivileges(t, f.preparationFixture.fixture)
	end := &fakeEnd{pod: pod, container: "containerd://" + strings.Repeat("b", 64), raw: raw}
	stage := "execution"
	record, err := finishGeneration(context.Background(), f.db, l, end, nil, &stage)
	if err != nil || record.GenerationEnded != "" || record.Changes != 1 {
		t.Fatal("clean capture", err, record.GenerationEnded)
	}
	if f.Count(t, `SELECT count(*) FROM execution_failures`) != 0 {
		t.Fatal("clean exit recorded a failure")
	}
}

// A step-budget run that left no change stops as a worker exit, with its
// observation retained and no candidate.
func TestStepBudgetExitWithoutAChangeStops(t *testing.T) {
	f, l, _, _, pod := exitedWorkerFixture(t)
	candidatePrivileges(t, f.preparationFixture.fixture)
	end := &fakeEnd{pod: pod, container: "containerd://" + strings.Repeat("b", 64), code: candidate.StepBudgetExitCode, raw: l.prepared.Source()}
	stage := "execution"
	record, err := finishGeneration(context.Background(), f.db, l, end, nonzeroWorkerExit{candidate.StepBudgetExitCode}, &stage)
	code, recorded := WorkerFailureDiagnostic(err)
	if err == nil || record.RequestID != "" || DiagnosticStage(err) != "worker-exit" || code != candidate.StepBudgetExitCode || !recorded {
		t.Fatal("unchanged step-budget run continued", err)
	}
	if f.Count(t, `SELECT count(*) FROM execution_candidates`) != 0 || f.Count(t, `SELECT count(*) FROM execution_failures WHERE exit_code=25`) != 1 {
		t.Fatal("unchanged step-budget run persisted a candidate")
	}
}

// Every other nonzero exit stops before export exactly as before: the
// launcher's kill (124), SCC's time budget or an unclassified budget (22),
// provider and authentication errors, livelock and crashes (1).
func TestOtherNonzeroExitsStopBeforeExport(t *testing.T) {
	for _, code := range []int32{124, 22, 20, 21, 23, 1, 10} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			f, l, _, raw, pod := exitedWorkerFixture(t)
			candidatePrivileges(t, f.preparationFixture.fixture)
			end := &fakeEnd{pod: pod, container: "containerd://" + strings.Repeat("b", 64), code: code, raw: raw}
			stage := "execution"
			record, err := finishGeneration(context.Background(), f.db, l, end, nonzeroWorkerExit{code}, &stage)
			observed, recorded := WorkerFailureDiagnostic(err)
			if err == nil || record.RequestID != "" || observed != code || !recorded || DiagnosticStage(err) != "worker-exit" {
				t.Fatal("nonzero exit continued", err)
			}
			if end.exports != 0 || f.Count(t, `SELECT count(*) FROM execution_effects WHERE kind='export'`) != 0 || f.Count(t, `SELECT count(*) FROM execution_candidates`) != 0 {
				t.Fatal("nonzero exit exported")
			}
		})
	}
}

// Without a retained observation (or with one that contradicts the drive),
// a step-budget exit stops before export, so no candidate lacks its marker.
func TestStepBudgetExitWithoutARetainedObservationStops(t *testing.T) {
	for _, scenario := range []string{"observation-error", "different-code", "foreign-pod"} {
		t.Run(scenario, func(t *testing.T) {
			f, l, _, raw, pod := exitedWorkerFixture(t)
			candidatePrivileges(t, f.preparationFixture.fixture)
			end := &fakeEnd{pod: pod, container: "containerd://" + strings.Repeat("b", 64), code: candidate.StepBudgetExitCode, raw: raw}
			switch scenario {
			case "observation-error":
				end.observeErr = errors.New("observation failed")
			case "different-code":
				end.code = 22
			case "foreign-pod":
				end.pod = store.NewID()
			}
			stage := "execution"
			_, err := finishGeneration(context.Background(), f.db, l, end, nonzeroWorkerExit{candidate.StepBudgetExitCode}, &stage)
			if code, recorded := WorkerFailureDiagnostic(err); err == nil || code != candidate.StepBudgetExitCode || recorded {
				t.Fatal("unrecorded step-budget exit continued", err)
			}
			if end.exports != 0 || f.Count(t, `SELECT count(*) FROM execution_candidates`) != 0 {
				t.Fatal("unrecorded step-budget exit exported")
			}
		})
	}
}

// The candidate bounds of a clean exit apply unchanged: an export with more
// changes than a delta admits is refused at capture.
func TestStepBudgetCandidateKeepsTheCaptureBounds(t *testing.T) {
	f, l, _, raw, pod := exitedWorkerFixture(t)
	candidatePrivileges(t, f.preparationFixture.fixture)
	bundle, err := workspacebundle.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	for n := range candidate.MaxChanges {
		bundle.Files = append(bundle.Files, workspacebundle.File{Path: fmt.Sprintf("extra-%02d.txt", n), Mode: 0644, Data: []byte("unvalidated\n")})
	}
	if raw, err = workspacebundle.Encode(bundle); err != nil {
		t.Fatal(err)
	}
	end := &fakeEnd{pod: pod, container: "containerd://" + strings.Repeat("b", 64), code: candidate.StepBudgetExitCode, raw: raw}
	stage := "execution"
	record, err := finishGeneration(context.Background(), f.db, l, end, nonzeroWorkerExit{candidate.StepBudgetExitCode}, &stage)
	var stopped ControllerError
	if err == nil || record.RequestID != "" || !errors.As(err, &stopped) || stopped.Stage != "capture" {
		t.Fatal("oversized step-budget candidate captured", err)
	}
	if f.Count(t, `SELECT count(*) FROM execution_candidates`) != 0 {
		t.Fatal("oversized candidate persisted")
	}
}

// The store accepts a nonzero export receipt only for the step-budget code and
// only with its retained worker-exit observation for the same Pod.
func TestCaptureAcceptsOnlyAnObservedStepBudgetReceipt(t *testing.T) {
	for _, scenario := range []string{"time-budget", "kill", "unobserved", "other-observation"} {
		t.Run(scenario, func(t *testing.T) {
			f, l, _, raw, pod := exitedWorkerFixture(t)
			candidatePrivileges(t, f.preparationFixture.fixture)
			code := int32(candidate.StepBudgetExitCode)
			switch scenario {
			case "time-budget":
				code = 22
			case "kill":
				code = 124
			case "other-observation":
				if err := f.db.RecordExecutionFailure(context.Background(), l.durable, pod, "containerd://"+strings.Repeat("b", 64), 22); err != nil {
					t.Fatal(err)
				}
			}
			if err := l.RunEffect(context.Background(), "export", func(context.Context, EffectInput) (store.LifecycleResult, error) {
				return store.LifecycleResult{UID: pod, Digest: workspacebundle.Digest(raw), Bytes: len(raw), ExitCode: &code}, nil
			}); err != nil {
				t.Fatal(err)
			}
			if record, err := l.CaptureCandidate(context.Background(), raw); err == nil || record.RequestID != "" {
				t.Fatal("nonzero receipt captured", scenario)
			}
			if f.Count(t, `SELECT count(*) FROM execution_candidates`) != 0 {
				t.Fatal("nonzero receipt persisted")
			}
		})
	}
}

// Unchanged compares only trees, with no clock: an export equal to the
// original source or to the workspace's starting tree (a repair's rejected
// candidate) is unchanged; any other tree, or an undecodable export, is not.
func TestUnchangedComparesTheStartingTrees(t *testing.T) {
	sha := strings.Repeat("a", 40)
	encode := func(content string) []byte {
		raw, err := workspacebundle.Encode(workspacebundle.Bundle{Version: workspacebundle.Version, SourceSHA: sha, Files: []workspacebundle.File{{Path: "file.txt", Mode: 0644, Data: []byte(content)}}})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	original, parent, changed := encode("original\n"), encode("rejected candidate\n"), encode("repaired\n")
	l := &Lifecycle{prepared: Prepared{originalSource: original, source: parent}}
	for name, tc := range map[string]struct {
		raw  []byte
		want bool
	}{"original": {original, true}, "parent": {parent, true}, "changed": {changed, false}, "undecodable": {[]byte("{}"), false}, "empty": {nil, false}} {
		if l.Unchanged(tc.raw) != tc.want {
			t.Fatal(name)
		}
	}
}
