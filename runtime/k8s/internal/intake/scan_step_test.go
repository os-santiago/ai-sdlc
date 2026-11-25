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
		{scanFailure("snapshot", &forge.APIError{Status: 429}), "snapshot-status-429"},
		{scanFailure("snapshot", &forge.APIError{Status: 502}), "snapshot-status-5xx"},
		{scanFailure("list", &forge.APIError{Status: 404}), "list-status-4xx"},
		{scanFailure("record", fmt.Errorf("wrapped: %w", context.DeadlineExceeded)), "record-timeout"},
		{scanFailure("snapshot", secret), "snapshot"},
		{scanFailure("pagination", errors.New("bound")), "pagination"},
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
