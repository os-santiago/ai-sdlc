package podtransport

import (
	"context"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
	core "k8s.io/api/core/v1"
)

// Metadata of a sealed dependency cache claim. A trusted dependency task
// fills the cache in the CI namespace; tools/dependency_cache_install.py seal
// then hands the volume over to the verifier namespace as a claim that is
// labelled sealed from creation, so nothing there can write to it.
const (
	DependencyKeyAnnotation = "ai-sdlc.io/dependency-key"
	DependencyStateLabel    = "ai-sdlc.io/dependency-state"
	DependencySealed        = "sealed"
)

// CheckDependencyCache reports whether the verifier namespace holds the
// sealed, bound cache claim for key. It is read-only: the controller never
// creates, changes or deletes a cache, and a missing or unsealed cache stops
// the verification before any pod exists instead of leaving pods pending.
func (client *Client) CheckDependencyCache(ctx context.Context, key string) error {
	claim := project.DependencyClaimName(key)
	if client == nil || client.http == nil || client.config == nil || claim == "" {
		return ErrTransport
	}
	r := &Resources{client: client, binding: Binding{Namespace: VerifierNamespace}}
	raw, err := r.fetch(ctx, r.url("persistentvolumeclaims", claim), "application/json", 65536)
	if err != nil {
		return err
	}
	var pvc core.PersistentVolumeClaim
	if authority.DecodeStrict(raw, &pvc) != nil || pvc.Name != claim || pvc.Namespace != VerifierNamespace ||
		pvc.DeletionTimestamp != nil || pvc.Status.Phase != core.ClaimBound || pvc.Spec.VolumeName == "" ||
		pvc.Annotations[DependencyKeyAnnotation] != key || pvc.Labels[DependencyStateLabel] != DependencySealed ||
		len(pvc.Spec.AccessModes) == 0 {
		return ErrTransport
	}
	// Pods attach the claim read-only; a ReadWriteMany volume could still be
	// written by a pod elsewhere, so only ReadOnlyMany and ReadWriteOnce
	// (bound to the verifier namespace's read-only pods) are accepted.
	for _, mode := range pvc.Spec.AccessModes {
		if mode != core.ReadOnlyMany && mode != core.ReadWriteOnce {
			return ErrTransport
		}
	}
	return nil
}
