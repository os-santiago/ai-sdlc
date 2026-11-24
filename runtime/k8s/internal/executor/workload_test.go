package executor

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

func TestWorkloadCompilerPinsIsolationAndReviewedInputs(t *testing.T) {
	now := time.Now()
	adev := []byte("reviewed profile fixture")
	p := profileFixture()
	p.ADevSHA256 = workspacebundle.Digest(adev)
	profile, _ := json.Marshal(p)
	a := assignmentFixture(now)
	a.ProfileSHA256 = workspacebundle.Digest(profile)
	assignment, _ := json.Marshal(a)
	image := "quay.io/os-santiago/ai-sdlc@sha256:" + strings.Repeat("a", 64)
	workload, err := Compile("fixture", image, image, DefaultRuntimeClass, assignment, profile, adev, now)
	if err != nil {
		t.Fatal(err)
	}
	if workload.ConfigMap["immutable"] != true {
		t.Fatal("mutable input")
	}
	spec := workload.Pod["spec"].(map[string]any)
	if spec["runtimeClassName"] != "ai-sdlc-offline" || spec["automountServiceAccountToken"] != false || spec["hostPID"] != false || spec["shareProcessNamespace"] != false || spec["hostNetwork"] != false || spec["restartPolicy"] != "Never" {
		t.Fatal("isolation contract changed")
	}
	containers := spec["containers"].([]any)
	for _, c := range containers {
		value := c.(map[string]any)
		security := value["securityContext"].(map[string]any)
		if value["image"] != image || security["readOnlyRootFilesystem"] != true || security["allowPrivilegeEscalation"] != false {
			t.Fatal("container isolation changed")
		}
	}
	worker := containers[0].(map[string]any)
	for _, mount := range worker["volumeMounts"].([]any) {
		if mount.(map[string]any)["name"] == "bridge" {
			t.Fatal("worker mounts mailbox files")
		}
	}
	// Objects are independent, avoiding accidental privilege widening via aliases.
	worker["securityContext"].(map[string]any)["allowPrivilegeEscalation"] = true
	if containers[1].(map[string]any)["securityContext"].(map[string]any)["allowPrivilegeEscalation"] != false {
		t.Fatal("aliased security objects")
	}
	for _, bad := range []string{"quay.io/os-santiago/ai-sdlc:latest", "https://attacker", "name@sha256:" + strings.Repeat("x", 64)} {
		if _, err := Compile("fixture", bad, image, DefaultRuntimeClass, assignment, profile, adev, now); err == nil {
			t.Fatal("unpinned image accepted")
		}
	}
	if _, err := Compile("fixture", image, image, DefaultRuntimeClass, assignment, profile, []byte("changed"), now); err == nil {
		t.Fatal("unbound A-Dev accepted")
	}
	if _, err := Compile("bad/namespace", image, image, DefaultRuntimeClass, assignment, profile, adev, now); err == nil {
		t.Fatal("namespace path accepted")
	}
	if _, err := Compile("fixture", image, image, DefaultRuntimeClass, assignment, profile, adev, now.Add(2*time.Minute)); err == nil {
		t.Fatal("expired assignment accepted")
	}
}

func TestWorkloadCompilerUsesConfiguredSandboxRuntimeClass(t *testing.T) {
	now := time.Now()
	adev := []byte("reviewed profile fixture")
	p := profileFixture()
	p.ADevSHA256 = workspacebundle.Digest(adev)
	profile, _ := json.Marshal(p)
	a := assignmentFixture(now)
	a.ProfileSHA256 = workspacebundle.Digest(profile)
	assignment, _ := json.Marshal(a)
	image := "quay.io/os-santiago/ai-sdlc@sha256:" + strings.Repeat("a", 64)
	base, err := Compile("fixture", image, image, DefaultRuntimeClass, assignment, profile, adev, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, class := range []string{"gvisor", "kata", "kata-mshv-vm-isolation", "sandbox.example.com"} {
		w, err := Compile("fixture", image, image, class, assignment, profile, adev, now)
		if err != nil {
			t.Fatal(class, err)
		}
		if w.Pod["spec"].(map[string]any)["runtimeClassName"] != class {
			t.Fatal("configured runtime class not used", class)
		}
		// Only the runtime class differs from the default compiled pod.
		w.Pod["spec"].(map[string]any)["runtimeClassName"] = DefaultRuntimeClass
		got, _ := json.Marshal(w)
		want, _ := json.Marshal(base)
		if string(got) != string(want) {
			t.Fatal("runtime class changed other pod fields", class)
		}
	}
	// No value means no sandbox, so it is rejected rather than defaulted here.
	for _, bad := range []string{"", "Kata", "-kata", "kata-", "kata_vm", "kata/../x", "a..b", strings.Repeat("a", 254)} {
		if ValidRuntimeClass(bad) {
			t.Fatal("invalid runtime class accepted", bad)
		}
		if _, err := Compile("fixture", image, image, bad, assignment, profile, adev, now); err == nil {
			t.Fatal("invalid runtime class compiled", bad)
		}
	}
	if !ValidRuntimeClass(strings.Repeat("a", 63) + "." + strings.Repeat("b", 63)) {
		t.Fatal("valid subdomain rejected")
	}
}
