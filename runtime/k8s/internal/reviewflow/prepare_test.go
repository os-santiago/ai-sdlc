package reviewflow

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/sourcefetch"
)

func TestPrepareStopNamesTheFailedStepWithoutLeakingDetail(t *testing.T) {
	var stop prepareStop
	if !errors.As(fmt.Errorf("wrapper: %w", prepareStop{"proof"}), &stop) || stop.step != "proof" {
		t.Fatal("failed preparation step not preserved")
	}
	for _, err := range []error{nil, ErrReview, errors.New("sensitive-fixture")} {
		if errors.As(err, &stop) {
			t.Fatal("unclassified rejection misnamed as a step")
		}
	}
	if (prepareStop{"authority"}).Error() != ErrReview.Error() {
		t.Fatal("preparation stop leaked step detail")
	}
}

func TestPrepareSourceFetchNamesTheExpiredWait(t *testing.T) {
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		err  error
		want string
	}{
		{"deadline", expired, sourcefetch.ErrSource, "source-deadline"},
		{"unavailable", context.Background(), sourcefetch.ErrSource, "source-unavailable"},
		{"unclassified", context.Background(), errors.New("sensitive-fixture"), "source-unavailable"},
	} {
		if got := sourceFetchStep(tc.ctx, tc.err); got != tc.want {
			t.Fatalf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}
