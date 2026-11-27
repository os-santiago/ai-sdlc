package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/flowconsole"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

func TestPollJournalLatchesReadinessAndLogsOnlyTransitions(t *testing.T) {
	// fail, fail, ok, ok, fail, fail, ok: two failures before the first read.
	outcomes := []bool{false, false, true, true, false, false, true}
	var mu sync.Mutex
	var logs []string
	var readyAfter []bool
	var ready atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	done := make(chan struct{})
	poll := func(context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		if calls > 0 && ctx.Err() == nil {
			readyAfter = append(readyAfter, ready.Load())
		}
		if calls == len(outcomes) {
			// A tick already pending may call poll once more before the loop
			// sees the cancellation; close done only once.
			if ctx.Err() == nil {
				cancel()
				close(done)
			}
			return nil
		}
		ok := outcomes[calls]
		calls++
		if ok {
			return nil
		}
		return errors.New("password=secret driver detail")
	}
	logf := func(args ...any) {
		mu.Lock()
		logs = append(logs, fmt.Sprint(args...))
		mu.Unlock()
	}
	go pollJournal(ctx, poll, time.Millisecond, &ready, logf)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("poll loop stalled")
	}
	mu.Lock()
	defer mu.Unlock()
	// Not ready until the first success; ready ever after, through the outage.
	if fmt.Sprint(readyAfter) != "[false false true true true true true]" {
		t.Fatal("readiness", readyAfter)
	}
	want := "flow journal read failed|flow journal read restored|flow journal read failed|flow journal read restored"
	if strings.Join(logs, "|") != want {
		t.Fatal("logs", logs)
	}
	for _, line := range logs {
		if strings.Contains(line, "secret") || strings.Contains(line, "driver") {
			t.Fatal("driver error logged")
		}
	}
}

func TestPollJournalStopsWithItsContext(t *testing.T) {
	var ready atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		pollJournal(ctx, func(context.Context) error { return nil }, time.Hour, &ready, func(...any) {})
		close(stopped)
	}()
	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("poll loop ignored cancellation")
	}
	if !ready.Load() {
		t.Fatal("successful read did not mark ready")
	}
}

func TestBackoffDoublesUpToItsBound(t *testing.T) {
	b := backoff{Initial: time.Second, Max: 30 * time.Second}
	var got []time.Duration
	d := time.Duration(0)
	for range 7 {
		d = b.next(d)
		got = append(got, d)
	}
	if fmt.Sprint(got) != "[1s 2s 4s 8s 16s 30s 30s]" {
		t.Fatal("schedule", got)
	}
}

// A fresh install starts the console before PostgreSQL (or the console's
// role) is ready. It must wait and retry, reporting that it is connecting,
// instead of exiting with "journal database unavailable".
func TestConnectJournalRetriesUntilTheDatabaseIsReachable(t *testing.T) {
	hub := flowconsole.NewHub(100, "journal")
	var logs []string
	opens, asserts := 0, 0
	var statuses []string
	open := func(context.Context) (*store.DB, error) {
		opens++
		statuses = append(statuses, hub.Status().Problem)
		if opens <= 3 {
			return nil, errors.New("dial tcp: connection refused password=secret")
		}
		return &store.DB{}, nil
	}
	assert := func(context.Context, *store.DB) error {
		asserts++
		if asserts == 1 {
			// PostgreSQL accepted the session but the role check could not
			// be read: still not ready, retried.
			return store.ErrObservationUnavailable
		}
		return nil
	}
	db, err := connectJournal(context.Background(), open, assert, hub, time.Hour, backoff{Initial: time.Millisecond, Max: 4 * time.Millisecond},
		func(args ...any) { logs = append(logs, fmt.Sprint(args...)) })
	if err != nil || db == nil {
		t.Fatal("not connected", err)
	}
	if opens != 5 || asserts != 2 {
		t.Fatal("attempts", opens, asserts)
	}
	if fmt.Sprint(statuses[1:]) != "[journal-connecting journal-connecting journal-connecting journal-connecting]" {
		t.Fatal("status while waiting", statuses)
	}
	if s := hub.Status(); s.Healthy || s.Problem != flowconsole.ProblemConnecting || s.Window != "1h0m0s" {
		t.Fatal("connecting status", s)
	}
	if len(logs) != 2 || !strings.Contains(logs[0], "retrying") || !strings.Contains(logs[1], "reachable after 5 attempts") {
		t.Fatal("logs", logs)
	}
	for _, line := range logs {
		if strings.Contains(line, "secret") || strings.Contains(line, "refused") {
			t.Fatal("driver error logged")
		}
	}
}

// Any identity other than the dedicated read-only role stays fatal at once.
func TestConnectJournalRefusesAnotherIdentityWithoutRetrying(t *testing.T) {
	opens := 0
	_, err := connectJournal(context.Background(), func(context.Context) (*store.DB, error) { opens++; return &store.DB{}, nil },
		func(context.Context, *store.DB) error { return errors.New("database component identity mismatch") },
		flowconsole.NewHub(100, "journal"), time.Hour, backoff{Initial: time.Millisecond, Max: time.Millisecond}, func(...any) {})
	if err == nil || err.Error() != "flow console requires its dedicated read-only database role" || opens != 1 {
		t.Fatal("identity", err, opens)
	}
}

func TestConnectJournalStopsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := connectJournal(ctx, func(context.Context) (*store.DB, error) { return nil, errors.New("unreachable") },
			func(context.Context, *store.DB) error { return nil }, flowconsole.NewHub(100, "journal"), time.Hour, backoff{Initial: time.Hour, Max: time.Hour}, func(...any) {})
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("connect loop ignored cancellation")
	}
}
