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

func TestCallWindowArgumentIsBoundedAndServeOnly(t *testing.T) {
	for _, args := range [][]string{
		{"serve", "--attempt", "a-1", "--pod-uid", "u", "--wait-seconds", "59"},
		{"serve", "--attempt", "a-1", "--pod-uid", "u", "--wait-seconds", "601"},
		{"read", "--attempt", "a-1", "--pod-uid", "u", "--wait-seconds", "120"},
	} {
		if err := run(context.Background(), args, nil, io.Discard); err == nil || err.Error() != "invalid mailbox call window" {
			t.Fatal("window accepted", args, err)
		}
	}
	err := run(context.Background(), []string{"serve", "--attempt", "a-1", "--pod-uid", "u", "--wait-seconds", "300"}, nil, io.Discard)
	if err != nil && err.Error() == "invalid mailbox call window" {
		t.Fatal("valid window refused")
	}
}
