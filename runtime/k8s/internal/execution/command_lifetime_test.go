package execution

import (
	"strings"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

// The default generation budget keeps the original 20-minute command; a
// larger reviewed budget extends it second for second, bounded at 900 s.
func TestCommandLifetimeKeepsHandoffRoomForLongerGeneration(t *testing.T) {
	for _, tc := range []struct {
		seconds int
		want    time.Duration
	}{{1, 20 * time.Minute}, {180, 20 * time.Minute}, {600, 20 * time.Minute}, {750, 22*time.Minute + 30*time.Second}, {900, 25 * time.Minute}, {5000, 25 * time.Minute}} {
		got := CommandLifetime(tc.seconds, 0)
		if got != tc.want {
			t.Fatal("lifetime", tc.seconds, got)
		}
		// Queueing, preparation and handoff keep at least the original 480 s
		// beyond the lifecycle claim (maxSeconds + 120 s, at most 1,020 s).
		if claim := time.Duration(min(tc.seconds, 900)+120) * time.Second; got-claim < 480*time.Second {
			t.Fatal("handoff room", tc.seconds, got-claim)
		}
		if got > time.Hour {
			t.Fatal("beyond the command verification bound")
		}
	}
}

// A generation toolchain extends the lifecycle claim by its start
// (maxSeconds + 120 s + 180 s); the command grows by exactly as much, so the
// handoff keeps 480 s, and the longest command stays under the one-hour
// bound of every command verifier (execution, repair and assignment).
func TestCommandLifetimeCoversTheToolchainStart(t *testing.T) {
	for _, tc := range []struct {
		seconds, start int
		want           time.Duration
	}{
		{600, 0, 20 * time.Minute},
		{600, executor.ToolchainStartSeconds, 23 * time.Minute},
		{900, 0, 25 * time.Minute},
		{900, executor.ToolchainStartSeconds, 28 * time.Minute},
		{900, store.MaxStartSeconds + 600, 30 * time.Minute},
		{600, -5, 20 * time.Minute},
	} {
		got := CommandLifetime(tc.seconds, tc.start)
		if got != tc.want {
			t.Fatal("lifetime", tc.seconds, tc.start, got)
		}
		start := max(0, min(tc.start, store.MaxStartSeconds))
		claim := time.Duration(min(tc.seconds, 900)+120+start) * time.Second
		if got-claim != 480*time.Second || got > time.Hour {
			t.Fatal("handoff room or verifier bound", tc.seconds, tc.start, got-claim)
		}
	}
}

func TestStartSecondsFollowTheInstalledToolchain(t *testing.T) {
	t.Cleanup(func() { project.SetInstalled(nil) })
	m := project.Manifest{Version: project.ManifestVersion, Repository: "example/service", BaseBranch: "main",
		Verification: &project.Verification{Image: "registry.example/go@sha256:" + strings.Repeat("a", 64), Command: []string{"go", "test", "./..."},
			TimeoutSeconds: 60, CPU: "1", Memory: "1Gi", EphemeralStorage: "1Gi", Network: "none", Result: project.Result{ExitCode: true},
			Repeat: 1, TestGlobs: []string{"**/*_test.go"}}}
	project.SetInstalled(&m)
	if StartSecondsFor("example/service", m.Digest()) != 0 || StartSecondsFor(project.Homedir, "") != 0 {
		t.Fatal("start without a toolchain")
	}
	m.Generation = &project.Generation{Toolchain: &project.Toolchain{Root: "/usr/local/go", Path: []string{"bin"}}}
	project.SetInstalled(&m)
	if StartSecondsFor("example/service", m.Digest()) != executor.ToolchainStartSeconds || StartSecondsFor("example/other", m.Digest()) != 0 {
		t.Fatal("start with a toolchain")
	}
	// A command signed with that lifetime passes the assignment bound.
	expires := time.Now().Add(CommandLifetime(900, StartSecondsFor("example/service", m.Digest())))
	if expires.After(time.Now().Add(time.Hour)) {
		t.Fatal("command beyond the assignment bound")
	}
}
