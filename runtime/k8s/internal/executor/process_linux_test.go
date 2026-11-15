package executor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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

func TestProcessKillsDescendantAfterDirectChildExit(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := runBounded(ctx, "/bin/sh", dir, []string{"PATH=/usr/bin:/bin"},
		[]string{"-c", "sleep 10 & echo $! > descendant.pid; exit 0"})
	if err == nil {
		t.Fatal("inherited pipe unexpectedly completed without its bound")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "descendant.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 1 {
		t.Fatal("child did not publish its PID")
	}
	// Only this fixture's child is cleaned up if the group-kill regression
	// leaves it running. A zombie has terminated and must not be mistaken alive.
	deadline := time.Now().Add(time.Second)
	for {
		stat, readErr := os.ReadFile(fmtProcStat(pid))
		if errors.Is(readErr, os.ErrNotExist) {
			return
		}
		if readErr != nil {
			t.Fatal(readErr)
		}
		fields := strings.Fields(string(stat)[strings.LastIndexByte(string(stat), ')')+1:])
		if len(fields) > 0 && (fields[0] == "Z" || fields[0] == "X") {
			return
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatal("descendant survived the bounded subprocess")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func fmtProcStat(pid int) string { return "/proc/" + strconv.Itoa(pid) + "/stat" }
