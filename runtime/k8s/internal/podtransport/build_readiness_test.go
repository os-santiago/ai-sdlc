package podtransport

import "testing"

func TestBuildControllersRequireFreshCompleteRollouts(t *testing.T) {
	for _, daemon := range []bool{false, true} {
		status := map[string]any{"observedGeneration": float64(4)}
		if daemon {
			for _, k := range []string{"desiredNumberScheduled", "currentNumberScheduled", "numberReady", "updatedNumberScheduled", "numberAvailable"} {
				status[k] = float64(1)
			}
		} else {
			for _, k := range []string{"replicas", "updatedReplicas", "readyReplicas", "availableReplicas"} {
				status[k] = float64(1)
			}
			status["conditions"] = []any{map[string]any{"type": "Available", "status": "True"}}
		}
		obj := map[string]any{"metadata": map[string]any{"generation": float64(4)}, "spec": map[string]any{"replicas": float64(1)}, "status": status}
		if !buildControllerReady(obj, daemon) {
			t.Fatal("complete rollout rejected")
		}
		for key := range status {
			changed := cloneObject(obj)
			changed["status"].(map[string]any)[key] = float64(0)
			if buildControllerReady(changed, daemon) {
				t.Fatal("stale/incomplete rollout accepted", key)
			}
		}
	}
}
