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
		})
	}
}
