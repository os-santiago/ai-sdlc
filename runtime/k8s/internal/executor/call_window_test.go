package executor

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

func profileRaw(t *testing.T, mutate func(map[string]any)) []byte {
	t.Helper()
	p := profileFixture()
	p.MaxSeconds = 600
	raw, _ := json.Marshal(p)
	var doc map[string]any
	_ = json.Unmarshal(raw, &doc)
	delete(doc, "maxRepairs")
	mutate(doc)
	out, _ := json.Marshal(doc)
	return out
}

// The per-call window is optional reviewed profile data: omitted keeps the
// original 60 s window and byte-identical workloads.
func TestCallWindowIsOptionalAndBounded(t *testing.T) {
	raw := profileRaw(t, func(map[string]any) {})
	p, err := ParseProfile(raw, workspacebundle.Digest(raw))
	if err != nil || p.CallSeconds != 0 || p.CallWindow() != time.Minute {
		t.Fatal("default window", p, err)
	}
	raw = profileRaw(t, func(d map[string]any) { d["callSeconds"] = 300 })
	if p, err = ParseProfile(raw, workspacebundle.Digest(raw)); err != nil || p.CallWindow() != 300*time.Second {
		t.Fatal("selected window", p, err)
	}
	for _, bad := range []any{0, 59, 601, 700, "300", nil} {
		raw = profileRaw(t, func(d map[string]any) { d["callSeconds"] = bad; d["maxSeconds"] = 900 })
		if _, err = ParseProfile(raw, workspacebundle.Digest(raw)); err == nil {
			t.Fatal("accepted window", bad)
		}
	}
	raw = profileRaw(t, func(d map[string]any) { d["callSeconds"] = 300; d["maxSeconds"] = 200 })
	if _, err = ParseProfile(raw, workspacebundle.Digest(raw)); err == nil {
		t.Fatal("window beyond the run budget accepted")
	}
}

func TestCallWindowReachesSCCAndTheMailbox(t *testing.T) {
	now := time.Now()
	adev := []byte("reviewed profile fixture")
	image := "quay.io/os-santiago/ai-sdlc@sha256:" + strings.Repeat("a", 64)
	compile := func(window int) (map[string]any, []string) {
		p := profileFixture()
		p.MaxSeconds, p.CallSeconds, p.ADevSHA256 = 600, window, workspacebundle.Digest(adev)
		profile, _ := json.Marshal(p)
		a := assignmentFixture(now)
		a.ProfileSHA256 = workspacebundle.Digest(profile)
		assignment, _ := json.Marshal(a)
		w, err := Compile("fixture", image, image, DefaultRuntimeClass, assignment, profile, adev, now)
		if err != nil {
			t.Fatal(err)
		}
		var scc map[string]any
		_ = json.Unmarshal([]byte(w.ConfigMap["data"].(map[string]any)["scc-config.json"].(string)), &scc)
		mailbox := w.Pod["spec"].(map[string]any)["containers"].([]any)[1].(map[string]any)
		var command []string
		for _, arg := range mailbox["command"].([]any) {
			command = append(command, arg.(string))
		}
		return scc["model"].(map[string]any), command
	}
	model, command := compile(0)
	if _, ok := model["timeout"]; ok || slices.Contains(command, "--wait-seconds") {
		t.Fatal("default workload changed")
	}
	model, command = compile(300)
	if model["timeout"] != float64(315000) || !slices.Equal(command[len(command)-2:], []string{"--wait-seconds", "300"}) {
		t.Fatal("window not propagated", model, command)
	}
}

// SCC ends gracefully before the launcher's kill when the reviewed budget
// exceeds the 600 s default: it checks its time budget only before a model
// call, so it stops one call window (plus its 15 s wait slack) early. The margin
// comes only from seconds above 600 and is capped at an eighth of the budget,
// so the default profile keeps exactly 600 s and no budget gives SCC less. The
// launcher still kills at maxSeconds, and reviewers keep their original budget.
func TestSCCTimeBudgetEndsBeforeTheLauncherKill(t *testing.T) {
	for _, tc := range []struct {
		maxSeconds, callSeconds, want int
	}{
		{600, 0, 600},   // default profile: unchanged from the fixed budget
		{600, 330, 600}, // a slow model at the default budget: unchanged
		{650, 0, 600},   // only the 50 s above 600 can become margin
		{700, 0, 625},   // 60 s window + 15 s
		{900, 0, 825},   // the largest installable budget with the default window
		{900, 330, 788}, // a slow model's window is capped at an eighth (112 s)
		{180, 0, 180},   // short budgets (tests, a repair's remaining time) keep all of it
		{10, 0, 10},
	} {
		p := profileFixture()
		p.MaxSeconds, p.CallSeconds = tc.maxSeconds, tc.callSeconds
		got := SCCSeconds(p)
		if got != tc.want || got > p.MaxSeconds || got < min(p.MaxSeconds, BaseSeconds) {
			t.Fatal("SCC budget", tc, got)
		}
		args := strings.Join(sccArguments(p, "/state/run/prompt.md", "/state"), " ")
		if !strings.Contains(args, "--max-seconds "+strconv.Itoa(tc.want)+" ") || !strings.Contains(args, "--max-steps "+strconv.Itoa(p.MaxSteps)) {
			t.Fatal("SCC arguments", args)
		}
	}
	p := profileFixture()
	p.Version, p.MaxSeconds = ReviewProfileVersion, 180
	if SCCSeconds(p) != 180 || !strings.Contains(strings.Join(sccArguments(p, "/review-state/run/prompt.md", "/review-state"), " "), "--max-seconds 180 ") {
		t.Fatal("reviewer budget changed")
	}
}

// The Pod deadline and the launcher's absolute bound keep their original
// values at the default budget and grow second for second above it, so the
// Pod keeps 300 s beyond the lifecycle claim and the launcher keeps 380 s
// beyond maxSeconds for preparation, ending 40 s before the Pod deadline.
func TestExecutorBoundsGrowWithTheGenerationBudget(t *testing.T) {
	for _, tc := range []struct{ maxSeconds, pod, process int }{
		{45, 1020, 980}, {180, 1020, 980}, {600, 1020, 980}, {750, 1170, 1130}, {900, 1320, 1280},
	} {
		p := profileFixture()
		p.MaxSeconds = tc.maxSeconds
		pod, process := PodDeadlineSeconds(p), ProcessSeconds(p)
		if pod != tc.pod || process != tc.process {
			t.Fatal("bounds", tc, pod, process)
		}
		if tc.maxSeconds >= BaseSeconds && (pod-(p.MaxSeconds+120) != 300 || process-p.MaxSeconds != 380) || pod-process != 40 {
			t.Fatal("margins", tc)
		}
	}
	p := profileFixture()
	p.Version, p.MaxSeconds = ReviewProfileVersion, 180
	if PodDeadlineSeconds(p) != 1020 || ProcessSeconds(p) != 980 {
		t.Fatal("reviewer bounds changed")
	}
}
