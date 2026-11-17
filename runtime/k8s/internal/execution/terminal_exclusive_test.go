package execution

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestTerminalControllerExcludesReplicaAndReleasesSession(t *testing.T) {
	b := newTerminalFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, e := b.j.DB.WithTerminalController(ctx, func(context.Context) error { close(entered); <-release; return nil })
		done <- e
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("controller did not acquire")
	}
	var calls atomic.Int32
	callback := func(context.Context) error { calls.Add(1); return nil }
	acquired, e := b.j.DB.WithTerminalController(ctx, callback)
	close(release)
	if acquired || e != nil || calls.Load() != 0 {
		t.Fatal("competing replica entered", e)
	}
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	if acquired, e = b.j.DB.WithTerminalController(ctx, callback); !acquired || e != nil || calls.Load() != 1 {
		t.Fatal("session lock leaked", e)
	}
	if acquired, e = b.f.p.b.f.Admin.WithTerminalController(ctx, callback); acquired || e == nil {
		t.Fatal("admin masqueraded as component")
	}
}

func TestTerminalControllerCancelsWhenOriginalBackendDies(t *testing.T) {
	b := newTerminalFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	entered := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, e := b.j.DB.WithTerminalController(ctx, func(active context.Context) error {
			close(entered)
			<-active.Done()
			return active.Err()
		})
		done <- e
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("controller did not acquire")
	}
	const lockID int64 = 0x414953444C435452
	var terminated bool
	e := b.f.p.b.f.Admin.Pool.QueryRowContext(ctx, `SELECT pg_terminate_backend(pid) FROM pg_locks WHERE locktype='advisory' AND classid=$1::oid AND objid=$2::oid AND objsubid=1 AND granted`, lockID>>32, lockID&0xffffffff).Scan(&terminated)
	if e != nil || !terminated {
		t.Fatal("isolated backend was not terminated", e)
	}
	select {
	case e := <-done:
		if e == nil {
			t.Fatal("lost backend accepted")
		}
	case <-ctx.Done():
		t.Fatal("lost session did not cancel work")
	}
	if acquired, e := b.j.DB.WithTerminalController(ctx, func(context.Context) error { return nil }); !acquired || e != nil {
		t.Fatal("lost session returned locked connection", e)
	}
}
