package podtransport

// Configuration fingerprints bind desired controller behavior. Readiness adds
// a current observed-generation check; it does not turn this point-in-time
// observation into a claim of continuous controller or network availability.
func buildControllerReady(obj map[string]any, daemon bool) bool {
	meta, ok := obj["metadata"].(map[string]any)
	if !ok {
		return false
	}
	generation, ok := meta["generation"].(float64)
	if !ok || generation < 1 || generation != float64(int64(generation)) {
		return false
	}
	status, ok := obj["status"].(map[string]any)
	if !ok || status["observedGeneration"] != generation {
		return false
	}
	spec, ok := obj["spec"].(map[string]any)
	if !ok {
		return false
	}
	if daemon {
		desired, ok := status["desiredNumberScheduled"].(float64)
		if !ok || desired < 1 || desired != float64(int64(desired)) {
			return false
		}
		for _, k := range []string{"currentNumberScheduled", "numberReady", "updatedNumberScheduled", "numberAvailable"} {
			if status[k] != desired {
				return false
			}
		}
		for _, k := range []string{"numberMisscheduled", "numberUnavailable"} {
			if value, exists := status[k]; exists && value != float64(0) {
				return false
			}
		}
		return true
	}
	desired, ok := spec["replicas"].(float64)
	if !ok || desired != 1 {
		return false
	}
	for _, k := range []string{"replicas", "updatedReplicas", "readyReplicas", "availableReplicas"} {
		if status[k] != desired {
			return false
		}
	}
	if v, exists := status["unavailableReplicas"]; exists && v != float64(0) {
		return false
	}
	conditions, ok := status["conditions"].([]any)
	if !ok {
		return false
	}
	available := false
	for _, raw := range conditions {
		c, ok := raw.(map[string]any)
		if !ok {
			return false
		}
		if c["type"] == "Available" {
			if available || c["status"] != "True" {
				return false
			}
			available = true
		}
	}
	return available
}
