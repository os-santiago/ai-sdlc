package podtransport

import (
	"encoding/json"
	"net"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/browsercheck"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasing"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const browserCA = "approved-console-ca"
const browserRequestAnnotation = "ai-sdlc.io/browser-request"

// CompileBrowserPod accepts only the installed image/profile and the journal's
// bounded request. It has no command, URL, script, environment or secret input.
func CompileBrowserPod(profile browsercheck.Profile, request browsercheck.Request, route releasing.Rollout, now time.Time) (core.Pod, error) {
	if profile.Validate() != nil || request.Validate(now) != nil || request.TargetSHA256 != route.TargetSHA256 || request.ImageDigest != imageSuffix(route.ImageID) {
		return core.Pod{}, ErrTransport
	}
	ip := net.ParseIP(route.ServiceIP)
	if ip == nil || ip.IsUnspecified() || ip.IsLoopback() || ip.IsMulticast() || ip.IsLinkLocalUnicast() {
		return core.Pod{}, ErrTransport
	}
	raw, _ := json.Marshal(request)
	seconds := int64((time.UnixMilli(request.ExpiresAt).Sub(now) + time.Second - 1) / time.Second)
	if seconds < 1 || seconds > 30 {
		return core.Pod{}, ErrTransport
	}
	name := "browser-" + request.RequestID[len("browser-"):len("browser-")+48]
	falseValue, trueValue := false, true
	user, grace, mode := int64(1001), int64(1), int32(0444)
	class := browsercheck.RuntimeClass
	p := core.Pod{TypeMeta: meta.TypeMeta{APIVersion: "v1", Kind: "Pod"}, ObjectMeta: meta.ObjectMeta{Name: name, Namespace: browsercheck.Namespace, Labels: map[string]string{"app.kubernetes.io/name": "ai-sdlc-browser-verifier"}, Annotations: map[string]string{browserRequestAnnotation: string(raw), "ai-sdlc.io/profile-sha256": profile.Digest()}}, Spec: core.PodSpec{NodeName: profile.NodeName,
		RuntimeClassName: &class, AutomountServiceAccountToken: &falseValue, EnableServiceLinks: &falseValue, RestartPolicy: core.RestartPolicyNever, ActiveDeadlineSeconds: &seconds, TerminationGracePeriodSeconds: &grace,
		DNSPolicy: core.DNSNone, DNSConfig: &core.PodDNSConfig{Nameservers: []string{"127.0.0.1"}}, HostAliases: []core.HostAlias{{IP: route.ServiceIP, Hostnames: []string{"saferus-console"}}},
		SecurityContext: &core.PodSecurityContext{RunAsNonRoot: &trueValue, RunAsUser: &user, RunAsGroup: &user, FSGroup: &user, SeccompProfile: &core.SeccompProfile{Type: core.SeccompProfileTypeRuntimeDefault}},
		Volumes: []core.Volume{{Name: "work", VolumeSource: core.VolumeSource{EmptyDir: &core.EmptyDirVolumeSource{Medium: core.StorageMediumMemory, SizeLimit: browserQuantity("512Mi")}}}, {Name: "input", VolumeSource: core.VolumeSource{Projected: &core.ProjectedVolumeSource{DefaultMode: &mode, Sources: []core.VolumeProjection{
			{ConfigMap: &core.ConfigMapProjection{LocalObjectReference: core.LocalObjectReference{Name: browserCA}, Items: []core.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}}}},
			{DownwardAPI: &core.DownwardAPIProjection{Items: []core.DownwardAPIVolumeFile{{Path: "request.json", FieldRef: &core.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.annotations['" + browserRequestAnnotation + "']"}}}}},
		}}}}},
		Containers: []core.Container{{Name: "browser", Image: profile.Image, ImagePullPolicy: core.PullIfNotPresent,
			SecurityContext: &core.SecurityContext{RunAsNonRoot: &trueValue, ReadOnlyRootFilesystem: &trueValue, AllowPrivilegeEscalation: &falseValue, Capabilities: &core.Capabilities{Drop: []core.Capability{"ALL"}}},
			Resources:       core.ResourceRequirements{Requests: core.ResourceList{core.ResourceCPU: resource.MustParse("100m"), core.ResourceMemory: resource.MustParse("256Mi"), core.ResourceEphemeralStorage: resource.MustParse("16Mi")}, Limits: core.ResourceList{core.ResourceCPU: resource.MustParse("2"), core.ResourceMemory: resource.MustParse("1Gi"), core.ResourceEphemeralStorage: resource.MustParse("16Mi")}},
			VolumeMounts:    []core.VolumeMount{{Name: "work", MountPath: "/work"}, {Name: "input", MountPath: "/input", ReadOnly: true}},
		}},
	}}
	return p, nil
}
func browserQuantity(s string) *resource.Quantity { v := resource.MustParse(s); return &v }
func imageSuffix(value string) string {
	for i := len(value) - 1; i >= 0; i-- {
		if value[i] == '@' {
			return value[i+1:]
		}
	}
	return ""
}
func BrowserPodDigest(p core.Pod) string { raw, _ := json.Marshal(p); return authority.Digest(raw) }

func exactBrowserPod(actual, expected core.Pod, uid string) bool {
	actual = platformObserved(actual, expected)
	if actual.APIVersion != "v1" || actual.Kind != "Pod" || !exactMetadata(actual.ObjectMeta, expected.ObjectMeta, uid) {
		return false
	}
	expected = *expected.DeepCopy()
	applyDefaults(&expected.Spec)
	// Generic executor defaults use ClusterFirst. This recipe deliberately has
	// no DNS egress and supplies the original Service IP through hostAliases.
	expected.Spec.DNSPolicy = core.DNSNone
	actual = *actual.DeepCopy()
	return equality.Semantic.DeepEqual(actual.Spec, expected.Spec)
}
