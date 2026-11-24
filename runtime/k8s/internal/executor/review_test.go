package executor

import (
	"encoding/json"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
	"strings"
	"testing"
	"time"
)

func TestReviewerCannotUseGenerationAuthorityOrWritableSource(t *testing.T) {
	p := profileFixture()
	p.Version = ReviewProfileVersion
	adev := []byte("review doctrine")
	p.ADevSHA256 = workspacebundle.Digest(adev)
	raw, _ := json.Marshal(p)
	if _, e := ParseProfile(raw, workspacebundle.Digest(raw)); e == nil {
		t.Fatal("review profile accepted as generation")
	}
	if _, e := ParseReviewProfile(raw, workspacebundle.Digest(raw)); e != nil {
		t.Fatal(e)
	}
	a := assignmentFixture(time.Now())
	a.ProfileSHA256 = workspacebundle.Digest(raw)
	assignment, _ := json.Marshal(a)
	image := "localhost/ai-sdlc-worker@sha256:" + strings.Repeat("a", 64)
	w, e := Compile("fixture", image, image, DefaultRuntimeClass, assignment, raw, adev, time.Now())
	if e != nil {
		t.Fatal(e)
	}
	cs := w.Pod["spec"].(map[string]any)["containers"].([]any)
	for i, c := range cs {
		mounts := c.(map[string]any)["volumeMounts"].([]any)
		seen := map[string]bool{}
		for _, m := range mounts {
			v := m.(map[string]any)
			name := v["name"].(string)
			seen[name] = true
			if i == 0 && name == "workspace" && v["readOnly"] != true {
				t.Fatal("reviewer writes source")
			}
			if i == 1 && name == "review-state" && v["readOnly"] != true {
				t.Fatal("mailbox writes reviewer state")
			}
		}
		if !seen["workspace"] || !seen["review-state"] || i == 0 && seen["bridge"] {
			t.Fatal("review mount boundary")
		}
	}
	var cfg map[string]any
	if json.Unmarshal(SCCConfiguration(p, nil), &cfg) != nil {
		t.Fatal("config")
	}
	permissions := cfg["permissions"].(map[string]any)
	if cfg["roles"].(map[string]any)["reviewer"] != p.Model || len(permissions["autoApprove"].([]any)) != 4 || permissions["denyCommands"].([]any)[0] != "*" || permissions["denyGitMutation"] != true {
		t.Fatal("review restrictions missing")
	}
	args := strings.Join(sccArguments(p, "/review-state/run/prompt.md", "/review-state"), " ")
	if strings.Contains(args, "-y") || !strings.Contains(args, "--role reviewer") || !strings.Contains(args, "--summary-file /review-state/run/summary.json") {
		t.Fatal("review arguments")
	}
	for _, change := range []func(*Profile){func(p *Profile) { p.MaxSteps = 9 }, func(p *Profile) { p.MaxSeconds = 181 }} {
		q := p
		change(&q)
		raw, _ := json.Marshal(q)
		if _, e := ParseReviewProfile(raw, workspacebundle.Digest(raw)); e == nil {
			t.Fatal("review budget exceeded")
		}
	}
}

// Repository commands run by the executor must not leave bytecode caches in
// the workspace: the candidate delta is the workspace. SCC passes run_shell
// children only allowlisted names, so the name must be allowlisted too.
func TestExecutorCommandsDoNotWriteBytecode(t *testing.T) {
	var cfg map[string]any
	if json.Unmarshal(SCCConfiguration(profileFixture(), nil), &cfg) != nil {
		t.Fatal("config")
	}
	shell, _ := cfg["run_shell"].(map[string]any)
	names, _ := shell["allowedEnvVars"].([]any)
	if len(names) != 1 || names[0] != "PYTHONDONTWRITEBYTECODE" || noBytecodeName+"="+noBytecodeValue != "PYTHONDONTWRITEBYTECODE=1" {
		t.Fatal("bytecode caches may enter the candidate", cfg["run_shell"])
	}
}
