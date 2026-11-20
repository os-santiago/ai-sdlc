package repositorytests

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
)

func testPod() Pod {
	return Pod{Namespace: "ai-sdlc", VerifierImage: "registry.example/verifier@sha256:" + strings.Repeat("a", 64),
		MailboxImage: "registry.example/mailbox@sha256:" + strings.Repeat("b", 64), WorkItemID: "11111111-1111-4111-8111-111111111111",
		Attempt: 1, Run: "cross", SourceSHA: strings.Repeat("c", 40), TreeSHA256: strings.Repeat("d", 64),
		Verification: project.Verification{Image: "registry.example/go@sha256:" + strings.Repeat("e", 64), Command: []string{"go", "test", "./...", "$(POD_UID)"},
			TimeoutSeconds: 300, CPU: "1", Memory: "1Gi", EphemeralStorage: "1Gi", Network: "none", Result: project.Result{ExitCode: true},
			Repeat: 2, TestGlobs: []string{"**/*_test.go"}}}
}

func TestCompilePodIsOfflineBoundedAndKeepsTheCommandOutOfTheSpec(t *testing.T) {
	w, err := CompilePod(testPod())
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(w.Pod)
	if strings.Contains(string(raw), "./...") {
		t.Fatal("the repository command reached the pod spec")
	}
	spec := w.Pod["spec"].(map[string]any)
	for key, want := range map[string]any{"runtimeClassName": "ai-sdlc-offline", "automountServiceAccountToken": false, "enableServiceLinks": false,
		"restartPolicy": "Never", "hostNetwork": false, "activeDeadlineSeconds": float64(ReadySeconds + 300 + MarginSeconds)} {
		if spec[key] != want {
			t.Fatal(key, spec[key])
		}
	}
	containers := spec["containers"].([]any)
	if len(containers) != 2 || len(spec["initContainers"].([]any)) != 1 {
		t.Fatal("containers", containers)
	}
	tests := containers[1].(map[string]any)
	if tests["name"] != "tests" || tests["image"] != testPod().Verification.Image {
		t.Fatal("tests container", tests)
	}
	command := tests["command"].([]any)
	if command[0] != RunnerPath || command[1] != "repository-tests-run" || command[4] != testPod().SourceSHA || command[5] != testPod().TreeSHA256 {
		t.Fatal("runner command", command)
	}
	if _, env := tests["env"]; env {
		t.Fatal("the tests container must receive no environment")
	}
	for _, c := range append(containers, spec["initContainers"].([]any)...) {
		sc := c.(map[string]any)["securityContext"].(map[string]any)
		if sc["readOnlyRootFilesystem"] != true || sc["allowPrivilegeEscalation"] != false || sc["runAsNonRoot"] != true {
			t.Fatal("security context", c)
		}
	}
	for _, m := range tests["volumeMounts"].([]any) {
		mount := m.(map[string]any)
		if (mount["name"] == "runner" || mount["name"] == "settings") && mount["readOnly"] != true {
			t.Fatal("runner and settings must be read-only for repository code", mount)
		}
	}
	config := w.ConfigMap
	if config["immutable"] != true {
		t.Fatal("settings must be immutable")
	}
	var v project.Verification
	if err := json.Unmarshal([]byte(config["data"].(map[string]any)["verification.json"].(string)), &v); err != nil || v.Command[3] != "$(POD_UID)" {
		t.Fatal("settings must carry the command verbatim", v, err)
	}
}

func TestCompilePodRefusesInvalidInput(t *testing.T) {
	mutations := map[string]func(*Pod){
		"namespace":      func(p *Pod) { p.Namespace = "Bad_NS" },
		"mutable image":  func(p *Pod) { p.VerifierImage = "registry.example/verifier:latest" },
		"work item":      func(p *Pod) { p.WorkItemID = "x" },
		"attempt":        func(p *Pod) { p.Attempt = 0 },
		"unknown run":    func(p *Pod) { p.Run = "candidate-3" },
		"source":         func(p *Pod) { p.SourceSHA = "abc" },
		"tree":           func(p *Pod) { p.TreeSHA256 = "" },
		"network":        func(p *Pod) { p.Verification.Network = "default" },
		"toolchain pin":  func(p *Pod) { p.Verification.Image = "golang:1.25" },
		"empty command":  func(p *Pod) { p.Verification.Command = nil },
		"short timeout":  func(p *Pod) { p.Verification.TimeoutSeconds = 1 },
		"repeat too big": func(p *Pod) { p.Verification.Repeat = 4 },
	}
	for name, mutate := range mutations {
		p := testPod()
		mutate(&p)
		if _, err := CompilePod(p); err == nil {
			t.Fatal("accepted", name)
		}
	}
}

func TestRunNamesAndDistinctPods(t *testing.T) {
	names := RunNames(3)
	if strings.Join(names, ",") != "baseline,cross,candidate-1,candidate-2,candidate-3" {
		t.Fatal(names)
	}
	seen := map[string]bool{}
	p := testPod()
	p.Verification.Repeat = 3
	for _, run := range names {
		p.Run = run
		w, err := CompilePod(p)
		if err != nil {
			t.Fatal(run, err)
		}
		name := w.Pod["metadata"].(map[string]any)["name"].(string)
		if seen[name] || len(name) > 63 {
			t.Fatal("pod name", name)
		}
		seen[name] = true
	}
}
