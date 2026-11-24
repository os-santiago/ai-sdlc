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
		w, err := Compile("fixture", image, image, assignment, profile, adev, now)
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

// SCC ends gracefully before the launcher's kill: it checks its time budget
// only before a model call, so it stops one call window (plus its 15 s wait
// slack) early, capped at an eighth of the budget. The launcher still kills
// at maxSeconds, and reviewers keep their original budget.
func TestSCCTimeBudgetEndsBeforeTheLauncherKill(t *testing.T) {
	for _, tc := range []struct {
		maxSeconds, callSeconds, want int
	}{
		{600, 0, 525},   // default profile: 60 s window + 15 s, a full call fits
		{900, 0, 825},   // the largest installable budget with the default window
		{600, 330, 525}, // a slow model's window is capped at an eighth (75 s)
		{900, 330, 788}, // 112 s margin at 900 s
		{10, 0, 9},      // tiny budgets keep almost all of their time
	} {
		p := profileFixture()
		p.MaxSeconds, p.CallSeconds = tc.maxSeconds, tc.callSeconds
		got := SCCSeconds(p)
		if got != tc.want || got >= p.MaxSeconds {
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
