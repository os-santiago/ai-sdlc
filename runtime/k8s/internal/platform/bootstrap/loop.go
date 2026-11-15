package bootstrap

import (
	"context"
	"time"
)

// Periodic starts one sequential, cancelable reconciler. Call its returned
// stop function before closing dependencies. Every callback must bound I/O.
func Periodic(interval time.Duration, work func(context.Context)) func() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if ctx.Err() != nil {
				return
			}
			work(ctx)
			timer := time.NewTimer(interval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
	return func() { cancel(); <-done }
}
