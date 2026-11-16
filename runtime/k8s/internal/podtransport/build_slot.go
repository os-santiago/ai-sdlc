package podtransport

import (
	"context"
	"reflect"
	"regexp"
	"time"
)

var buildPVName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

func (r *BuildResources) inspectBuildSlot(ctx context.Context) error {
	if r == nil || r.slot.Validate() != nil {
		return ErrBuildTransport
	}
	for name, uid := range map[string]string{"kube-system": r.slot.ClusterUID, r.slot.Namespace: r.slot.NamespaceUID} {
		obj, _, e := r.getBuildObject(ctx, "/api/v1/namespaces/"+name)
		if e != nil || !sameObjectUID(obj, uid) {
			return ErrBuildTransport
		}
	}
	pvc, _, e := r.getBuildObject(ctx, "/api/v1/namespaces/"+r.slot.Namespace+"/persistentvolumeclaims/"+r.slot.PVC)
	if e != nil || !sameObjectUID(pvc, r.slot.PVCUID) {
		return ErrBuildTransport
	}
	spec, ok := pvc["spec"].(map[string]any)
	if !ok {
		return ErrBuildTransport
	}
	volume, ok := spec["volumeName"].(string)
	if !ok || !buildPVName.MatchString(volume) {
		return ErrBuildTransport
	}
	expectedPVC := map[string]any{"accessModes": []any{"ReadWriteOnce"}, "resources": map[string]any{"requests": map[string]any{"storage": "4Gi"}}, "storageClassName": "", "volumeMode": "Filesystem", "volumeName": volume}
	status, ok := pvc["status"].(map[string]any)
	if !ok || status["phase"] != "Bound" || !reflect.DeepEqual(spec, expectedPVC) {
		return ErrBuildTransport
	}
	pv, _, e := r.getBuildObject(ctx, "/api/v1/persistentvolumes/"+volume)
	if e != nil || !sameObjectUID(pv, r.slot.PVUID) {
		return ErrBuildTransport
	}
	pvSpec, ok := pv["spec"].(map[string]any)
	if !ok {
		return ErrBuildTransport
	}
	reference, ok := pvSpec["claimRef"].(map[string]any)
	if !ok || reference["uid"] != r.slot.PVCUID || reference["name"] != r.slot.PVC || reference["namespace"] != r.slot.Namespace || reference["apiVersion"] != "v1" || reference["kind"] != "PersistentVolumeClaim" {
		return ErrBuildTransport
	}
	// resourceVersion is API bind bookkeeping; all other claim fields stay exact.
	expectedRef := map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "name": r.slot.PVC, "namespace": r.slot.Namespace, "uid": r.slot.PVCUID}
	if version, exists := reference["resourceVersion"]; exists {
		if text, ok := version.(string); !ok || text == "" {
			return ErrBuildTransport
		}
		expectedRef["resourceVersion"] = version
	}
	expectedPV := map[string]any{"accessModes": []any{"ReadWriteOnce"}, "capacity": map[string]any{"storage": "4Gi"}, "claimRef": expectedRef, "persistentVolumeReclaimPolicy": "Retain", "storageClassName": "", "volumeMode": "Filesystem", "local": map[string]any{"path": "/var/local/ai-sdlc/build-slots/" + r.slot.StorageIdentitySHA256}, "nodeAffinity": map[string]any{"required": map[string]any{"nodeSelectorTerms": []any{map[string]any{"matchExpressions": []any{map[string]any{"key": "kubernetes.io/hostname", "operator": "In", "values": []any{r.slot.Node}}}}}}}}
	delete(expectedPV, "storageClassName") // Empty string is omitted by core/v1 PV serialization.
	if !reflect.DeepEqual(pvSpec, expectedPV) {
		return ErrBuildTransport
	}
	node, _, e := r.getBuildObject(ctx, "/api/v1/nodes/"+r.slot.Node)
	if e != nil || !sameObjectUID(node, r.slot.NodeUID) {
		return ErrBuildTransport
	}
	status, ok = node["status"].(map[string]any)
	if !ok {
		return ErrBuildTransport
	}
	info, ok := status["nodeInfo"].(map[string]any)
	if !ok || info["bootID"] != r.slot.NodeBootID {
		return ErrBuildTransport
	}
	conditions, ok := status["conditions"].([]any)
	if !ok {
		return ErrBuildTransport
	}
	ready := false
	for _, raw := range conditions {
		condition, ok := raw.(map[string]any)
		if !ok {
			return ErrBuildTransport
		}
		if condition["type"] == "Ready" {
			ready = condition["status"] == "True"
		}
	}
	if !ready {
		return ErrBuildTransport
	}
	lease, _, e := r.getBuildObject(ctx, "/apis/coordination.k8s.io/v1/namespaces/kube-node-lease/leases/"+r.slot.Node)
	if e != nil {
		return ErrBuildTransport
	}
	metadata, ok := lease["metadata"].(map[string]any)
	if !ok || metadata["deletionTimestamp"] != nil {
		return ErrBuildTransport
	}
	owners, ok := metadata["ownerReferences"].([]any)
	if !ok || len(owners) != 1 {
		return ErrBuildTransport
	}
	owner, ok := owners[0].(map[string]any)
	if !ok || owner["kind"] != "Node" || owner["name"] != r.slot.Node || owner["uid"] != r.slot.NodeUID {
		return ErrBuildTransport
	}
	leaseSpec, ok := lease["spec"].(map[string]any)
	if !ok || leaseSpec["holderIdentity"] != r.slot.Node {
		return ErrBuildTransport
	}
	renewal, ok := leaseSpec["renewTime"].(string)
	if !ok {
		return ErrBuildTransport
	}
	at, e := time.Parse(time.RFC3339Nano, renewal)
	now := time.Now()
	if e != nil || at.Before(now.Add(-30*time.Second)) || at.After(now.Add(5*time.Second)) {
		return ErrBuildTransport
	}
	return nil
}
func sameObjectUID(obj map[string]any, uid string) bool {
	if !resourceUID.MatchString(uid) {
		return false
	}
	metadata, ok := obj["metadata"].(map[string]any)
	return ok && metadata["uid"] == uid && metadata["deletionTimestamp"] == nil
}
