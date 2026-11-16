package podtransport

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecordedBuildContainerTermination(t *testing.T) {
	directory := os.Getenv("AI_SDLC_BUILD_TERMINAL_FIXTURE")
	if directory == "" {
		t.Skip("explicit archived successful source/publisher fixture required")
	}
	for _, file := range []string{"saferus-go-go-pod.json", "saferus-publish-publish-pod.json"} {
		raw, e := os.ReadFile(filepath.Join(directory, file))
		if e != nil {
			t.Fatal(e)
		}
		var pod map[string]any
		if json.Unmarshal(raw, &pod) != nil {
			t.Fatal("bad fixture")
		}
		spec := pod["spec"].(map[string]any)
		status := pod["status"].(map[string]any)
		pins := &buildPins{}
		exits, complete, e := inspectContainerStates(spec, status, pins)
		if e != nil || !complete || len(exits) != len(spec["containers"].([]any))+len(spec["initContainers"].([]any)) {
			t.Fatalf("real termination denied %s: %v", file, e)
		}
		for _, which := range []string{"restart", "replaced", "missing", "extra", "image", "exit", "regression", "duplicate-id", "last-state", "reversed-time"} {
			t.Run(file+"/"+which, func(t *testing.T) {
				before := &buildPins{}
				if _, _, e := inspectContainerStates(spec, status, before); e != nil {
					t.Fatal(e)
				}
				changed := cloneObject(status)
				entry := changed["containerStatuses"].([]any)[0].(map[string]any)
				switch which {
				case "restart":
					entry["restartCount"] = float64(1)
				case "replaced":
					entry["containerID"] = "containerd://" + strings.Repeat("0", 64)
					entry["state"].(map[string]any)["terminated"].(map[string]any)["containerID"] = entry["containerID"]
				case "missing":
					delete(changed, "containerStatuses")
				case "extra":
					changed["containerStatuses"] = append(changed["containerStatuses"].([]any), cloneObject(entry))
				case "image":
					entry["imageID"] = "untrusted@sha256:" + strings.Repeat("0", 64)
				case "exit":
					entry["state"].(map[string]any)["terminated"].(map[string]any)["exitCode"] = float64(1)
				case "regression":
					entry["state"] = map[string]any{"running": map[string]any{}}
				case "duplicate-id":
					entry["containerID"] = changed["initContainerStatuses"].([]any)[0].(map[string]any)["containerID"]
				case "last-state":
					entry["lastState"] = map[string]any{"terminated": map[string]any{"exitCode": float64(0)}}
				case "reversed-time":
					entry["state"].(map[string]any)["terminated"].(map[string]any)["finishedAt"] = "2000-01-01T00:00:00Z"
				}
				if _, _, e := inspectContainerStates(spec, changed, before); e == nil {
					t.Fatal("invalid original termination accepted")
				}
			})
		}
	}
}
func TestBuildSucceededConditionRejectsFailureDuplicateAndMalformed(t *testing.T) {
	for _, which := range []string{"pending", "succeeded", "failed", "duplicate", "bad-type"} {
		conditions := []any{map[string]any{"type": "Succeeded", "status": "Unknown"}}
		switch which {
		case "succeeded":
			conditions[0].(map[string]any)["status"] = "True"
		case "failed":
			conditions[0].(map[string]any)["status"] = "False"
		case "duplicate":
			conditions = append(conditions, map[string]any{"type": "Succeeded", "status": "True"})
		case "bad-type":
			conditions = []any{"invalid"}
		}
		done, e := succeededStatus(map[string]any{"status": map[string]any{"conditions": conditions}})
		if which == "pending" {
			if e != nil || done {
				t.Fatal("pending misclassified")
			}
		} else if which == "succeeded" {
			if e != nil || !done {
				t.Fatal("success denied")
			}
		} else if e == nil {
			t.Fatal("invalid completion accepted", which)
		}
	}
}
