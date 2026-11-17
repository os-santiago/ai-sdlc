package podtransport

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/browsercheck"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasing"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

func browserFixture(t *testing.T) (browsercheck.Profile, browsercheck.Request, releasing.Rollout, core.Pod, time.Time) {
	t.Helper()
	now := time.Now().Truncate(time.Millisecond)
	id := func(n int) string { return fmt.Sprintf("%08d-1111-1111-1111-111111111111", n) }
	profile := browsercheck.Profile{NodeName: "fixture-node", NodeUID: id(8), Version: browsercheck.ProfileVersion, ClusterUID: id(1), NamespaceUID: id(2), RuntimeClassUID: id(3), CAConfigMapUID: id(4), DenyPolicyUID: id(5), EgressPolicyUID: id(6), ConsoleIngressPolicyUID: id(7), Image: "quay.io/os-santiago/ai-sdlc@sha256:" + strings.Repeat("a", 64), RuntimeDigest: "sha256:" + strings.Repeat("b", 64), CASHA256: strings.Repeat("c", 64), RuntimeConfigurationSHA256: strings.Repeat("d", 64), BaselineExpectation: "baseline-seconds", CandidateExpectation: "readable-duration"}
	request := browsercheck.Request{Version: browsercheck.RequestVersion, RequestID: "browser-" + strings.Repeat("a", 64), TargetSHA256: strings.Repeat("b", 64), SnapshotSHA256: strings.Repeat("c", 64), ImageDigest: "sha256:" + strings.Repeat("d", 64), IssuedAt: now.UnixMilli(), ExpiresAt: now.Add(30 * time.Second).UnixMilli(), Expectation: "readable-duration"}
	route := releasing.Rollout{ServiceIP: "10.96.0.24", TargetSHA256: request.TargetSHA256, ImageID: "registry-service.kind-registry/ai-sdlc-fixtures/saferus@" + request.ImageDigest}
	pod, e := CompileBrowserPod(profile, request, route, now)
	if e != nil {
		t.Fatal(e)
	}
	return profile, request, route, pod, now
}
func TestBrowserPodCannotAcquireAlternateExecutionOrNetworkAuthority(t *testing.T) {
	_, _, _, pod, _ := browserFixture(t)
	actual := *pod.DeepCopy()
	actual.UID = types.UID("99999999-1111-1111-1111-111111111111")
	applyDefaults(&actual.Spec)
	actual.Spec.DNSPolicy = core.DNSNone
	actual.Spec.NodeName = "fixture-node"
	if !exactBrowserPod(actual, pod, string(actual.UID)) {
		t.Fatal("expected API defaults rejected")
	}
	for name, change := range map[string]func(*core.Pod){
		"alternate node":    func(p *core.Pod) { p.Spec.NodeName = "unprepared-node" },
		"default runtime":   func(p *core.Pod) { p.Spec.RuntimeClassName = nil },
		"host networking":   func(p *core.Pod) { p.Spec.HostNetwork = true },
		"API credential":    func(p *core.Pod) { v := true; p.Spec.AutomountServiceAccountToken = &v },
		"arbitrary command": func(p *core.Pod) { p.Spec.Containers[0].Command = []string{"sh"} },
		"extra environment": func(p *core.Pod) {
			p.Spec.Containers[0].Env = []core.EnvVar{{Name: "SSLKEYLOGFILE", Value: "/work/keys"}}
		},
		"DNS egress":        func(p *core.Pod) { p.Spec.DNSPolicy = core.DNSClusterFirst },
		"foreign route":     func(p *core.Pod) { p.Spec.HostAliases[0].IP = "10.96.0.1" },
		"restarts":          func(p *core.Pod) { p.Spec.RestartPolicy = core.RestartPolicyAlways },
		"extended deadline": func(p *core.Pod) { v := int64(60); p.Spec.ActiveDeadlineSeconds = &v },
		"changed request":   func(p *core.Pod) { p.Annotations[browserRequestAnnotation] = "{}" },
		"writable image":    func(p *core.Pod) { v := false; p.Spec.Containers[0].SecurityContext.ReadOnlyRootFilesystem = &v },
		"secret mount": func(p *core.Pod) {
			p.Spec.Volumes = append(p.Spec.Volumes, core.Volume{Name: "credential", VolumeSource: core.VolumeSource{Secret: &core.SecretVolumeSource{SecretName: "credential"}}})
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := *actual.DeepCopy()
			change(&p)
			if exactBrowserPod(p, pod, string(actual.UID)) {
				t.Fatal("accepted expanded browser Pod")
			}
		})
	}
	if exactBrowserPod(actual, pod, "88888888-1111-1111-1111-111111111111") {
		t.Fatal("adopted replacement Pod")
	}
}
func TestBrowserPodRejectsForeignRequestAndNonServiceRoute(t *testing.T) {
	profile, request, route, _, now := browserFixture(t)
	for _, ip := range []string{"127.0.0.1", "0.0.0.0", "169.254.169.254", "224.0.0.1", "invalid"} {
		r := route
		r.ServiceIP = ip
		if _, e := CompileBrowserPod(profile, request, r, now); e == nil {
			t.Fatal("accepted non-service destination", ip)
		}
	}
	request.TargetSHA256 = strings.Repeat("e", 64)
	if _, e := CompileBrowserPod(profile, request, route, now); e == nil {
		t.Fatal("accepted foreign request")
	}
}
