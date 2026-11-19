package executor

import (
	"encoding/json"
	"slices"
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
