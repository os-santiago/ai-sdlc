package executor

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

func compiledFixture(t *testing.T, maxSeconds int) ([]byte, []byte, map[string]any) {
	t.Helper()
	now := time.Now()
	adev := []byte("reviewed profile fixture")
	p := profileFixture()
	p.MaxSeconds, p.ADevSHA256 = maxSeconds, workspacebundle.Digest(adev)
	profile, _ := json.Marshal(p)
	a := assignmentFixture(now)
	a.ProfileSHA256 = workspacebundle.Digest(profile)
	assignment, _ := json.Marshal(a)
	image := "quay.io/os-santiago/ai-sdlc@sha256:" + strings.Repeat("a", 64)
	workload, err := Compile("fixture", image, image, assignment, profile, adev, now)
	if err != nil {
		t.Fatal(err)
	}
	return assignment, profile, workload.Pod["spec"].(map[string]any)
}

// The compiled executor Pod keeps activeDeadlineSeconds 1020 at the default
// budget and grows with a larger reviewed budget.
func TestCompiledPodDeadlineFollowsTheGenerationBudget(t *testing.T) {
	for seconds, want := range map[int]int{60: 1020, 600: 1020, 900: 1320} {
		if _, _, spec := compiledFixture(t, seconds); fmt.Sprint(spec["activeDeadlineSeconds"]) != fmt.Sprint(want) {
			t.Fatal("pod deadline", seconds, spec["activeDeadlineSeconds"])
		}
	}
}
