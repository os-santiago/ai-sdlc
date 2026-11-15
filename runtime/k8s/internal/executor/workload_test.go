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
	workload, err := Compile("fixture", image, image, assignment, profile, adev, now)
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
		if _, err := Compile("fixture", bad, image, assignment, profile, adev, now); err == nil {
			t.Fatal("unpinned image accepted")
		}
	}
	if _, err := Compile("fixture", image, image, assignment, profile, []byte("changed"), now); err == nil {
		t.Fatal("unbound A-Dev accepted")
	}
	if _, err := Compile("bad/namespace", image, image, assignment, profile, adev, now); err == nil {
		t.Fatal("namespace path accepted")
	}
	if _, err := Compile("fixture", image, image, assignment, profile, adev, now.Add(2*time.Minute)); err == nil {
		t.Fatal("expired assignment accepted")
	}
}
