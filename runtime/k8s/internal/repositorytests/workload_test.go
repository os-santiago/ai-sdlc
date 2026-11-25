package repositorytests

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
)

func testPod() Pod {
	return Pod{Namespace: "ai-sdlc",
		MailboxImage: "registry.example/mailbox@sha256:" + strings.Repeat("b", 64), RuntimeClass: executor.DefaultRuntimeClass, WorkItemID: "11111111-1111-4111-8111-111111111111",
		VerificationID: "verification-" + strings.Repeat("9", 64), Run: RunCross, SourceSHA: strings.Repeat("c", 40), TreeSHA256: strings.Repeat("d", 64),
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
		"restartPolicy": "Never", "hostNetwork": false, "activeDeadlineSeconds": float64(PullSeconds + ReadySeconds + 300 + MarginSeconds)} {
		if spec[key] != want {
			t.Fatal(key, spec[key])
		}
	}
	containers := spec["containers"].([]any)
	inits := spec["initContainers"].([]any)
	if len(containers) != 1 || len(inits) != 2 || inits[1].(map[string]any)["restartPolicy"] != "Always" || inits[1].(map[string]any)["name"] != "transfer" {
		t.Fatal("the transfer sidecar must be a native sidecar and tests the only container", containers, inits)
	}
	tests := containers[0].(map[string]any)
	limits := tests["resources"].(map[string]any)["limits"].(map[string]any)
	if limits["ephemeral-storage"] != "1408Mi" || limits["memory"] != "1Gi" {
		t.Fatal("the disk limit must cover the scratch, workspace and runner volumes", limits)
	}
	if tests["name"] != "tests" || tests["image"] != testPod().Verification.Image {
		t.Fatal("tests container", tests)
	}
	command := tests["command"].([]any)
	if command[0] != RunnerPath || command[1] != "run" || command[4] != testPod().SourceSHA || command[5] != testPod().TreeSHA256 || command[6] != "1" {
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
	if config["metadata"].(map[string]any)["name"] != "verify-"+strings.Repeat("9", 32)+"-inputs" || w.Pod["metadata"].(map[string]any)["name"] != "verify-"+strings.Repeat("9", 32)+"-tests" {
		t.Fatal("names must fit the verifier effect ledger", config["metadata"], w.Pod["metadata"])
	}
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
		"mutable image":  func(p *Pod) { p.MailboxImage = "registry.example/mailbox:latest" },
		"work item":      func(p *Pod) { p.WorkItemID = "x" },
		"verification":   func(p *Pod) { p.VerificationID = "verification-x" },
		"unknown run":    func(p *Pod) { p.Run = "candidate-3" },
		"source":         func(p *Pod) { p.SourceSHA = "abc" },
		"tree":           func(p *Pod) { p.TreeSHA256 = "" },
		"network":        func(p *Pod) { p.Verification.Network = "default" },
		"toolchain pin":  func(p *Pod) { p.Verification.Image = "golang:1.25" },
		"empty command":  func(p *Pod) { p.Verification.Command = nil },
		"short timeout":  func(p *Pod) { p.Verification.TimeoutSeconds = 1 },
		"repeat too big": func(p *Pod) { p.Verification.Repeat = 4 },
		"reserved env":   func(p *Pod) { p.Verification.Env = map[string]string{"HOME": "/root"} },
		"relative PATH":  func(p *Pod) { p.Verification.Env = map[string]string{"PATH": "bin:/usr/bin"} },
		"writable PATH":  func(p *Pod) { p.Verification.Env = map[string]string{"PATH": "/workspaces/repository/bin:/usr/bin"} },
		"scratch PATH":   func(p *Pod) { p.Verification.Env = map[string]string{"PATH": "/usr/bin:/tmp"} },
		"loader env":     func(p *Pod) { p.Verification.Env = map[string]string{"LD_PRELOAD": "/x.so"} },
		"no sandbox":     func(p *Pod) { p.RuntimeClass = "" },
		"bad sandbox":    func(p *Pod) { p.RuntimeClass = "Kata" },
	}
	for name, mutate := range mutations {
		p := testPod()
		mutate(&p)
		if _, err := CompilePod(p); err == nil {
			t.Fatal("accepted", name)
		}
	}
}

func TestCompilePodUsesInstalledSandboxRuntimeClass(t *testing.T) {
	p := testPod()
	w, err := CompilePod(p)
	if err != nil || w.Pod["spec"].(map[string]any)["runtimeClassName"] != "ai-sdlc-offline" {
		t.Fatal("default sandbox runtime class changed", err)
	}
	p.RuntimeClass = "gvisor"
	w, err = CompilePod(p)
	if err != nil || w.Pod["spec"].(map[string]any)["runtimeClassName"] != "gvisor" {
		t.Fatal("configured sandbox runtime class not used", err)
	}
}

func TestReviewedEnvCompiles(t *testing.T) {
	p := testPod()
	p.Verification.Env = map[string]string{"PATH": "/usr/local/go/bin:/usr/bin:/bin", "GOFLAGS": "-mod=vendor"}
	if _, err := CompilePod(p); err != nil {
		t.Fatal(err)
	}
}

func TestRunsShareSettingsAndTheCandidateRepeats(t *testing.T) {
	p := testPod()
	p.Verification.Repeat = 3
	pods := map[string]bool{}
	var settings any
	for _, run := range RunNames() {
		p.Run = run
		w, err := CompilePod(p)
		if err != nil {
			t.Fatal(run, err)
		}
		name := w.Pod["metadata"].(map[string]any)["name"].(string)
		if pods[name] || len(name) > 63 {
			t.Fatal("pod name", name)
		}
		pods[name] = true
		if settings != nil && !reflect.DeepEqual(settings, w.ConfigMap) {
			t.Fatal("the runs of one verification must share one settings object")
		}
		settings = w.ConfigMap
		spec := w.Pod["spec"].(map[string]any)
		command := spec["containers"].([]any)[0].(map[string]any)["command"].([]any)
		want, deadline := "1", float64(PullSeconds+ReadySeconds+300+MarginSeconds)
		if run == RunCandidate {
			want, deadline = "3", float64(PullSeconds+ReadySeconds+3*300+MarginSeconds)
		}
		if command[6] != want || spec["activeDeadlineSeconds"] != deadline {
			t.Fatal("repetitions", run, command[6], spec["activeDeadlineSeconds"])
		}
	}
}

// Digests of the pods rendered on main before analysis existed: a manifest
// without analysis must render every run byte for byte as before.
func TestPodsWithoutAnalysisRenderExactlyAsBefore(t *testing.T) {
	want := map[string]string{
		RunBaseline:  "7edc86efb09dcb3d7e892a609f8fe0c17e8c1a85e101ede22b87177f2c1452dd",
		RunCross:     "b676b2bdfd10704833b5d87a761918e1814cdee2eb1467f524a84bd075b35c68",
		RunCandidate: "968652eb834b99c19902e5c050e0c539c3b6aec0f5c414213dbe579cbb22a63e",
	}
	for _, run := range RunNames() {
		p := testPod()
		p.Run = run
		w, err := CompilePod(p)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(w)
		if got := fmt.Sprintf("%x", sha256.Sum256(raw)); got != want[run] {
			t.Fatalf("%s pod changed: %s", run, got)
		}
	}
}

func TestPodsWithAnalysisAnalyzeBaselineAndCandidateOnly(t *testing.T) {
	for _, run := range RunNames() {
		p := testPod()
		p.Run = run
		p.Verification.Analysis = &project.Analysis{Commands: [][]string{{"go", "vet", "./..."}, {"staticcheck", "./..."}}, TimeoutSeconds: 90}
		w, err := CompilePod(p)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(w.Pod)
		if strings.Contains(string(raw), "vet") || strings.Contains(string(raw), "staticcheck") {
			t.Fatal("an analysis command reached the pod spec")
		}
		spec := w.Pod["spec"].(map[string]any)
		command := spec["containers"].([]any)[0].(map[string]any)["command"].([]any)
		analyzes := run != RunCross
		extra := 0
		if analyzes {
			extra = 2 * 90
		}
		if (len(command) == 8 && command[7] == AnalyzeArgument) != analyzes || (!analyzes && len(command) != 7) {
			t.Fatal(run, "runner command", command)
		}
		if spec["activeDeadlineSeconds"] != float64(PullSeconds+ReadySeconds+Repetitions(run, p.Verification)*300+extra+MarginSeconds) ||
			AnalysisSeconds(run, p.Verification) != extra || Analyzes(run, p.Verification) != analyzes {
			t.Fatal(run, "deadline", spec["activeDeadlineSeconds"])
		}
		settings := w.ConfigMap["data"].(map[string]any)["verification.json"].(string)
		if !strings.Contains(settings, `"analysis":{"commands":[["go","vet","./..."],["staticcheck","./..."]],"timeoutSeconds":90}`) {
			t.Fatal("settings", settings)
		}
	}
}
