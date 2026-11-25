package intake

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/forge"
)

// An incomplete scan is named by a bounded step: where it stopped, the forge's
// status class, or the scan bound; never a body, URL or issue content.
func TestScanStepIsBounded(t *testing.T) {
	secret := errors.New("issue body: do not log")
	for _, tc := range []struct {
		err  error
		want string
	}{
		{nil, ""},
		{secret, "other"},
		{scanFailure("list", &forge.APIError{Status: 403}), "list-status-403"},
		{scanFailure("snapshot-issue", &forge.APIError{Status: 429}), "snapshot-issue-status-429"},
		{scanFailure("snapshot-base", &forge.APIError{Status: 502}), "snapshot-base-status-5xx"},
		{scanFailure("list", &forge.APIError{Status: 404}), "list-status-4xx"},
		{scanFailure("record", fmt.Errorf("wrapped: %w", context.DeadlineExceeded)), "record-timeout"},
		{scanFailure("snapshot-changed", secret), "snapshot-changed"},
		{scanFailure("pagination", errors.New("bound")), "pagination"},
		// The forge replaces a context error with its own text; PollOnce marks
		// a failure after the scan bound from the scan's context instead.
		{scanTimedOut{scanFailure("list", errors.New("GitHub transport unavailable"))}, "list-timeout"},
	} {
		if got := ScanStep(tc.err); got != tc.want {
			t.Errorf("ScanStep(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
	// The wrapper keeps the original error for callers and the backoff.
	var api *forge.APIError
	if !errors.As(scanFailure("list", &forge.APIError{Status: 429}), &api) || api.Status != 429 {
		t.Fatal("scan step hides the forge error")
	}
}

// An issue edited between the two authorization reads is named on its own,
// distinct from a transport failure (audit of #807).
func TestSnapshotNamesAChangedIssue(t *testing.T) {
	s := fakeSource()
	s.changed = true
	p := Poller{Config: config(), Source: s}
	if _, err := p.snapshot(context.Background(), 3); ScanStep(err) != "snapshot-changed" {
		t.Fatalf("changed issue named %q", ScanStep(err))
	}
}
