package podtransport

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"sort"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildworkload"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

// BuildTrust is public installed configuration bound through the immutable
// build-trust ConfigMap fingerprint in the signed recipe profile. It is never
// learned or updated automatically by the runtime when drift is observed.
type BuildTrust struct {
	Version                 string                              `json:"version"`
	MutationInventorySHA256 string                              `json:"mutationInventorySHA256"`
	Components              map[string]buildworkload.Dependency `json:"components"`
}

var buildTrustPaths = map[string]string{
	"tekton-controller": "/apis/apps/v1/namespaces/tekton-pipelines/deployments/tekton-pipelines-controller",
	"tekton-defaults":   "/api/v1/namespaces/tekton-pipelines/configmaps/config-defaults",
	"tekton-features":   "/api/v1/namespaces/tekton-pipelines/configmaps/feature-flags",
	"trusted-cabundle":  "/api/v1/namespaces/ai-sdlc-builds/configmaps/config-trusted-cabundle",
	"service-cabundle":  "/api/v1/namespaces/ai-sdlc-builds/configmaps/config-service-cabundle",
	"kindnet":           "/apis/apps/v1/namespaces/kube-system/daemonsets/kindnet",
}
var buildMutationPaths = []string{
	"/apis/admissionregistration.k8s.io/v1/mutatingwebhookconfigurations",
	"/apis/admissionregistration.k8s.io/v1/mutatingadmissionpolicies",
	"/apis/admissionregistration.k8s.io/v1/mutatingadmissionpolicybindings",
}

func (t BuildTrust) Validate() error {
	if t.Version != "ai-sdlc.build-trust/v1" || !workspacebundle.ValidDigest(t.MutationInventorySHA256) || len(t.Components) != len(buildTrustPaths) {
		return ErrBuildTransport
	}
	for key := range buildTrustPaths {
		d, ok := t.Components[key]
		if !ok || !resourceUID.MatchString(d.UID) || !workspacebundle.ValidDigest(d.SpecSHA256) {
			return ErrBuildTransport
		}
	}
	return nil
}
func dependencyPath(key string) (string, bool) {
	ns := buildworkload.Namespace
	fixed := map[string]string{
		"serviceAccount/build":                                "/api/v1/namespaces/" + ns + "/serviceaccounts/build",
		"serviceAccount/publish":                              "/api/v1/namespaces/" + ns + "/serviceaccounts/publish",
		"secret/registry-auth":                                "/api/v1/namespaces/" + ns + "/secrets/registry-auth",
		"configMap/build-trust":                               "/api/v1/namespaces/" + ns + "/configmaps/build-trust",
		"configMap/trusted-ca":                                "/api/v1/namespaces/" + ns + "/configmaps/trusted-ca",
		"configMap/tekton-flags":                              "/api/v1/namespaces/tekton-pipelines/configmaps/feature-flags",
		"networkPolicy/build":                                 "/apis/networking.k8s.io/v1/namespaces/" + ns + "/networkpolicies/build",
		"networkPolicy/publish":                               "/apis/networking.k8s.io/v1/namespaces/" + ns + "/networkpolicies/publish",
		"limitRange/build-budget":                             "/api/v1/namespaces/" + ns + "/limitranges/build-budget",
		"validatingAdmissionPolicy/ai-sdlc-build-pods":        "/apis/admissionregistration.k8s.io/v1/validatingadmissionpolicies/ai-sdlc-build-pods",
		"validatingAdmissionPolicyBinding/ai-sdlc-build-pods": "/apis/admissionregistration.k8s.io/v1/validatingadmissionpolicybindings/ai-sdlc-build-pods",
		"mutatingAdmissionPolicy/ai-sdlc-build-bounds":        "/apis/admissionregistration.k8s.io/v1/mutatingadmissionpolicies/ai-sdlc-build-bounds",
		"mutatingAdmissionPolicyBinding/ai-sdlc-build-bounds": "/apis/admissionregistration.k8s.io/v1/mutatingadmissionpolicybindings/ai-sdlc-build-bounds",
		"default/service/kubernetes":                          "/api/v1/namespaces/default/services/kubernetes",
		"default/endpoints/kubernetes":                        "/api/v1/namespaces/default/endpoints/kubernetes",
		"kind-registry/service/registry-service":              "/api/v1/namespaces/kind-registry/services/registry-service",
		"kind-registry/endpoints/registry-service":            "/api/v1/namespaces/kind-registry/endpoints/registry-service",
	}
	path, ok := fixed[key]
	return path, ok
}

// BuildObjectFingerprint excludes only server observation bookkeeping. Every
// other top-level field remains hashed; unknown configuration cannot disappear
// during normalization. This helper does not establish installation authority.
func BuildObjectFingerprint(raw []byte) (buildworkload.Dependency, error) {
	var obj map[string]any
	if len(raw) > maxBuildObject || authority.DecodeStrict(raw, &obj) != nil {
		return buildworkload.Dependency{}, ErrBuildTransport
	}
	metadata, ok := obj["metadata"].(map[string]any)
	if !ok {
		return buildworkload.Dependency{}, ErrBuildTransport
	}
	uid, ok := metadata["uid"].(string)
	if !ok || !resourceUID.MatchString(uid) || metadata["deletionTimestamp"] != nil {
		return buildworkload.Dependency{}, ErrBuildTransport
	}
	if _, ok := metadata["name"].(string); !ok {
		return buildworkload.Dependency{}, ErrBuildTransport
	}
	delete(obj, "status")
	for _, k := range []string{"resourceVersion", "creationTimestamp", "managedFields", "generation"} {
		delete(metadata, k)
	}
	canonical, _ := json.Marshal(obj)
	return buildworkload.Dependency{UID: uid, SpecSHA256: authority.Digest(canonical)}, nil
}
func (r *BuildResources) getBuildObject(ctx context.Context, path string) (map[string]any, []byte, error) {
	raw, status, e := r.request(ctx, http.MethodGet, path, nil, false)
	if e != nil || status != http.StatusOK {
		return nil, nil, ErrBuildTransport
	}
	var obj map[string]any
	if authority.DecodeStrict(raw, &obj) != nil {
		return nil, nil, ErrBuildTransport
	}
	return obj, raw, nil
}
func (r *BuildResources) checkDependency(ctx context.Context, path string, expected buildworkload.Dependency) (map[string]any, error) {
	obj, raw, e := r.getBuildObject(ctx, path)
	if e != nil {
		return nil, e
	}
	actual, e := BuildObjectFingerprint(raw)
	if e != nil || actual != expected {
		return nil, ErrBuildTransport
	}
	return obj, nil
}
func mutationDigest(collections map[string][]map[string]any) (string, error) {
	if len(collections) != len(buildMutationPaths) {
		return "", ErrBuildTransport
	}
	normalized := map[string][]buildworkload.Dependency{}
	for _, path := range buildMutationPaths {
		items, ok := collections[path]
		if !ok || len(items) > 128 {
			return "", ErrBuildTransport
		}
		seen := map[string]bool{}
		entries := []buildworkload.Dependency{}
		for _, obj := range items {
			raw, _ := json.Marshal(obj)
			d, e := BuildObjectFingerprint(raw)
			if e != nil || seen[d.UID] {
				return "", ErrBuildTransport
			}
			seen[d.UID] = true
			entries = append(entries, d)
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].UID < entries[j].UID })
		normalized[path] = entries
	}
	raw, _ := json.Marshal(normalized)
	return authority.Digest(raw), nil
}
func (r *BuildResources) checkMutationInventory(ctx context.Context, expected string) error {
	collections := map[string][]map[string]any{}
	for _, path := range buildMutationPaths {
		obj, _, e := r.getBuildObject(ctx, path)
		if e != nil {
			return e
		}
		if obj["apiVersion"] != "admissionregistration.k8s.io/v1" {
			return ErrBuildTransport
		}
		metadata, ok := obj["metadata"].(map[string]any)
		if !ok {
			return ErrBuildTransport
		}
		if value, exists := metadata["continue"]; exists && value != "" {
			return ErrBuildTransport
		}
		items, ok := obj["items"].([]any)
		if !ok || len(items) > 128 {
			return ErrBuildTransport
		}
		collections[path] = []map[string]any{}
		for _, value := range items {
			item, ok := value.(map[string]any)
			if !ok {
				return ErrBuildTransport
			}
			collections[path] = append(collections[path], item)
		}
	}
	digest, e := mutationDigest(collections)
	if e != nil || digest != expected {
		return ErrBuildTransport
	}
	return nil
}

// inspectInstalledProfile is private until physical-slot checks and an explicit
// installer trust snapshot are integrated. A matching snapshot is a point-in-time
// observation; concurrent administrator mutation is outside this profile's TCB.
func (r *BuildResources) inspectInstalledProfile(ctx context.Context) error {
	if r == nil || r.profile.Validate() != nil {
		return ErrBuildTransport
	}
	observed := map[string]map[string]any{}
	for _, key := range buildworkload.DependencyKeys() {
		path, ok := dependencyPath(key)
		if !ok {
			return ErrBuildTransport
		}
		obj, e := r.checkDependency(ctx, path, r.profile.Dependencies[key])
		if e != nil {
			return e
		}
		observed[key] = obj
	}
	targets, e := BuildNetworkTargets(observed)
	if e != nil || !checkTargetEquality(r.profile.DeniedTargets, targets) {
		return ErrBuildTransport
	}
	if e := positiveBuildTargets(ctx, targets); e != nil {
		return e
	}
	config := observed["configMap/build-trust"]
	if config["immutable"] != true {
		return ErrBuildTransport
	}
	data, ok := config["data"].(map[string]any)
	if !ok || len(data) != 1 {
		return ErrBuildTransport
	}
	raw, ok := data["trust.json"].(string)
	if !ok || len(raw) > 65536 {
		return ErrBuildTransport
	}
	var trust BuildTrust
	if authority.DecodeStrict([]byte(raw), &trust) != nil || trust.Validate() != nil {
		return ErrBuildTransport
	}
	for key, path := range buildTrustPaths {
		obj, e := r.checkDependency(ctx, path, trust.Components[key])
		if e != nil {
			return e
		}
		if (key == "tekton-controller" || key == "kindnet") && !buildControllerReady(obj, key == "kindnet") {
			return ErrBuildTransport
		}
	}
	if e := r.checkMutationInventory(ctx, trust.MutationInventorySHA256); e != nil {
		return e
	}
	// A sealed digest cannot authorize a different admission definition. Compare
	// the installed own policies with this compiler's exact reviewed definitions.
	policy, binding, e := buildworkload.Admission(r.profile, r.slots)
	if e != nil {
		return ErrBuildTransport
	}
	mutation, mutationBinding := buildworkload.BoundsMutation()
	definitions := map[string][]byte{"validatingAdmissionPolicy/ai-sdlc-build-pods": policy, "validatingAdmissionPolicyBinding/ai-sdlc-build-pods": binding, "mutatingAdmissionPolicy/ai-sdlc-build-bounds": mutation, "mutatingAdmissionPolicyBinding/ai-sdlc-build-bounds": mutationBinding}
	for key, raw := range definitions {
		if !exactAdmissionDefinition(observed[key], raw) {
			return ErrBuildTransport
		}
	}
	return nil
}

func exactAdmissionDefinition(actual map[string]any, raw []byte) bool {
	var expected map[string]any
	if authority.DecodeStrict(raw, &expected) != nil || actual["apiVersion"] != expected["apiVersion"] || actual["kind"] != expected["kind"] {
		return false
	}
	spec := expected["spec"].(map[string]any)
	for _, key := range []string{"matchConstraints", "matchResources"} {
		if match, ok := spec[key].(map[string]any); ok {
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
	return reflect.DeepEqual(actual["spec"], spec)
}
