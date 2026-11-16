package buildworkload

import (
	"encoding/base64"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
)

const AdmissionName = "ai-sdlc-build-pods"
const resultNames = "IMAGE_URL,IMAGE_DIGEST,CHAINS-GIT_URL,CHAINS-GIT_COMMIT,DEFINITION_DIGEST,RUN_UID"
const ControllerUser = "system:serviceaccount:tekton-pipelines:tekton-pipelines-controller"

func cel(v any) string { raw, _ := json.Marshal(v); return string(raw) }
func guardArguments(s building.Slot, phase string) []string {
	args := []string{"-wait_file", "/tekton/downward/ready", "-wait_file_content", "-post_file", "/tekton/run/0/out", "-termination_path", "/tekton/termination", "-step_metadata_dir", "/tekton/run/0/status"}
	if phase == "publish" {
		args = append(args, "-docker-config=registry-auth")
	}
	args = append(args, "-timeout", "30s")
	if phase == "publish" {
		args = append(args, "-results", resultNames)
	}
	return append(args, "-entrypoint", "/component", "--", "--guard-workspace", "--storage-identity="+s.StorageIdentitySHA256, "--node-boot-id="+s.NodeBootID, "--workspace-phase="+phase)
}

// Admission generates the mandatory post-mutation Pod envelope. Exact dynamic
// source hashes are supplied only by the trusted compiler/controller; the
// transport also verifies the admitted hierarchy against its persisted plan.
// Canonical controller and mutating-admission inventories must be installed and
// rechecked separately. This policy is not a defense against cluster admins.
func Admission(p Profile, slots []building.Slot) ([]byte, []byte, error) {
	if p.Validate() != nil || len(slots) < 1 || len(slots) > 8 {
		return nil, nil, ErrWorkload
	}
	pvcs := []string{}
	nodes := []string{}
	guardOptions := []string{}
	for _, s := range slots {
		if s.Validate() != nil || s.ClusterUID != p.ClusterUID || s.NamespaceUID != p.NamespaceUID {
			return nil, nil, ErrWorkload
		}
		pvcs = append(pvcs, s.PVC)
		nodes = append(nodes, s.Node)
		for _, phase := range []string{"build", "publish"} {
			guardOptions = append(guardOptions, "(variables.phase == "+cel(phase)+" && c.args == "+cel(guardArguments(s, phase))+" + ['--authority-expires-at=' + object.metadata.annotations['ai-sdlc.expires-at']] && object.spec.volumes.exists(v, v.name == 'slot' && v.persistentVolumeClaim.claimName == "+cel(s.PVC)+") && object.spec.nodeSelector['kubernetes.io/hostname'] == "+cel(s.Node)+")")
		}
	}
	script := base64.StdEncoding.EncodeToString([]byte(buildScript(p)))
	wrapperPrefix := "scriptfile=\"/tekton/scripts/script-1-"
	wrapperSuffix := "\"\ntouch ${scriptfile} && chmod +x ${scriptfile}\ncat > ${scriptfile} << '_EOF_'\n" + script + "\n_EOF_\n/tekton/bin/entrypoint decode-script \"${scriptfile}\"\n"
	scriptPattern := "^" + regexp.QuoteMeta(wrapperPrefix) + "[a-z0-9]{5}" + regexp.QuoteMeta(wrapperSuffix) + "$"
	checks := []string{
		"request.userInfo.username == " + cel(ControllerUser) + " && (!has(request.subResource) || request.subResource == '')",
		"has(object.metadata.labels) && 'ai-sdlc.role' in object.metadata.labels && variables.phase in ['build','publish'] && object.spec.serviceAccountName == variables.phase",
		"has(object.spec.automountServiceAccountToken) && !object.spec.automountServiceAccountToken && (!has(object.spec.enableServiceLinks) || !object.spec.enableServiceLinks)",
		"(!has(object.spec.hostNetwork) || !object.spec.hostNetwork) && (!has(object.spec.hostPID) || !object.spec.hostPID) && (!has(object.spec.hostIPC) || !object.spec.hostIPC) && (!has(object.spec.shareProcessNamespace) || !object.spec.shareProcessNamespace)",
		"object.spec.restartPolicy == 'Never' && has(object.spec.nodeSelector) && object.spec.nodeSelector['kubernetes.io/hostname'] in " + cel(nodes),
		"has(object.spec.securityContext) && object.spec.securityContext.runAsNonRoot && object.spec.securityContext.runAsUser == 1000 && object.spec.securityContext.runAsGroup == 1000 && object.spec.securityContext.seccompProfile.type == 'RuntimeDefault' && !has(object.spec.securityContext.fsGroup) && (!has(object.spec.securityContext.supplementalGroups) || size(object.spec.securityContext.supplementalGroups) == 0)",
		"(!has(object.spec.ephemeralContainers) || size(object.spec.ephemeralContainers) == 0) && size(object.spec.containers) == 2 && object.spec.containers[0].name == 'step-custody' && object.spec.containers[1].name == (variables.phase == 'build' ? 'step-verify' : 'step-publish')",
		"size(object.spec.initContainers) == (variables.phase == 'build' ? 2 : 1) && object.spec.initContainers[0].name == 'prepare' && (variables.phase != 'build' || object.spec.initContainers[1].name == 'place-scripts')",
		"(object.spec.containers + object.spec.initContainers).all(c, has(c.securityContext) && c.securityContext.allowPrivilegeEscalation == false && c.securityContext.readOnlyRootFilesystem && c.securityContext.runAsNonRoot && (!has(c.securityContext.runAsUser) || c.securityContext.runAsUser == 1000) && (!has(c.securityContext.runAsGroup) || c.securityContext.runAsGroup == 1000) && (!has(c.securityContext.privileged) || !c.securityContext.privileged) && c.securityContext.capabilities.drop == ['ALL'] && (!has(c.securityContext.capabilities.add) || size(c.securityContext.capabilities.add) == 0) && (!has(c.securityContext.procMount) || c.securityContext.procMount == 'Default') && (!has(c.securityContext.seccompProfile) || c.securityContext.seccompProfile.type == 'RuntimeDefault') && !has(c.restartPolicy) && !has(c.lifecycle) && !has(c.livenessProbe) && !has(c.readinessProbe) && !has(c.startupProbe) && (!has(c.volumeDevices) || size(c.volumeDevices) == 0) && (!has(c.ports) || size(c.ports) == 0) && (!has(c.envFrom) || size(c.envFrom) == 0))",
		"object.spec.containers.all(c, c.command == ['/tekton/bin/entrypoint'] && c.image == (c.name == 'step-verify' ? " + cel(GoImage) + " : " + cel(p.PublisherImage) + "))",
		"object.spec.initContainers[0].image == " + cel(EntrypointImage) + " && object.spec.initContainers[0].command == ['/ko-app/entrypoint','init','/ko-app/entrypoint','/tekton/bin/entrypoint','step-custody', (variables.phase == 'build' ? 'step-verify' : 'step-publish')] && (!has(object.spec.initContainers[0].args) || size(object.spec.initContainers[0].args) == 0)",
		"variables.phase != 'build' || (object.spec.initContainers[1].image == " + cel(ScriptImage) + " && object.spec.initContainers[1].command == ['sh'] && size(object.spec.initContainers[1].args) == 2 && object.spec.initContainers[1].args[0] == '-c' && object.spec.initContainers[1].args[1].matches(" + cel(scriptPattern) + "))",
		"object.spec.containers.filter(c, c.name == 'step-custody').all(c, " + strings.Join(guardOptions, " || ") + ")",
		"object.spec.containers.filter(c, c.name == 'step-verify').all(c, size(c.args) == 13 && c.args[0] == '-wait_file' && c.args[1] == '/tekton/run/0/out' && c.args[2] == '-post_file' && c.args[3] == '/tekton/run/1/out' && c.args[4] == '-termination_path' && c.args[5] == '/tekton/termination' && c.args[6] == '-step_metadata_dir' && c.args[7] == '/tekton/run/1/status' && c.args[8] == '-timeout' && c.args[9] == '9m0s' && c.args[10] == '-entrypoint' && c.args[11].matches('^/tekton/scripts/script-1-[a-z0-9]{5}$') && c.args[12] == '--')",
		"(object.spec.containers + object.spec.initContainers).all(c, !has(c.env) || c.env.all(e, !has(e.valueFrom) && ((e.name == 'SSL_CERT_DIR' && e.value == '/tekton-custom-certs:/etc/ssl/certs:/etc/pki/tls/certs') || (c.name == 'step-verify' && e.name in ['AI_SDLC_SOURCE_SHA','AI_SDLC_SOURCE_TREE'] && e.value.matches('^[a-f0-9]{40}$')))))",
		"object.spec.containers.filter(c,c.name == 'step-verify').all(c, c.env.filter(e,e.name == 'AI_SDLC_SOURCE_SHA').size() == 1 && c.env.filter(e,e.name == 'AI_SDLC_SOURCE_TREE').size() == 1)",
		"object.spec.volumes.size() <= 20 && object.spec.volumes.all(v, (v.name == 'slot' && has(v.persistentVolumeClaim) && v.persistentVolumeClaim.claimName in " + cel(pvcs) + " && (variables.phase == 'publish' ? (has(v.persistentVolumeClaim.readOnly) && v.persistentVolumeClaim.readOnly) : (!has(v.persistentVolumeClaim.readOnly) || !v.persistentVolumeClaim.readOnly))) || (v.name in ['tmp','tekton-internal-workspace','tekton-internal-home','tekton-internal-results','tekton-internal-steps','tekton-internal-artifacts','tekton-internal-scripts','tekton-internal-bin','tekton-internal-run-0','tekton-internal-run-1','tekton-creds-init-home-0','tekton-creds-init-home-1'] && has(v.emptyDir) && has(v.emptyDir.medium) && has(dyn(v.emptyDir).sizeLimit) && v.emptyDir.medium == 'Memory' && dyn(v.emptyDir).sizeLimit == (v.name == 'tmp' ? '64Mi' : (v.name == 'tekton-internal-bin' ? '96Mi' : '16Mi'))) || (v.name == 'tekton-internal-downward' && has(v.downwardAPI) && v.downwardAPI.items.size() == 1 && v.downwardAPI.items[0].path == 'ready' && v.downwardAPI.items[0].fieldRef.fieldPath == \"metadata.annotations['tekton.dev/ready']\") || (variables.phase == 'publish' && (v.name == 'registry-auth' || v.name.matches('^tekton-creds-[a-f0-9]{8}$')) && has(v.secret) && v.secret.secretName == 'registry-auth') || (has(v.configMap) && ((variables.phase == 'publish' && v.name == 'ca' && v.configMap.name == 'trusted-ca') || (v.name == 'config-trusted-cabundle-volume' && v.configMap.name == 'config-trusted-cabundle') || (v.name == 'config-service-cabundle-volume' && v.configMap.name == 'config-service-cabundle'))))",
		"object.spec.volumes.filter(v,v.name == 'slot').size() == 1 && (object.spec.containers + object.spec.initContainers).all(c, !has(c.volumeMounts) || c.volumeMounts.all(m, m.name != 'slot' || (m.mountPath == '/slot' && !has(m.subPath) && !has(m.subPathExpr) && (variables.phase == 'publish' ? (has(m.readOnly) && m.readOnly) : (!has(m.readOnly) || !m.readOnly)))))",
	}
	checks = append(checks, "has(object.spec.activeDeadlineSeconds) && object.spec.activeDeadlineSeconds == (variables.phase == 'build' ? 600 : 300)")
	checks = append(checks, "object.spec.terminationGracePeriodSeconds <= 30 && object.spec.terminationGracePeriodSeconds >= 0 && (request.operation == 'CREATE' ? (!has(object.spec.nodeName) || object.spec.nodeName == '') : ((!has(object.spec.nodeName) || object.spec.nodeName == '') ? (!has(oldObject.spec.nodeName) || oldObject.spec.nodeName == '') : (object.spec.nodeName == oldObject.spec.nodeName && object.spec.nodeName == object.spec.nodeSelector['kubernetes.io/hostname']))) && !has(object.spec.runtimeClassName) && object.spec.schedulerName == 'default-scheduler' && (!has(object.spec.priorityClassName) || object.spec.priorityClassName == '') && (!has(object.spec.priority) || object.spec.priority == 0) && !has(object.spec.affinity) && (!has(object.spec.hostAliases) || object.spec.hostAliases.size() == 0) && object.spec.nodeSelector.size() == 1 && object.spec.dnsPolicy == 'ClusterFirst' && !has(object.spec.dnsConfig)")
	checks = append(checks, "(!has(object.spec.imagePullSecrets) || object.spec.imagePullSecrets.size() == 0) && (!has(object.spec.resourceClaims) || object.spec.resourceClaims.size() == 0) && (!has(object.spec.tolerations) || object.spec.tolerations.all(t, t.key in ['node.kubernetes.io/not-ready','node.kubernetes.io/unreachable'] && t.operator == 'Exists' && t.effect == 'NoExecute' && t.tolerationSeconds == 300 && (!has(t.value) || t.value == '')))")
	checks = append(checks, "has(object.metadata.annotations) && object.metadata.annotations['ai-sdlc.expires-at'].matches('^[1-9][0-9]{12}$')")
	checks = append(checks, mountChecks()...)
	checks = append(checks, resourceChecks()...)
	checks = append(checks, publisherArguments())
	expanded := []string{}
	for _, x := range checks {
		if strings.Contains(x, "(object.spec.containers + object.spec.initContainers)") {
			for _, field := range []string{"object.spec.containers", "object.spec.initContainers"} {
				expanded = append(expanded, strings.ReplaceAll(x, "(object.spec.containers + object.spec.initContainers)", field))
			}
		} else {
			expanded = append(expanded, x)
		}
	}
	checks = expanded
	validations := []object{}
	for i, x := range checks {
		validations = append(validations, object{"expression": x, "message": "Closed build Pod requirement " + string(rune('A'+i)) + " denied"})
	}
	policy := object{"apiVersion": "admissionregistration.k8s.io/v1", "kind": "ValidatingAdmissionPolicy", "metadata": object{"name": AdmissionName}, "spec": object{"failurePolicy": "Fail", "matchConstraints": object{"resourceRules": []object{{"apiGroups": []string{""}, "apiVersions": []string{"v1"}, "operations": []string{"CREATE", "UPDATE"}, "resources": []string{"pods", "pods/ephemeralcontainers"}}}}, "variables": []object{{"name": "phase", "expression": "has(object.metadata.labels) && 'ai-sdlc.role' in object.metadata.labels ? object.metadata.labels['ai-sdlc.role'] : ''"}}, "validations": validations}}
	binding := object{"apiVersion": "admissionregistration.k8s.io/v1", "kind": "ValidatingAdmissionPolicyBinding", "metadata": object{"name": AdmissionName}, "spec": object{"policyName": AdmissionName, "validationActions": []string{"Deny"}, "matchResources": object{"namespaceSelector": object{"matchLabels": object{"kubernetes.io/metadata.name": Namespace}}}}}
	a, _ := json.Marshal(policy)
	b, _ := json.Marshal(binding)
	return a, b, nil
}
