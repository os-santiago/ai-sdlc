package mergeflow

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/mergegate"
)

func TestDiagnosticStageIsBounded(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{StopError{Stage: "gate-changed-checks"}, "gate-changed-checks"},
		{StopError{Stage: "gate-deadline"}, "gate-deadline"},
		{StopError{Stage: "transport"}, "transport"},
		{fmt.Errorf("wrapped: %w", StopError{Stage: "claim"}), "claim"},
		{StopError{Stage: "secret response body"}, "unknown"},
		{ErrMerge, "unknown"},
		{errors.New("credential text"), "unknown"},
	} {
		if got := DiagnosticStage(c.err); got != c.want {
			t.Errorf("DiagnosticStage(%v) = %q, want %q", c.err, got, c.want)
		}
	}
	if !errors.Is(StopError{Stage: "claim"}, ErrMerge) {
		t.Fatal("a stop no longer unwraps to ErrMerge")
	}
}

func TestGateChangeNamesTheFirstDifference(t *testing.T) {
	ready := mergegate.Observation{Expected: mergegate.Expected{Repository: "os-santiago/homedir", PullRequest: 7}, PolicySHA256: strings.Repeat("a", 64), ChecksSHA256: strings.Repeat("b", 64), ReviewsSHA256: strings.Repeat("c", 64), Ready: true, Reasons: []string{}}
	if got := gateChange(ready, ready); got != "" {
		t.Fatal("same gate reported changed", got)
	}
	for want, change := range map[string]func(*mergegate.Observation){
		"gate-changed-checks":         func(o *mergegate.Observation) { o.ChecksSHA256 = strings.Repeat("d", 64) },
		"gate-changed-reviews":        func(o *mergegate.Observation) { o.ReviewsSHA256 = strings.Repeat("d", 64) },
		"gate-changed-policy":         func(o *mergegate.Observation) { o.PolicySHA256 = strings.Repeat("d", 64) },
		"gate-changed-expected":       func(o *mergegate.Observation) { o.Expected.PullRequest = 8 },
		"gate-native-merge-not-ready": func(o *mergegate.Observation) { o.Ready = false; o.Reasons = []string{"native-merge-not-ready"} },
		"gate-not-ready":              func(o *mergegate.Observation) { o.Ready = false; o.Reasons = []string{"unlisted forge text"} },
	} {
		current := ready
		current.Reasons = append([]string{}, ready.Reasons...)
		change(&current)
		got := gateChange(ready, current)
		if got != want || !stopStages[got] {
			t.Errorf("gateChange = %q, want %q", got, want)
		}
	}
	queued := ready
	queued.Ready = false
	if got := gateChange(queued, ready); got != "gate-queued-invalid" {
		t.Fatal("an unready queued gate was accepted", got)
	}
}

// A gate that is not ready names its real blocker: an unresolved review
// thread is not a pending check, and every blocker is listed.
func TestWaitNamesEachBlocker(t *testing.T) {
	for _, c := range []struct {
		reasons []string
		threads int
		state   string
		listed  []string
		count   int
	}{
		{[]string{"checks-not-passed"}, 0, "merge-waiting-for-checks", []string{"checks-not-passed"}, 0},
		// Observed on a published pull request held by one third-party thread.
		{[]string{"native-merge-not-ready", "review-threads-unresolved"}, 1, "merge-waiting-for-review-threads", []string{"review-threads-unresolved", "native-merge-not-ready"}, 1},
		{[]string{"review-changes-requested", "review-threads-unresolved"}, 3, "merge-waiting-for-changes-requested", []string{"review-changes-requested", "review-threads-unresolved"}, 3},
		{[]string{"checks-not-passed", "review-threads-unresolved"}, 2, "merge-waiting-for-checks", []string{"checks-not-passed", "review-threads-unresolved"}, 2},
		{[]string{"native-protection-insufficient", "checks-not-passed"}, 0, "merge-waiting-for-protection", []string{"native-protection-insufficient", "checks-not-passed"}, 0},
		{[]string{"native-merge-not-ready"}, 0, "merge-waiting-for-mergeability", []string{"native-merge-not-ready"}, 0},
		{[]string{"unlisted forge text"}, 0, "merge-waiting", []string{}, 0},
		// A count without its reason, or beyond the gate's bound, is not reported.
		{[]string{"checks-not-passed"}, 4, "merge-waiting-for-checks", []string{"checks-not-passed"}, 0},
		{[]string{"review-threads-unresolved"}, 101, "merge-waiting-for-review-threads", []string{"review-threads-unresolved"}, 0},
	} {
		w := waitFor(mergegate.Observation{Expected: mergegate.Expected{PullRequest: 11}, Reasons: c.reasons, UnresolvedThreads: c.threads})
		if w.State != c.state || w.PullRequest != 11 || strings.Join(w.Reasons, ",") != strings.Join(c.listed, ",") || w.UnresolvedThreads != c.count {
			t.Errorf("waitFor(%v) = %+v", c.reasons, w)
		}
	}
	states := map[string]bool{}
	for _, s := range WaitStates() {
		states[s] = true
	}
	if len(states) != len(mergegate.Reasons)+1 || !states["merge-waiting"] {
		t.Fatal("wait states", WaitStates())
	}
	for _, reason := range mergegate.Reasons {
		if !stopStages["gate-"+reason] {
			t.Error("gate reason without a stop stage", reason)
		}
		if !states[waitFor(mergegate.Observation{Reasons: []string{reason}}).State] {
			t.Error("gate reason without a wait state", reason)
		}
	}
	threads := mergegate.Observation{Reasons: []string{"native-merge-not-ready", "review-threads-unresolved"}}
	ready := mergegate.Observation{Ready: true, Reasons: []string{}}
	if got := gateChange(ready, threads); got != "gate-review-threads-unresolved" {
		t.Fatal("pre-send stop hid the thread blocker", got)
	}
}
