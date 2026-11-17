package podtransport

import (
	"context"
	"net/http"
	"time"

	"encoding/json"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/browsercheck"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasing"
	core "k8s.io/api/core/v1"
	network "k8s.io/api/networking/v1"
	node "k8s.io/api/node/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

type BrowserResources struct {
	console   *ConsoleDeployment
	profile   browsercheck.Profile
	request   browsercheck.Request
	pod       core.Pod
	resources *Resources
}

func NewBrowserResources(console *ConsoleDeployment, profile browsercheck.Profile, request browsercheck.Request, route ConsoleRollout) (*BrowserResources, error) {
	if console == nil || profile.ClusterUID != console.target.ClusterUID || profile.CASHA256 != authority.Digest([]byte(console.target.TLSCA)) {
		return nil, ErrTransport
	}
	pod, e := CompileBrowserPod(profile, request, route, time.Now())
	if e != nil {
		return nil, e
	}
	return &BrowserResources{console: console, profile: profile, request: request, pod: pod, resources: &Resources{client: console.client, binding: Binding{Namespace: browsercheck.Namespace}, pod: pod, expires: time.UnixMilli(request.ExpiresAt)}}, nil
}

func browserPolicies() (network.NetworkPolicySpec, network.NetworkPolicySpec, network.NetworkPolicySpec) {
	selector := meta.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "ai-sdlc-browser-verifier"}}
	console := meta.LabelSelector{MatchLabels: map[string]string{"app": "saferus-console"}}
	browserNS := meta.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": browsercheck.Namespace}}
	consoleNS := meta.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "ai-sdlc-production"}}
	protocol := core.ProtocolTCP
	port := intstr.FromInt32(8443)
	ports := []network.NetworkPolicyPort{{Protocol: &protocol, Port: &port}}
	deny := network.NetworkPolicySpec{PodSelector: meta.LabelSelector{}, PolicyTypes: []network.PolicyType{network.PolicyTypeIngress, network.PolicyTypeEgress}}
	egress := network.NetworkPolicySpec{PodSelector: selector, PolicyTypes: []network.PolicyType{network.PolicyTypeEgress}, Egress: []network.NetworkPolicyEgressRule{{To: []network.NetworkPolicyPeer{{NamespaceSelector: &consoleNS, PodSelector: &console}}, Ports: ports}}}
	ingress := network.NetworkPolicySpec{PodSelector: console, PolicyTypes: []network.PolicyType{network.PolicyTypeIngress}, Ingress: []network.NetworkPolicyIngressRule{{From: []network.NetworkPolicyPeer{{NamespaceSelector: &browserNS, PodSelector: &selector}}, Ports: ports}}}
	return deny, egress, ingress
}
func (b *BrowserResources) read(ctx context.Context, path string, value any) error {
	raw, e := b.console.request(ctx, http.MethodGet, path, nil)
	if e != nil || authority.DecodeStrict(raw, value) != nil {
		return ErrTransport
	}
	return nil
}

// Every effect/result re-observes the installed namespace, CA, runtime class
// and complete network-policy set. Additional browser egress is rejected.
// Node installation/binary custody belongs to the administrative installer;
// this workload never receives node, namespace or policy mutation permission.
func (b *BrowserResources) scope(ctx context.Context) error {
	if b == nil || b.console == nil {
		return ErrTransport
	}
	return validateBrowserScope(b.read, ctx, b.profile, b.console.target)
}

func validateBrowserScope(read func(context.Context, string, any) error, ctx context.Context, profile browsercheck.Profile, target releasing.Target) error {
	if profile.Validate() != nil || profile.ClusterUID != target.ClusterUID || profile.CASHA256 != authority.Digest([]byte(target.TLSCA)) {
		return ErrTransport
	}
	for _, scope := range []struct{ name, uid string }{{"kube-system", profile.ClusterUID}, {browsercheck.Namespace, profile.NamespaceUID}} {
		var actual core.Namespace
		if read(ctx, "/api/v1/namespaces/"+scope.name, &actual) != nil || string(actual.UID) != scope.uid || actual.Name != scope.name || actual.DeletionTimestamp != nil {
			return ErrTransport
		}
		if scope.name == browsercheck.Namespace && (actual.Labels["pod-security.kubernetes.io/enforce"] != "restricted" || actual.Labels["kubernetes.io/metadata.name"] != browsercheck.Namespace) {
			return ErrTransport
		}
	}
	var originalNode core.Node
	if read(ctx, "/api/v1/nodes/"+profile.NodeName, &originalNode) != nil || string(originalNode.UID) != profile.NodeUID || originalNode.Name != profile.NodeName || originalNode.DeletionTimestamp != nil || originalNode.Spec.Unschedulable {
		return ErrTransport
	}
	ready := false
	for _, condition := range originalNode.Status.Conditions {
		if condition.Type == core.NodeReady && condition.Status == core.ConditionTrue {
			ready = true
		}
	}
	if !ready {
		return ErrTransport
	}
	var class node.RuntimeClass
	if read(ctx, "/apis/node.k8s.io/v1/runtimeclasses/"+browsercheck.RuntimeClass, &class) != nil || string(class.UID) != profile.RuntimeClassUID || class.Name != browsercheck.RuntimeClass || class.Handler != browsercheck.RuntimeClass || class.DeletionTimestamp != nil || class.Overhead != nil || class.Scheduling != nil || class.Annotations["ai-sdlc.io/cluster-uid"] != profile.ClusterUID || class.Annotations["ai-sdlc.io/configuration-sha256"] != profile.RuntimeConfigurationSHA256 {
		return ErrTransport
	}
	var ca core.ConfigMap
	if read(ctx, "/api/v1/namespaces/"+browsercheck.Namespace+"/configmaps/"+browserCA, &ca) != nil || string(ca.UID) != profile.CAConfigMapUID || ca.Name != browserCA || ca.Namespace != browsercheck.Namespace || ca.DeletionTimestamp != nil || ca.Immutable == nil || !*ca.Immutable || len(ca.Data) != 1 || len(ca.BinaryData) != 0 || ca.Data["ca.crt"] != target.TLSCA || authority.Digest([]byte(ca.Data["ca.crt"])) != profile.CASHA256 {
		return ErrTransport
	}
	var policies network.NetworkPolicyList
	if read(ctx, "/apis/networking.k8s.io/v1/namespaces/"+browsercheck.Namespace+"/networkpolicies", &policies) != nil || len(policies.Items) != 2 {
		return ErrTransport
	}
	deny, egress, ingress := browserPolicies()
	seen := map[string]bool{}
	for _, policy := range policies.Items {
		if policy.Namespace != browsercheck.Namespace || policy.DeletionTimestamp != nil || seen[policy.Name] {
			return ErrTransport
		}
		seen[policy.Name] = true
		switch policy.Name {
		case "deny-all":
			if string(policy.UID) != profile.DenyPolicyUID || !equality.Semantic.DeepEqual(policy.Spec, deny) {
				return ErrTransport
			}
		case "console-egress":
			if string(policy.UID) != profile.EgressPolicyUID || !equality.Semantic.DeepEqual(policy.Spec, egress) {
				return ErrTransport
			}
		default:
			return ErrTransport
		}
	}
	var policy network.NetworkPolicy
	if read(ctx, "/apis/networking.k8s.io/v1/namespaces/ai-sdlc-production/networkpolicies/ai-sdlc-browser-console", &policy) != nil || string(policy.UID) != profile.ConsoleIngressPolicyUID || policy.Name != "ai-sdlc-browser-console" || policy.Namespace != "ai-sdlc-production" || policy.DeletionTimestamp != nil || !equality.Semantic.DeepEqual(policy.Spec, ingress) {
		return ErrTransport
	}
	return nil
}

// BrowserInstallation contains only public observations. Its compiler has no
// network or write authority and shares the running adapter's scope validator.
type BrowserInstallation struct {
	Target         releasing.Target          `json:"target"`
	Profile        browsercheck.Profile      `json:"profile"`
	Cluster        core.Namespace            `json:"cluster"`
	Namespace      core.Namespace            `json:"namespace"`
	Node           core.Node                 `json:"node"`
	RuntimeClass   node.RuntimeClass         `json:"runtimeClass"`
	CA             core.ConfigMap            `json:"ca"`
	Policies       network.NetworkPolicyList `json:"policies"`
	ConsoleIngress network.NetworkPolicy     `json:"consoleIngress"`
}

func (i BrowserInstallation) Validate(now time.Time) error {
	if i.Target.Validate(now) != nil {
		return ErrTransport
	}
	objects := map[string]any{
		"/api/v1/namespaces/kube-system":                                                                   i.Cluster,
		"/api/v1/namespaces/" + browsercheck.Namespace:                                                     i.Namespace,
		"/api/v1/nodes/" + i.Profile.NodeName:                                                              i.Node,
		"/apis/node.k8s.io/v1/runtimeclasses/" + browsercheck.RuntimeClass:                                 i.RuntimeClass,
		"/api/v1/namespaces/" + browsercheck.Namespace + "/configmaps/" + browserCA:                        i.CA,
		"/apis/networking.k8s.io/v1/namespaces/" + browsercheck.Namespace + "/networkpolicies":             i.Policies,
		"/apis/networking.k8s.io/v1/namespaces/ai-sdlc-production/networkpolicies/ai-sdlc-browser-console": i.ConsoleIngress,
	}
	return validateBrowserScope(func(_ context.Context, path string, value any) error {
		observed, ok := objects[path]
		if !ok {
			return ErrTransport
		}
		raw, e := json.Marshal(observed)
		if e != nil {
			return ErrTransport
		}
		return authority.DecodeStrict(raw, value)
	}, context.Background(), i.Profile, i.Target)
}
