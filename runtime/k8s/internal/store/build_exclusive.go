package store

import (
	"context"
	"database/sql/driver"
	"errors"
	"time"
)

// WithBuildController excludes other reconcilers for this database. It is not
// execution authority: every effect still requires its durable claim fences.
// The dedicated session never returns to the pool while holding its lock.
func (db *DB) WithBuildController(ctx context.Context, work func(context.Context) error) (acquired bool, err error) {
	if db == nil || db.Pool == nil || work == nil {
		return false, ErrExecution
	}
	acquire, cancelAcquire := context.WithTimeout(ctx, 3*time.Second)
	defer cancelAcquire()
	conn, e := db.Pool.Conn(acquire)
	if e != nil {
		return false, ErrExecution
	}
	defer conn.Close()
	if AssertComponent(acquire, conn, "build-controller") != nil {
		return false, ErrExecution
	}
	const lockID int64 = 0x414953444C434255
	if e = conn.QueryRowContext(acquire, `SELECT pg_try_advisory_lock($1)`, lockID).Scan(&acquired); e != nil {
		// The lock response may have been lost. Discard the physical session.
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		return false, ErrExecution
	}
	if !acquired {
		return false, nil
	}
	defer func() {
		unlock, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		var released bool
		if e := conn.QueryRowContext(unlock, `SELECT pg_advisory_unlock($1)`, lockID).Scan(&released); e != nil || !released {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			err = errors.Join(err, ErrExecution)
		}
	}()
	active, cancel := context.WithTimeout(ctx, 25*time.Minute)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-active.Done():
				return
			case <-ticker.C:
				probe, stop := context.WithTimeout(active, 2*time.Second)
				var owned bool
				e := conn.QueryRowContext(probe, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND pid=pg_backend_pid() AND classid=$1::oid AND objid=$2::oid AND objsubid=1 AND mode='ExclusiveLock' AND granted)`, lockID>>32, lockID&0xffffffff).Scan(&owned)
				stop()
				if e != nil || !owned {
					cancel()
					return
				}
			}
		}
	}()
	defer func() { cancel(); <-done }()
	err = work(active)
	if active.Err() != nil {
		err = errors.Join(err, active.Err())
	}
	return true, err
}
