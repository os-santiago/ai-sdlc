package executor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStartBoundExclusiveAndDeadline(t *testing.T) {
	expected := Start{"ai-sdlc.executor-start/v1", "12345678-1234-1234-1234-123456789abc", strings.Repeat("a", 64), strings.Repeat("b", 64)}
	parent := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	if WaitStart(ctx, parent, expected) == nil {
		t.Fatal("missing start accepted")
	}
	cancel()
	if PublishStart(parent, expected) != nil {
		t.Fatal("valid start rejected")
	}
	if PublishStart(parent, expected) == nil {
		t.Fatal("repeated start accepted")
	}
	if WaitStart(context.Background(), parent, expected) != nil {
		t.Fatal("matching start rejected")
	}
	expected.TreeSHA256 = strings.Repeat("c", 64)
	if WaitStart(context.Background(), parent, expected) == nil {
		t.Fatal("wrong tree accepted")
	}
	parent = t.TempDir()
	if os.Symlink("/etc/passwd", filepath.Join(parent, "start.json")) != nil {
		t.Fatal("link fixture")
	}
	if WaitStart(context.Background(), parent, expected) == nil {
		t.Fatal("link accepted")
	}
}
