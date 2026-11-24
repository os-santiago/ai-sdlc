package executor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

// The launcher derives its absolute bound from the projected, digest-bound
// profile and keeps the original 980 s for any input it cannot verify.
func TestProjectedProcessBoundFollowsTheReviewedProfile(t *testing.T) {
	for seconds, want := range map[int]int{600: 980, 900: 1280} {
		assignment, profile, _ := compiledFixture(t, seconds)
		dir := t.TempDir()
		if os.WriteFile(filepath.Join(dir, "assignment.json"), assignment, 0600) != nil || os.WriteFile(filepath.Join(dir, "profile.json"), profile, 0600) != nil {
			t.Fatal("fixture")
		}
		digest := workspacebundle.Digest(assignment)
		if got := ProjectedProcessSeconds(dir, digest, time.Now()); got != want {
			t.Fatal("process bound", seconds, got)
		}
		if ProjectedProcessSeconds(dir, strings.Repeat("0", 64), time.Now()) != DefaultProcessSeconds {
			t.Fatal("unbound assignment extended the bound")
		}
		if os.WriteFile(filepath.Join(dir, "profile.json"), append(profile, ' '), 0600) != nil || ProjectedProcessSeconds(dir, digest, time.Now()) != DefaultProcessSeconds {
			t.Fatal("unbound profile extended the bound")
		}
		if ProjectedProcessSeconds(t.TempDir(), digest, time.Now()) != DefaultProcessSeconds {
			t.Fatal("missing input extended the bound")
		}
	}
}
