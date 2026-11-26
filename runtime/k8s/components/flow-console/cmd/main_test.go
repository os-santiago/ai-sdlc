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
		if calls > 0 {
			readyAfter = append(readyAfter, ready.Load())
		}
		if calls == len(outcomes) {
			cancel()
			close(done)
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
