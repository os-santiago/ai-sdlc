package podtransport

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildworkload"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

var cleanupBuildID = regexp.MustCompile(`^build-[a-f0-9]{64}$`)

// CleanupQueuedBuild obtains only archived original identities from a terminal
// journal claim. Freeze/expiry cannot grant execution, or prevent containment.
// No storage is erased or reused; unknown creates remain quarantined.
func CleanupQueuedBuild(ctx context.Context, client *Client, journal *store.BuildJournal, claim store.BuildClaim) error {
	if client == nil || client.http == nil || client.config == nil || journal == nil || journal.DB == nil {
		return ErrBuildTransport
	}
	original, err := journal.ReadBuildCleanup(ctx, claim)
	if err != nil {
		return err
	}
	proof, err := cleanupBuildResources(ctx, client, original)
	if err != nil {
		return err
	}
	return journal.RecordBuildCleanup(ctx, claim, proof)
}

func cleanupBuildResources(ctx context.Context, client *Client, original store.BuildCleanup) (store.BuildCleanupProof, error) {
	var proof store.BuildCleanupProof
	if !cleanupBuildID.MatchString(original.Claim.ID) || original.Slot.Validate() != nil || original.Claim.SlotID != original.Slot.ID() || original.Claim.SlotDigest != original.Slot.Digest() || len(original.Resources) > 2 {
		return proof, ErrBuildTransport
	}
	ctx, cancel := context.WithTimeout(ctx, 240*time.Second)
	defer cancel()
	r := &BuildResources{client: client, slot: original.Slot}
	get := func(ctx context.Context, path string) (map[string]any, []byte, error) {
		return r.getBuildObjectMode(ctx, path, true)
	}
	seen := map[string]bool{}
	for _, resource := range original.Resources {
		if (resource.Kind != "build" && resource.Kind != "publish") || seen[resource.Kind] || !resourceUID.MatchString(resource.UID) || len(original.Claim.ID) != len("build-")+64 || resource.Name != "ai-sdlc-"+resource.Kind+"-"+original.Claim.ID[len("build-"):len("build-")+32] {
			return proof, ErrBuildTransport
		}
		seen[resource.Kind] = true
	}
	// Delete only original acknowledged roots, foreground and with UID CAS.
	// Do not strip finalizers, force-delete Pods, or adopt unknown descendants.
	for _, resource := range original.Resources {
		for name, uid := range map[string]string{"kube-system": original.Slot.ClusterUID, original.Slot.Namespace: original.Slot.NamespaceUID} {
			object, _, err := get(ctx, "/api/v1/namespaces/"+name)
			if err != nil || !sameObjectUID(object, uid) {
				return proof, ErrBuildTransport
			}
		}
		body, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "DeleteOptions", "preconditions": map[string]any{"uid": resource.UID}, "propagationPolicy": "Foreground"})
		_, status, err := r.request(ctx, http.MethodDelete, runPath(resource.Name), body, true)
		if err != nil || (status != http.StatusOK && status != http.StatusAccepted && status != http.StatusNotFound) {
			return proof, ErrBuildTransport
		}
	}
	if original.UnknownCreate {
		return proof, ErrUnknownBuildCreate
	}
	for {
		if r.inspectBuildSlotWith(ctx, get) != nil {
			return proof, ErrBuildTransport
		}
		absent := true
		for _, resource := range original.Resources {
			raw, status, err := r.request(ctx, http.MethodGet, runPath(resource.Name), nil, true)
			if err != nil {
				return proof, ErrBuildTransport
			}
			if status == http.StatusNotFound {
				continue
			}
			var object map[string]any
			if status != http.StatusOK || authority.DecodeStrict(raw, &object) != nil {
				return proof, ErrBuildTransport
			}
			meta, ok := object["metadata"].(map[string]any)
			if !ok || object["apiVersion"] != "tekton.dev/v1" || object["kind"] != "PipelineRun" || meta["uid"] != resource.UID || meta["name"] != resource.Name || meta["namespace"] != buildworkload.Namespace {
				return proof, ErrBuildTransport
			}
			absent = false
		}
		tasks, err := r.listBuildObjectsWith(ctx, "/apis/tekton.dev/v1/namespaces/"+buildworkload.Namespace+"/taskruns", get)
		if err != nil {
			return proof, err
		}
		for _, task := range tasks {
			for _, resource := range original.Resources {
				if objectOwnerUID(task, "PipelineRun", resource.UID) || buildLabel(task, "ai-sdlc.build-id") == original.Claim.ID[6:38] {
					absent = false
				}
			}
		}
		// A detached, substituted or unrelated PVC consumer also keeps the slot
		// quarantined. Its identity is not cleanup authority and is never deleted.
		pods, err := r.listBuildObjectsWith(ctx, "/api/v1/namespaces/"+buildworkload.Namespace+"/pods", get)
		if err != nil {
			return proof, err
		}
		for _, pod := range pods {
			spec, ok := pod["spec"].(map[string]any)
			if !ok {
				return proof, ErrBuildTransport
			}
			if volumes, exists := spec["volumes"]; exists {
				values, ok := volumes.([]any)
				if !ok {
					return proof, ErrBuildTransport
				}
				for _, value := range values {
					volume, ok := value.(map[string]any)
					if !ok {
						return proof, ErrBuildTransport
					}
					if raw, exists := volume["persistentVolumeClaim"]; exists {
						pvc, ok := raw.(map[string]any)
						if !ok {
							return proof, ErrBuildTransport
						}
						if pvc["claimName"] == original.Slot.PVC {
							absent = false
						}
					}
				}
			}
		}
		if absent {
			if r.inspectBuildSlotWith(ctx, get) != nil {
				return proof, ErrBuildTransport
			}
			return store.BuildCleanupProof{Version: "ai-sdlc.build-cleanup/v1", BuildID: original.Claim.ID, SlotDigest: original.Claim.SlotDigest, Resources: original.Resources, UnknownCreate: false, ObservedAbsentAt: time.Now().UnixMilli()}, nil
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return proof, ctx.Err()
		case <-timer.C:
		}
	}
}
