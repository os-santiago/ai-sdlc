package executor

import (
	"context"
	"testing"
	"time"
)

func TestProcessBounds(t *testing.T) {
	for _, tc := range []struct {
		name, script string
		success      bool
	}{
		{"success", "printf ok", true},
		{"failure", "exit 7", false},
		{"output", "while :; do printf '%01000d' 0; done", false},
		{"deadline", "sleep 10", false},
		{"inherited-pipe", "sleep 10 & exit 0", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			started := time.Now()
			err := runBounded(ctx, "/bin/sh", t.TempDir(), []string{"PATH=/usr/bin:/bin"}, []string{"-c", tc.script})
			if (err == nil) != tc.success {
				t.Fatalf("unexpected result: %v", err)
			}
			if time.Since(started) > 2*time.Second {
				t.Fatal("subprocess exceeded external bound")
			}
		})
	}
}
