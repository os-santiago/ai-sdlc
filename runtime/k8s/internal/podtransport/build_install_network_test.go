package podtransport

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildworkload"
)

// Real TLS API responses, original-slot checks and actual TCP denial distinguish
// configuration observation from the controller's mandatory connectivity gate.
func TestAdministrativeProfileDoesNotBypassRuntimeConnectivity(t *testing.T) {
	objects := map[string]map[string]any{}
	collections := map[string][]map[string]any{}
	mutations := 0
	r, _, _ := buildResourceFixture(t, func(r *BuildResources, w http.ResponseWriter, req *http.Request) {
		if req.Method != "GET" {
			mutations++
			w.WriteHeader(403)
			return
		}
		if obj := syntheticSlotResponse(r, req.URL.Path); obj != nil {
			json.NewEncoder(w).Encode(obj)
			return
		}
		if obj, ok := objects[req.URL.Path]; ok {
			json.NewEncoder(w).Encode(obj)
			return
		}
		if list, ok := collections[req.URL.Path]; ok {
			version, kind, _, _ := buildListType(req.URL.Path)
			json.NewEncoder(w).Encode(map[string]any{"apiVersion": version, "kind": kind, "metadata": map[string]any{}, "items": list})
			return
		}
		w.WriteHeader(404)
	})
	r.slots = []building.Slot{r.slot}
	add := func(path string, obj map[string]any) {
		if obj == nil {
			obj = map[string]any{"apiVersion": "v1", "kind": "ConfigMap"}
		}
		meta, ok := obj["metadata"].(map[string]any)
		if !ok {
			meta = map[string]any{}
			obj["metadata"] = meta
		}
		meta["name"] = path[strings.LastIndex(path, "/")+1:]
		meta["uid"] = fixtureConfigUID
		objects[path] = obj
	}
	for _, key := range buildworkload.DependencyKeys() {
		path, _ := dependencyPath(key)
		add(path, nil)
	}
	network := networkObjects()
	// TEST-NET-like private addresses intentionally have no listener in this
	// disposable fixture. Runtime must fail on any unreachable declared endpoint.
	for key, obj := range network {
		path, _ := dependencyPath(key)
		add(path, obj)
	}
	targets, err := BuildNetworkTargets(network)
	if err != nil {
		t.Fatal(err)
	}
	r.profile.DeniedTargets = targets
	policy, binding, err := buildworkload.Admission(r.profile, r.slots)
	if err != nil {
		t.Fatal(err)
	}
	mutation, mb := buildworkload.BoundsMutation()
	for key, raw := range map[string][]byte{"validatingAdmissionPolicy/ai-sdlc-build-pods": policy, "validatingAdmissionPolicyBinding/ai-sdlc-build-pods": binding, "mutatingAdmissionPolicy/ai-sdlc-build-bounds": mutation, "mutatingAdmissionPolicyBinding/ai-sdlc-build-bounds": mb} {
		var obj map[string]any
		if json.Unmarshal(raw, &obj) != nil {
			t.Fatal("fixture admission")
		}
		spec := obj["spec"].(map[string]any)
		for _, name := range []string{"matchConstraints", "matchResources"} {
			if match, ok := spec[name].(map[string]any); ok {
				if _, ok := match["matchPolicy"]; !ok {
					match["matchPolicy"] = "Equivalent"
				}
				for _, selector := range []string{"namespaceSelector", "objectSelector"} {
					if _, ok := match[selector]; !ok {
						match[selector] = map[string]any{}
					}
				}
				if rules, ok := match["resourceRules"].([]any); ok {
					for _, raw := range rules {
						rule := raw.(map[string]any)
						if _, ok := rule["scope"]; !ok {
							rule["scope"] = "*"
						}
					}
				}
			}
		}
		path, _ := dependencyPath(key)
		add(path, obj)
	}
	trust := BuildTrust{Version: "ai-sdlc.build-trust/v1", Components: map[string]buildworkload.Dependency{}}
	for key, path := range buildTrustPaths {
		add(path, nil)
		obj := objects[path]
		if key == "kindnet" || key == "tekton-controller" {
			obj["metadata"].(map[string]any)["generation"] = float64(1)
			obj["spec"] = map[string]any{"replicas": float64(1)}
			status := map[string]any{"observedGeneration": float64(1)}
			if key == "kindnet" {
				for _, name := range []string{"desiredNumberScheduled", "currentNumberScheduled", "numberReady", "updatedNumberScheduled", "numberAvailable"} {
					status[name] = float64(1)
				}
			} else {
				for _, name := range []string{"replicas", "updatedReplicas", "readyReplicas", "availableReplicas"} {
					status[name] = float64(1)
				}
				status["conditions"] = []any{map[string]any{"type": "Available", "status": "True"}}
			}
			obj["status"] = status
		}
		d, e := BuildObjectFingerprint(marshalObject(obj))
		if e != nil {
			t.Fatal(e)
		}
		trust.Components[key] = d
	}
	for _, path := range buildMutationPaths {
		collections[path] = []map[string]any{}
	}
	for _, key := range []string{"mutatingAdmissionPolicy/ai-sdlc-build-bounds", "mutatingAdmissionPolicyBinding/ai-sdlc-build-bounds"} {
		path, _ := dependencyPath(key)
		collection := path[:strings.LastIndex(path, "/")]
		collections[collection] = append(collections[collection], objects[path])
	}
	trust.MutationInventorySHA256, err = mutationDigest(collections)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(trust)
	path, _ := dependencyPath("configMap/build-trust")
	objects[path]["immutable"] = true
	objects[path]["data"] = map[string]any{"trust.json": string(raw)}
	for _, key := range buildworkload.DependencyKeys() {
		path, _ := dependencyPath(key)
		d, e := BuildObjectFingerprint(marshalObject(objects[path]))
		if e != nil {
			t.Fatal(e)
		}
		r.profile.Dependencies[key] = d
	}
	input := buildworkload.AdmissionConfiguration{Version: "ai-sdlc.build-admission-input/v1", ClusterUID: r.profile.ClusterUID, NamespaceUID: r.profile.NamespaceUID, PublisherImage: r.profile.PublisherImage, DeniedTargets: targets}
	observed, err := ObserveBuildProfile(context.Background(), r.client, input, r.slots, trust)
	if err != nil || observed.Digest() != r.profile.Digest() {
		t.Fatal("administrative configuration rejected", err)
	}
	if err = r.inspectInstalledProfile(context.Background()); err == nil {
		t.Fatal("runtime omitted positive connectivity")
	}
	if mutations != 0 {
		t.Fatal("read-only preflight performed an effect")
	}
	for _, key := range []string{"default/endpoints/kubernetes", "configMap/build-trust"} {
		path, _ := dependencyPath(key)
		original := objects[path]
		changed := cloneObject(original)
		if key == "configMap/build-trust" {
			changed["data"].(map[string]any)["trust.json"] = "{}"
		} else {
			changed["subsets"].([]any)[0].(map[string]any)["addresses"].([]any)[0].(map[string]any)["ip"] = "10.255.255.249"
		}
		objects[path] = changed
		if _, err = ObserveBuildProfile(context.Background(), r.client, input, r.slots, trust); err == nil {
			t.Fatal("administrative observer accepted drift", key)
		}
		if err = r.inspectInstalledProfile(context.Background()); err == nil {
			t.Fatal("runtime accepted drift", key)
		}
		objects[path] = original
	}
	if mutations != 0 {
		t.Fatal("drift rejection performed an effect")
	}
}
