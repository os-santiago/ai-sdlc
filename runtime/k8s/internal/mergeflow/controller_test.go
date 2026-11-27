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
