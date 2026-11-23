package reviewflow

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelmailbox"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/podtransport"
)

type testSession struct {
	reads                int
	substitute, exitRace bool
	exit                 int32
}

func TestIndependentQueueScheduling(t *testing.T) {
	for _, test := range []struct {
		state string
		err   error
		want  bool
	}{
		{"", nil, true}, {"rejected", nil, true},
		{"stopped", ControllerError{Stage: "gateway-unavailable"}, true},
		{"stopped", ControllerError{Stage: "gateway-profile-unfit"}, true},
		{"stopped", ControllerError{Stage: "gateway-profile-unfit", CleanupPending: true}, false},
		{"review-passed", nil, true},
		{"stopped", ControllerError{Stage: "model"}, false},
		{"cleanup-uncertain", ControllerError{Stage: "cleanup", CleanupPending: true}, false},
		{"stopped", ControllerError{Stage: "gateway-unavailable", CleanupPending: true}, false},
		{"", errors.New("unclassified"), false},
	} {
		if got := VerificationMayProceed(test.state, test.err); got != test.want {
			t.Fatalf("state %q error %v: may proceed = %t", test.state, test.err, got)
		}
	}
}

func (s *testSession) WorkerExit(context.Context) (int32, bool, error) {
	return s.exit, s.reads >= 3, nil
}
func (s *testSession) ReadRequest(context.Context) (modelmailbox.Envelope, error) {
	s.reads++
	seq := uint64(1)
	if s.reads >= 3 {
		seq = 2
	}
	r := modelmailbox.Envelope{Sequence: seq, Payload: []byte("original")}
	if s.substitute && s.reads == 2 {
		r.Payload = []byte("substituted")
	}
	if s.exitRace && s.reads == 3 {
		return r, podtransport.ErrWorkerExited
	}
	return r, nil
}
func TestReviewDriveDeduplicatesOnlyExactHandoffs(t *testing.T) {
	for _, scenario := range []string{"success", "substitution", "exit-race", "wrong-exit", "revoked", "delivery-failed"} {
		t.Run(scenario, func(t *testing.T) {
			s := &testSession{exit: 10, substitute: scenario == "substitution", exitRace: scenario == "exit-race"}
			if scenario == "wrong-exit" {
				s.exit = 0
			}
			calls := 0
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			e := drive(ctx, s, func(context.Context) error {
				if scenario == "revoked" {
					return errors.New("revoked")
				}
				return nil
			}, func(modelmailbox.Envelope) error {
				calls++
				if scenario == "delivery-failed" {
					return errors.New("lost handoff")
				}
				return nil
			})
			if scenario == "success" {
				if e != nil || calls != 2 {
					t.Fatal(e, calls)
				}
			} else if e == nil {
				t.Fatal("unsafe review drive accepted")
			}
			if scenario == "delivery-failed" && calls != 1 {
				t.Fatal("uncertain delivery replayed")
			}
			// Each stop names its bounded drive step; a failed delivery keeps
			// the dispatcher's stage, so drive itself names none.
			want := map[string]string{"substitution": "drive-sequence", "exit-race": "drive-exit-race", "wrong-exit": "worker-exit-0", "revoked": "drive-authority"}[scenario]
			var stopped ControllerError
			if want != "" && (!errors.As(e, &stopped) || stopped.Stage != want) {
				t.Fatalf("%s: stop %v, want %s", scenario, e, want)
			}
			if scenario == "delivery-failed" && (errors.As(e, &stopped) || !errors.Is(e, ErrReview)) {
				t.Fatal("a failed delivery must keep the dispatcher's stage")
			}
			if want != "" && DiagnosticStage(ControllerError{Stage: "model-" + want}) != "model-"+want {
				t.Fatalf("%s: drive stop is not a bounded diagnostic", scenario)
			}
		})
	}
}

type stuckSession struct{ observeFails bool }

func (s stuckSession) WorkerExit(context.Context) (int32, bool, error) {
	if s.observeFails {
		return 0, false, errors.New("observation unavailable")
	}
	return 0, false, nil
}
func (stuckSession) ReadRequest(context.Context) (modelmailbox.Envelope, error) {
	return modelmailbox.Envelope{}, modelmailbox.ErrPending
}

func TestReviewDriveNamesObservationAndDeadlineStops(t *testing.T) {
	allow := func(context.Context) error { return nil }
	never := func(modelmailbox.Envelope) error { t.Fatal("nothing to deliver"); return nil }
	var stopped ControllerError
	if e := drive(context.Background(), stuckSession{observeFails: true}, allow, never); !errors.As(e, &stopped) || stopped.Stage != "drive-observe" {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if e := drive(ctx, stuckSession{}, allow, never); !errors.As(e, &stopped) || stopped.Stage != "drive-deadline" {
		t.Fatal(e)
	}
	for _, stage := range []string{"drive-observe", "drive-deadline"} {
		if DiagnosticStage(ControllerError{Stage: "model-" + stage}) != "model-"+stage {
			t.Fatal(stage)
		}
	}
}
