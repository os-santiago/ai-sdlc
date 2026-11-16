package podtransport

import (
	"context"
	"encoding/json"
	"reflect"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildworkload"
)

// Installation observations are read-only and do not manufacture a build
// command, signature, source or claim. Their deadline only bounds API reads.
func installedBuildReader(client *Client) (*BuildResources, error) {
	if client == nil || client.http == nil || client.config == nil {
		return nil, ErrBuildTransport
	}
	return &BuildResources{client: client, compiled: buildworkload.Compiled{ExpiresAt: time.Now().Add(2 * time.Minute)}}, nil
}

func (r *BuildResources) installationScope(ctx context.Context, slots []building.Slot) error {
	if len(slots) < 1 || len(slots) > 8 {
		return ErrBuildTransport
	}
	seen := map[string]bool{}
	for _, s := range slots {
		if s.Validate() != nil || seen[s.ID()] || s.ClusterUID != slots[0].ClusterUID || s.NamespaceUID != slots[0].NamespaceUID {
			return ErrBuildTransport
		}
		seen[s.ID()] = true
		r.slot = s
		if e := r.inspectBuildSlot(ctx); e != nil {
			return e
		}
	}
	return nil
}

// ObserveBuildAdmission collects actual API/registry targets after checking
// original storage/cluster custody. It does not write admission resources.
func ObserveBuildAdmission(ctx context.Context, client *Client, slots []building.Slot, publisher string) (buildworkload.AdmissionConfiguration, error) {
	r, e := installedBuildReader(client)
	if e != nil {
		return buildworkload.AdmissionConfiguration{}, e
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if e = r.installationScope(ctx, slots); e != nil {
		return buildworkload.AdmissionConfiguration{}, e
	}
	objects := map[string]map[string]any{}
	for _, key := range []string{"default/service/kubernetes", "default/endpoints/kubernetes", "kind-registry/service/registry-service", "kind-registry/endpoints/registry-service"} {
		path, _ := dependencyPath(key)
		obj, _, e := r.getBuildObject(ctx, path)
		if e != nil {
			return buildworkload.AdmissionConfiguration{}, e
		}
		objects[key] = obj
	}
	targets, e := BuildNetworkTargets(objects)
	if e != nil {
		return buildworkload.AdmissionConfiguration{}, e
	}
	c := buildworkload.AdmissionConfiguration{Version: "ai-sdlc.build-admission-input/v1", ClusterUID: slots[0].ClusterUID, NamespaceUID: slots[0].NamespaceUID, PublisherImage: publisher, DeniedTargets: targets}
	if c.Validate() != nil {
		return buildworkload.AdmissionConfiguration{}, ErrBuildTransport
	}
	return c, nil
}

// ObserveBuildTrust returns a candidate inventory for administrative review.
// Observation is not approval. Runtime code must never use it to relearn drift.
func ObserveBuildTrust(ctx context.Context, client *Client, slots []building.Slot) (BuildTrust, error) {
	r, e := installedBuildReader(client)
	if e != nil {
		return BuildTrust{}, e
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if e = r.installationScope(ctx, slots); e != nil {
		return BuildTrust{}, e
	}
	trust := BuildTrust{Version: "ai-sdlc.build-trust/v1", Components: map[string]buildworkload.Dependency{}}
	for key, path := range buildTrustPaths {
		obj, raw, e := r.getBuildObject(ctx, path)
		if e != nil {
			return BuildTrust{}, e
		}
		if (key == "tekton-controller" || key == "kindnet") && !buildControllerReady(obj, key == "kindnet") {
			return BuildTrust{}, ErrBuildTransport
		}
		d, e := BuildObjectFingerprint(raw)
		if e != nil {
			return BuildTrust{}, e
		}
		trust.Components[key] = d
	}
	collections := map[string][]map[string]any{}
	for _, path := range buildMutationPaths {
		items, e := r.listBuildObjects(ctx, path)
		if e != nil {
			return BuildTrust{}, e
		}
		collections[path] = items
	}
	trust.MutationInventorySHA256, e = mutationDigest(collections)
	if e != nil || trust.Validate() != nil {
		return BuildTrust{}, ErrBuildTransport
	}
	return trust, nil
}

// ObserveBuildProfile binds a separately approved trust inventory to the exact
// installed compiler policies and current original storage custody. It grants
// no execution permission and never alters the approved trust on mismatch.
func ObserveBuildProfile(ctx context.Context, client *Client, input buildworkload.AdmissionConfiguration, slots []building.Slot, approved BuildTrust) (buildworkload.Profile, error) {
	if input.Validate() != nil || approved.Validate() != nil {
		return buildworkload.Profile{}, ErrBuildTransport
	}
	r, e := installedBuildReader(client)
	if e != nil {
		return buildworkload.Profile{}, e
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if e = r.installationScope(ctx, slots); e != nil {
		return buildworkload.Profile{}, e
	}
	if slots[0].ClusterUID != input.ClusterUID || slots[0].NamespaceUID != input.NamespaceUID {
		return buildworkload.Profile{}, ErrBuildTransport
	}
	p := buildworkload.Profile{Version: "ai-sdlc.saferus-build-profile/v1", ClusterUID: input.ClusterUID, NamespaceUID: input.NamespaceUID, PublisherImage: input.PublisherImage, DeniedTargets: input.DeniedTargets, Dependencies: map[string]buildworkload.Dependency{}}
	for _, key := range buildworkload.DependencyKeys() {
		path, _ := dependencyPath(key)
		obj, raw, e := r.getBuildObject(ctx, path)
		if e != nil {
			return buildworkload.Profile{}, e
		}
		if key == "configMap/build-trust" {
			data, ok := obj["data"].(map[string]any)
			if !ok {
				return buildworkload.Profile{}, ErrBuildTransport
			}
			text, ok := data["trust.json"].(string)
			if !ok {
				return buildworkload.Profile{}, ErrBuildTransport
			}
			var actual BuildTrust
			if authority.DecodeStrict([]byte(text), &actual) != nil || !reflect.DeepEqual(actual, approved) {
				return buildworkload.Profile{}, ErrBuildTransport
			}
		}
		d, e := BuildObjectFingerprint(raw)
		if e != nil {
			return buildworkload.Profile{}, e
		}
		p.Dependencies[key] = d
	}
	r.profile = p
	r.slots = append([]building.Slot(nil), slots...)
	if e = r.inspectInstalledProfile(ctx); e != nil {
		return buildworkload.Profile{}, e
	}
	if e = r.installationScope(ctx, slots); e != nil {
		return buildworkload.Profile{}, e
	}
	// Return a detached public value; client credentials and Secret bytes never
	// appear in this output. Fingerprints bind their exact observed content.
	raw, _ := json.Marshal(p)
	var result buildworkload.Profile
	if authority.DecodeStrict(raw, &result) != nil {
		return buildworkload.Profile{}, ErrBuildTransport
	}
	return result, nil
}
