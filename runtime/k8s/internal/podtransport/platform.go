package podtransport

import (
	"encoding/json"
	"os"
	"regexp"

	core "k8s.io/api/core/v1"
)

// PlatformOpenShift selects the OpenShift admission profile. The runtime sets
// it with AI_SDLC_PLATFORM=openshift. On any other value pods must match their
// compiled templates exactly, as before.
var platformOpenShift = os.Getenv("AI_SDLC_PLATFORM") == "openshift"

// OpenShift admission and networking add these annotations to every pod. They
// are informational: none of them changes what the pod runs or may access. A
// pod admitted under any SCC other than the standard non-privileged ones keeps
// its annotation and fails the comparison.
var openShiftAnnotations = map[string]*regexp.Regexp{
	"openshift.io/scc":                                 regexp.MustCompile(`^(nonroot-v2|restricted-v2)$`),
	"seccomp.security.alpha.kubernetes.io/pod":         regexp.MustCompile(`^runtime/default$`),
	"security.openshift.io/validated-scc-subject-type": regexp.MustCompile(`^(user|serviceaccount)$`),
	"k8s.ovn.org/pod-networks":                         regexp.MustCompile(`(?s)^\{.*\}$`),
	"k8s.v1.cni.cncf.io/network-status":                regexp.MustCompile(`(?s)^\[.*\]$`),
}

var openShiftNetworkAnnotations = map[string]bool{"k8s.ovn.org/pod-networks": true, "k8s.v1.cni.cncf.io/network-status": true}

// The SCC assigns the namespace's MCS level, and the ServiceAccount admission
// adds the default ServiceAccount's registry pull secret.
var (
	maxOpenShiftAnnotation = 8192
	openShiftMCSLevel      = regexp.MustCompile(`^s0:c[0-9]{1,4},c[0-9]{1,4}$`)
	openShiftPullSecret    = regexp.MustCompile(`^default-dockercfg-[a-z0-9]{5}$`)
)

// platformObserved returns the pod as the runtime compiled it: on OpenShift it
// removes only the exact, known admission additions that the template does not
// declare. Anything else that differs still fails the exact comparison.
func platformObserved(actual, expected core.Pod) core.Pod {
	if !platformOpenShift {
		return actual
	}
	actual = *actual.DeepCopy()
	for key, format := range openShiftAnnotations {
		value, added := actual.Annotations[key]
		// The network plugins annotate a pod only once it is scheduled; one
		// already present at creation would come from someone else.
		if openShiftNetworkAnnotations[key] && actual.Spec.NodeName == "" {
			continue
		}
		if _, declared := expected.Annotations[key]; added && !declared && len(value) <= maxOpenShiftAnnotation && format.MatchString(value) && wellFormedNetworkAnnotation(key, value) {
			delete(actual.Annotations, key)
		}
	}
	if len(actual.Annotations) == 0 && expected.Annotations == nil {
		actual.Annotations = nil
	}
	if s, e := actual.Spec.SecurityContext, expected.Spec.SecurityContext; s != nil && s.SELinuxOptions != nil && (e == nil || e.SELinuxOptions == nil) {
		o := s.SELinuxOptions
		if o.User == "" && o.Role == "" && o.Type == "" && openShiftMCSLevel.MatchString(o.Level) {
			s.SELinuxOptions = nil
		}
	}
	if len(expected.Spec.ImagePullSecrets) == 0 && len(actual.Spec.ImagePullSecrets) == 1 && openShiftPullSecret.MatchString(actual.Spec.ImagePullSecrets[0].Name) {
		actual.Spec.ImagePullSecrets = nil
	}
	return actual
}

// wellFormedNetworkAnnotation requires the plugins' JSON shapes: a JSON object
// of networks for OVN-Kubernetes and a JSON array of network objects for
// Multus. Other annotations are fully constrained by their pattern.
func wellFormedNetworkAnnotation(key, value string) bool {
	switch key {
	case "k8s.ovn.org/pod-networks":
		var networks map[string]map[string]any
		return json.Unmarshal([]byte(value), &networks) == nil && len(networks) > 0
	case "k8s.v1.cni.cncf.io/network-status":
		var statuses []map[string]any
		return json.Unmarshal([]byte(value), &statuses) == nil && len(statuses) > 0
	}
	return true
}
