package main

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

func TestDeadlineBoundsStalledInputAndOutput(t *testing.T) {
	for _, mode := range []string{"input", "output"} {
		t.Run(mode, func(t *testing.T) {
			r, w := io.Pipe()
			defer r.Close()
			defer w.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			start := time.Now()
			err := bounded(ctx, func() error {
				if mode == "input" {
					_, err := io.ReadAll(io.LimitReader(r, 1024))
					return err
				}
				_, err := w.Write([]byte("bounded result"))
				return err
			})
			if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
				t.Fatal("I/O was not bounded", err)
			}
		})
	}
}
