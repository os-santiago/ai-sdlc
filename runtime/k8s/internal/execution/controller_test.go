package execution

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelmailbox"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/podtransport"
)

type driverSession struct {
	requests    []modelmailbox.Envelope
	read        int
	exitAfter   int
	code        int32
	readErr     error
	errorOnRead int
}

func (s *driverSession) WorkerExit(context.Context) (int32, bool, error) {
	return s.code, s.exitAfter >= 0 && s.read >= s.exitAfter, nil
}
func (s *driverSession) ReadRequest(context.Context) (modelmailbox.Envelope, error) {
	s.read++
	if s.readErr != nil && (s.errorOnRead == 0 || s.read == s.errorOnRead) {
		if s.read <= len(s.requests) {
			return s.requests[s.read-1], s.readErr
		}
		return modelmailbox.Envelope{}, s.readErr
	}
	if s.read > len(s.requests) {
		return modelmailbox.Envelope{}, modelmailbox.ErrPending
	}
	return s.requests[s.read-1], nil
}
func req(sequence uint64) modelmailbox.Envelope {
	return modelmailbox.Envelope{Sequence: sequence, Payload: []byte("bounded data")}
}

func TestControllerDrivesVariableTurnsAndExactDuplicates(t *testing.T) {
	for _, count := range []int{1, 2, 4} {
		s := &driverSession{exitAfter: count + 1}
		for i := 1; i <= count; i++ {
			s.requests = append(s.requests, req(uint64(i)))
		}
		// Last request remains visible until the worker consumes its reply/exits.
		s.requests = append(s.requests, req(uint64(count)))
		calls := 0
		err := driveModel(context.Background(), s, func(context.Context) error { return nil }, func(context.Context, modelmailbox.Envelope) error { calls++; return nil })
		if err != nil || calls != count {
			t.Fatalf("turns %d: calls=%d err=%v", count, calls, err)
		}
	}
}
func TestControllerRejectsSequenceDriftAndNonzeroExit(t *testing.T) {
	changed := req(1)
	changed.Payload = []byte("substitution")
	for name, s := range map[string]*driverSession{
		"sequence gap":                      {requests: []modelmailbox.Envelope{req(2)}, exitAfter: 1},
		"duplicate drift":                   {requests: []modelmailbox.Envelope{req(1), changed}, exitAfter: 2},
		"nonzero":                           {exitAfter: 0, code: 7},
		"permission during exit":            {exitAfter: 1, readErr: podtransport.ErrPermission},
		"protocol failure before zero exit": {exitAfter: 1, readErr: podtransport.ErrTransport},
		"transport while running":           {exitAfter: -1, readErr: podtransport.ErrTransport},
	} {
		t.Run(name, func(t *testing.T) {
			if driveModel(context.Background(), s, func(context.Context) error { return nil }, func(context.Context, modelmailbox.Envelope) error { return nil }) == nil {
				t.Fatal("accepted invalid execution")
			}
		})
	}
}
func TestControllerHandlesOriginalWorkerExitRace(t *testing.T) {
	s := &driverSession{exitAfter: 1, readErr: podtransport.ErrWorkerExited}
	if driveModel(context.Background(), s, func(context.Context) error { return nil }, func(context.Context, modelmailbox.Envelope) error { t.Fatal("unexpected dispatch"); return nil }) != nil {
		t.Fatal("original zero exit not observed")
	}
}

func TestControllerPreservesNonzeroCodeAcrossMailboxExitRace(t *testing.T) {
	for _, session := range []*driverSession{{exitAfter: 0, code: 10}, {exitAfter: 1, code: 10, readErr: podtransport.ErrWorkerExited}} {
		err := driveModel(context.Background(), session, func(context.Context) error { return nil }, func(context.Context, modelmailbox.Envelope) error { t.Fatal("failure dispatched new work"); return nil })
		var failed nonzeroWorkerExit
		if !errors.Is(err, errWorkerNonzero) || !errors.As(err, &failed) || failed.code != 10 {
			t.Fatal("original nonzero code lost")
		}
	}
}
func TestControllerStopsIdleOnRevocationAndCancellation(t *testing.T) {
	s := &driverSession{exitAfter: -1}
	proofs := 0
	if driveModel(context.Background(), s, func(context.Context) error {
		proofs++
		if proofs == 2 {
			return ErrAuthority
		}
		return nil
	}, func(context.Context, modelmailbox.Envelope) error { t.Fatal("unexpected dispatch"); return nil }) == nil || proofs != 2 {
		t.Fatal("idle authority not checked")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if driveModel(ctx, s, func(context.Context) error { return nil }, nil) == nil || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatal("idle cancellation ignored")
	}
}
func TestControllerDoesNotReplayUncertainDelivery(t *testing.T) {
	s := &driverSession{requests: []modelmailbox.Envelope{req(1), req(1)}, exitAfter: 2}
	calls := 0
	if driveModel(context.Background(), s, func(context.Context) error { return nil }, func(context.Context, modelmailbox.Envelope) error { calls++; return errors.New("uncertain") }) == nil || calls != 1 {
		t.Fatal("uncertain call was retried")
	}
}

func TestControllerExitCannotHideObservedRequestDrift(t *testing.T) {
	changed := req(1)
	changed.Payload = []byte("different")
	for name, last := range map[string]modelmailbox.Envelope{"exact duplicate": req(1), "new unhanded": req(2), "gap": req(3), "substituted duplicate": changed} {
		t.Run(name, func(t *testing.T) {
			s := &driverSession{requests: []modelmailbox.Envelope{req(1), last}, exitAfter: 2, readErr: podtransport.ErrWorkerExited, errorOnRead: 2}
			calls := 0
			err := driveModel(context.Background(), s, func(context.Context) error { return nil }, func(context.Context, modelmailbox.Envelope) error { calls++; return nil })
			if calls != 1 || ((err == nil) != (name == "exact duplicate")) {
				t.Fatalf("exit hid observed request: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestControllerNamesEachDriveStop(t *testing.T) {
	for want, s := range map[string]*driverSession{
		"execution-sequence": {requests: []modelmailbox.Envelope{req(2)}, exitAfter: 1},
		"execution-read":     {exitAfter: -1, readErr: podtransport.ErrTransport},
	} {
		err := driveModel(context.Background(), s, func(context.Context) error { return nil }, func(context.Context, modelmailbox.Envelope) error { return nil })
		var stopped driveStop
		if !errors.Is(err, ErrAuthority) || !errors.As(err, &stopped) || stopped.stage != want || DiagnosticStage(ControllerError{stopped.stage}) != want {
			t.Fatalf("%s: err=%v", want, err)
		}
	}
	err := driveModel(context.Background(), &driverSession{exitAfter: -1}, func(context.Context) error { return ErrAuthority }, nil)
	var stopped driveStop
	if !errors.As(err, &stopped) || stopped.stage != "execution-authority" {
		t.Fatalf("authority: err=%v", err)
	}
}
