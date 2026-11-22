package podtransport

import (
	"strings"
	"testing"

	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func compiledPod() core.Pod {
	user := int64(1000)
	return core.Pod{
		ObjectMeta: meta.ObjectMeta{Name: "executor", Annotations: map[string]string{"ai-sdlc.assignment-sha256": "a"}},
		Spec:       core.PodSpec{SecurityContext: &core.PodSecurityContext{RunAsUser: &user, FSGroup: &user}},
	}
}

func openShiftAdmitted(p core.Pod) core.Pod {
	p = *p.DeepCopy()
	p.Annotations["openshift.io/scc"] = "nonroot-v2"
	p.Annotations["seccomp.security.alpha.kubernetes.io/pod"] = "runtime/default"
	p.Annotations["security.openshift.io/validated-scc-subject-type"] = "serviceaccount"
	p.Annotations["k8s.ovn.org/pod-networks"] = `{"default":{"ip_addresses":["10.217.0.60/23"]}}`
	p.Annotations["k8s.v1.cni.cncf.io/network-status"] = "[{\n \"name\": \"ovn-kubernetes\"\n}]"
	p.Spec.SecurityContext.SELinuxOptions = &core.SELinuxOptions{Level: "s0:c26,c5"}
	p.Spec.ImagePullSecrets = []core.LocalObjectReference{{Name: "default-dockercfg-bntkq"}}
	p.Spec.NodeName = "node"
	return p
}

func withPlatform(t *testing.T, openShift bool) {
	previous := platformOpenShift
	platformOpenShift = openShift
	t.Cleanup(func() { platformOpenShift = previous })
}

func TestPlatformObservedRemovesOnlyOpenShiftAdmission(t *testing.T) {
	withPlatform(t, true)
	expected := compiledPod()
	got := platformObserved(openShiftAdmitted(expected), expected)
	got.Spec.NodeName = ""
	if !equality.Semantic.DeepEqual(got.ObjectMeta, expected.ObjectMeta) || !equality.Semantic.DeepEqual(got.Spec, expected.Spec) {
		t.Fatalf("admitted pod not normalized: %+v", got)
	}
}

func TestPlatformObservedKeepsEverythingElse(t *testing.T) {
	withPlatform(t, true)
	expected := compiledPod()
	for name, mutate := range map[string]func(*core.Pod){
		"privileged scc":       func(p *core.Pod) { p.Annotations["openshift.io/scc"] = "privileged" },
		"unknown annotation":   func(p *core.Pod) { p.Annotations["example.com/injected"] = "x" },
		"selinux type":         func(p *core.Pod) { p.Spec.SecurityContext.SELinuxOptions.Type = "spc_t" },
		"selinux level format": func(p *core.Pod) { p.Spec.SecurityContext.SELinuxOptions.Level = "s0-s0:c0.c1023" },
		"other pull secret":    func(p *core.Pod) { p.Spec.ImagePullSecrets[0].Name = "registry-admin" },
		"extra pull secret": func(p *core.Pod) {
			p.Spec.ImagePullSecrets = append(p.Spec.ImagePullSecrets, core.LocalObjectReference{Name: "default-dockercfg-abcde"})
		},
		"seccomp unconfined":        func(p *core.Pod) { p.Annotations["seccomp.security.alpha.kubernetes.io/pod"] = "unconfined" },
		"network before scheduling": func(p *core.Pod) { p.Spec.NodeName = "" },
		"oversized network status": func(p *core.Pod) {
			p.Annotations["k8s.v1.cni.cncf.io/network-status"] = "[" + strings.Repeat("x", maxOpenShiftAnnotation) + "]"
		},
		"invalid mcs level": func(p *core.Pod) { p.Spec.SecurityContext.SELinuxOptions.Level = "s0:c1" },
	} {
		t.Run(name, func(t *testing.T) {
			admitted := openShiftAdmitted(expected)
			mutate(&admitted)
			got := platformObserved(admitted, expected)
			got.Spec.NodeName = ""
			if equality.Semantic.DeepEqual(got.ObjectMeta, expected.ObjectMeta) && equality.Semantic.DeepEqual(got.Spec, expected.Spec) {
				t.Fatal("unexpected mutation accepted")
			}
		})
	}
}

func TestPlatformObservedKeepsDeclaredAnnotations(t *testing.T) {
	withPlatform(t, true)
	expected := compiledPod()
	expected.Annotations["openshift.io/scc"] = "restricted-v2"
	admitted := openShiftAdmitted(expected)
	admitted.Annotations["openshift.io/scc"] = "nonroot-v2"
	if got := platformObserved(admitted, expected); got.Annotations["openshift.io/scc"] != "nonroot-v2" {
		t.Fatal("declared annotation removed")
	}
}

func TestExactPodTemplateUnderOpenShiftProfile(t *testing.T) {
	expected := compiledPod()
	expected.APIVersion, expected.Kind = "v1", "Pod"
	expected.UID = "0f7b2f0a-1111-4222-8333-944455556666"
	admitted := openShiftAdmitted(expected)
	applyDefaults(&admitted.Spec)
	withPlatform(t, false)
	if exactPodTemplate(admitted, expected, "") {
		t.Fatal("OpenShift admission accepted without the profile")
	}
	withPlatform(t, true)
	if !exactPodTemplate(admitted, expected, "") {
		t.Fatal("admitted pod rejected under the profile")
	}
	admitted.Spec.HostNetwork = true
	if exactPodTemplate(admitted, expected, "") {
		t.Fatal("changed spec accepted under the profile")
	}
}

func TestPlatformObservedIsExactOffOpenShift(t *testing.T) {
	withPlatform(t, false)
	expected := compiledPod()
	got := platformObserved(openShiftAdmitted(expected), expected)
	if equality.Semantic.DeepEqual(got.ObjectMeta, expected.ObjectMeta) {
		t.Fatal("OpenShift admission accepted outside the OpenShift profile")
	}
}
