package podtransport

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildworkload"
)

func TestBuildFingerprintBindsConfigurationButNotObservationBookkeeping(t *testing.T) {
	obj := map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "fixture", "namespace": "ai-sdlc-builds", "uid": fixtureConfigUID, "resourceVersion": "1", "generation": float64(1)}, "immutable": true, "data": map[string]any{"policy": "reviewed"}}
	fingerprint, e := BuildObjectFingerprint(marshalObject(obj))
	if e != nil {
		t.Fatal(e)
	}
	for _, which := range []string{"version", "status", "data", "uid", "labels", "owner", "future-field", "deletion"} {
		changed := cloneObject(obj)
		meta := changed["metadata"].(map[string]any)
		switch which {
		case "version":
			meta["resourceVersion"] = "2"
			meta["generation"] = float64(2)
		case "status":
			changed["status"] = map[string]any{"ready": true}
		case "data":
			changed["data"].(map[string]any)["policy"] = "changed"
		case "uid":
			meta["uid"] = fixtureOtherUID
		case "labels":
			meta["labels"] = map[string]any{"changed": "true"}
		case "owner":
			meta["ownerReferences"] = []any{map[string]any{"uid": fixtureOtherUID}}
		case "future-field":
			changed["newExecutionField"] = true
		case "deletion":
			meta["deletionTimestamp"] = "2026-11-16T00:00:00Z"
		}
		got, e := BuildObjectFingerprint(marshalObject(changed))
		if which == "version" || which == "status" {
			if e != nil || got != fingerprint {
				t.Fatal("observation bookkeeping changed fingerprint", which)
			}
		} else if e == nil && got == fingerprint {
			t.Fatal("configuration drift hidden", which)
		}
	}
	if _, e := BuildObjectFingerprint([]byte(`{"metadata":{"uid":"` + fixtureConfigUID + `","uid":"` + fixtureOtherUID + `","name":"fixture"}}`)); e == nil {
		t.Fatal("duplicate JSON key accepted")
	}
}
func TestWholeMutationInventoryRejectsUnknownAddedRemovedAndChangedObjects(t *testing.T) {
	input := map[string][]map[string]any{}
	for _, path := range buildMutationPaths {
		input[path] = []map[string]any{}
	}
	base, e := mutationDigest(input)
	if e != nil {
		t.Fatal(e)
	}
	obj := map[string]any{"apiVersion": "admissionregistration.k8s.io/v1", "kind": "MutatingAdmissionPolicy", "metadata": map[string]any{"name": "new-mutator", "uid": fixtureConfigUID}, "spec": map[string]any{"failurePolicy": "Fail"}}
	input[buildMutationPaths[1]] = append(input[buildMutationPaths[1]], obj)
	changed, e := mutationDigest(input)
	if e != nil || changed == base {
		t.Fatal("new mutator hidden")
	}
	obj["spec"].(map[string]any)["failurePolicy"] = "Ignore"
	again, e := mutationDigest(input)
	if e != nil || again == changed {
		t.Fatal("changed mutator hidden")
	}
	input[buildMutationPaths[1]] = append(input[buildMutationPaths[1]], obj)
	if _, e := mutationDigest(input); e == nil {
		t.Fatal("duplicate identity accepted")
	}
	delete(input, buildMutationPaths[0])
	if _, e := mutationDigest(input); e == nil {
		t.Fatal("incomplete inventory accepted")
	}
}
func networkObjects() map[string]map[string]any {
	out := map[string]map[string]any{}
	for i, prefix := range []string{"default/", "kind-registry/"} {
		name := "kubernetes"
		if i == 1 {
			name = "registry-service"
		}
		ip := []string{"10.96.0.1", "10.96.0.2"}[i]
		backend := []string{"172.18.0.2", "10.244.0.5"}[i]
		port := []float64{6443, 5000}[i]
		out[prefix+"service/"+name] = map[string]any{"spec": map[string]any{"clusterIP": ip, "clusterIPs": []any{ip}, "ports": []any{map[string]any{"protocol": "TCP", "port": float64(443)}}}}
		out[prefix+"endpoints/"+name] = map[string]any{"metadata": map[string]any{}, "subsets": []any{map[string]any{"addresses": []any{map[string]any{"ip": backend}}, "ports": []any{map[string]any{"protocol": "TCP", "port": port}}}}}
	}
	return out
}
func TestBuildNetworkTargetsCoverActualServicesAndBackends(t *testing.T) {
	expected := []buildworkload.DeniedTarget{{Host: "10.96.0.1", Port: 443}, {Host: "10.96.0.2", Port: 443}, {Host: "172.18.0.2", Port: 6443}, {Host: "10.244.0.5", Port: 5000}}
	got, e := BuildNetworkTargets(networkObjects())
	if e != nil || !checkTargetEquality(expected, got) {
		t.Fatal("actual endpoint coverage missing", e)
	}
	if checkTargetEquality([]buildworkload.DeniedTarget{{Host: "10.99.99.1", Port: 443}, {Host: "10.99.99.2", Port: 443}}, got) {
		t.Fatal("blackhole addresses accepted")
	}
	for _, which := range []string{"missing", "ipv6", "public", "udp", "truncated", "not-ready", "empty", "fractional", "extra-service-ip"} {
		obj := networkObjects()
		service := obj["default/service/kubernetes"]["spec"].(map[string]any)
		ep := obj["default/endpoints/kubernetes"]
		subset := ep["subsets"].([]any)[0].(map[string]any)
		switch which {
		case "missing":
			delete(obj, "default/endpoints/kubernetes")
		case "ipv6":
			subset["addresses"].([]any)[0].(map[string]any)["ip"] = "fd00::1"
		case "public":
			subset["addresses"].([]any)[0].(map[string]any)["ip"] = "1.1.1.1"
		case "udp":
			subset["ports"].([]any)[0].(map[string]any)["protocol"] = "UDP"
		case "truncated":
			ep["metadata"].(map[string]any)["annotations"] = map[string]any{"endpoints.kubernetes.io/over-capacity": "truncated"}
		case "not-ready":
			subset["notReadyAddresses"] = []any{map[string]any{"ip": "172.18.0.3"}}
		case "empty":
			ep["subsets"] = []any{}
		case "fractional":
			subset["ports"].([]any)[0].(map[string]any)["port"] = float64(443.5)
		case "extra-service-ip":
			service["clusterIPs"] = []any{"10.96.0.1", "10.96.0.3"}
		}
		if _, e := BuildNetworkTargets(obj); e == nil {
			raw, _ := json.Marshal(obj)
			t.Fatal("invalid targets accepted", which, string(raw))
		}
	}
}
func TestBuildTrustIsFiniteAndNoDependencyAliasIsAccepted(t *testing.T) {
	trust := BuildTrust{Version: "ai-sdlc.build-trust/v1", MutationInventorySHA256: strings.Repeat("a", 64), Components: map[string]buildworkload.Dependency{}}
	for key := range buildTrustPaths {
		trust.Components[key] = buildworkload.Dependency{UID: fixtureConfigUID, SpecSHA256: strings.Repeat("b", 64)}
	}
	if trust.Validate() != nil {
		t.Fatal("valid finite trust denied")
	}
	trust.Components["unreviewed"] = trust.Components["kindnet"]
	if trust.Validate() == nil {
		t.Fatal("unknown trust target accepted")
	}
	for _, key := range buildworkload.DependencyKeys() {
		if _, ok := dependencyPath(key); !ok {
			t.Fatal("required dependency lacks exact route", key)
		}
	}
	if _, ok := dependencyPath("configMap/../../secrets"); ok {
		t.Fatal("arbitrary API route exposed")
	}
}
