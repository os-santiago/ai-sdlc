package buildworkload

import "encoding/json"

// BoundsMutation constrains Tekton-created helper storage and the kubelet's
// deadline. Mandatory validation independently rejects an unbounded final Pod.
// Kubernetes must expose native MutatingAdmissionPolicy v1 (1.36+).
func BoundsMutation() ([]byte, []byte) {
	expression := `object.spec.volumes.filter(v,has(v.emptyDir)).map(v,JSONPatch{op:'add',path:'/spec/volumes/'+string(object.spec.volumes.indexOf(v))+'/emptyDir',value:{'medium':'Memory','sizeLimit':v.name == 'tmp' ? '64Mi' : (v.name == 'tekton-internal-bin' ? '96Mi' : '16Mi')}}) + [JSONPatch{op:'add',path:'/spec/activeDeadlineSeconds',value:object.metadata.labels['ai-sdlc.role'] == 'build' ? 600 : 300}]`
	policy := object{"apiVersion": "admissionregistration.k8s.io/v1", "kind": "MutatingAdmissionPolicy", "metadata": object{"name": "ai-sdlc-build-bounds"}, "spec": object{"failurePolicy": "Fail", "reinvocationPolicy": "IfNeeded", "matchConstraints": object{"resourceRules": []object{{"apiGroups": []string{""}, "apiVersions": []string{"v1"}, "operations": []string{"CREATE"}, "resources": []string{"pods"}}}}, "matchConditions": []object{{"name": "owned-phase", "expression": "has(object.metadata.labels) && 'ai-sdlc.role' in object.metadata.labels && object.metadata.labels['ai-sdlc.role'] in ['build','publish']"}}, "mutations": []object{{"patchType": "JSONPatch", "jsonPatch": object{"expression": expression}}}}}
	binding := object{"apiVersion": "admissionregistration.k8s.io/v1", "kind": "MutatingAdmissionPolicyBinding", "metadata": object{"name": "ai-sdlc-build-bounds"}, "spec": object{"policyName": "ai-sdlc-build-bounds", "matchResources": object{"namespaceSelector": object{"matchLabels": object{"kubernetes.io/metadata.name": Namespace}}}}}
	a, _ := json.Marshal(policy)
	b, _ := json.Marshal(binding)
	return a, b
}
