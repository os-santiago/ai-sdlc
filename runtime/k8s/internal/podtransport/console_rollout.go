package podtransport

import (
	"context"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasing"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"net"
	"net/http"
	"reflect"
	"strings"
	"time"
)

// ConsoleRollout is an instantaneous exact-owner observation, not health proof.
type ConsoleRollout = releasing.Rollout

func soleOwner(meta meta.ObjectMeta, kind, name, uid string) bool {
	if len(meta.OwnerReferences) != 1 {
		return false
	}
	o := meta.OwnerReferences[0]
	return o.Controller != nil && *o.Controller && o.Kind == kind && o.Name == name && string(o.UID) == uid && o.APIVersion == "apps/v1"
}

// The expected template is already API-defaulted in its Deployment. Normalize
// only admission's standard Pod fields; unexpected mounts/sidecars/privileges,
// service-account changes and selectors remain observable and denied.
func consolePodSpec(actual core.PodSpec, expected core.PodSpec) bool {
	a := *actual.DeepCopy()
	e := *expected.DeepCopy()
	a.NodeName = ""
	if e.EnableServiceLinks == nil {
		v := true
		e.EnableServiceLinks = &v
	}
	if e.Priority == nil {
		v := int32(0)
		e.Priority = &v
	}
	if e.PreemptionPolicy == nil {
		v := core.PreemptLowerPriority
		e.PreemptionPolicy = &v
	}
	if e.DeprecatedServiceAccount == "" {
		e.DeprecatedServiceAccount = e.ServiceAccountName
	}
	// Match the pinned DefaultTolerationSeconds admission plugin: append each
	// missing default, preserving explicit and wildcard approved tolerations.
	for _, key := range []string{core.TaintNodeNotReady, core.TaintNodeUnreachable} {
		present := false
		for _, tolerance := range e.Tolerations {
			if (tolerance.Key == key || tolerance.Key == "") && (tolerance.Effect == core.TaintEffectNoExecute || tolerance.Effect == "") {
				present = true
			}
		}
		if !present {
			v := int64(300)
			e.Tolerations = append(e.Tolerations, core.Toleration{Key: key, Operator: core.TolerationOpExists, Effect: core.TaintEffectNoExecute, TolerationSeconds: &v})
		}
	}
	return reflect.DeepEqual(a, e)
}
func (c *ConsoleDeployment) ObserveRollout(ctx context.Context, expected ConsoleSnapshot) (ConsoleRollout, bool, error) {
	var empty ConsoleRollout
	ctx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	d, s, e := c.readTarget(ctx)
	if e != nil {
		return empty, false, e
	}
	if expected.DeploymentUID != string(d.UID) || expected.TargetSHA256 != c.target.Digest() || d.Generation != expected.Generation || d.Spec.Template.Spec.Containers[0].Image != expected.Image {
		return empty, false, ErrTransport
	}
	if d.Status.ObservedGeneration != d.Generation || d.Status.Replicas != 1 || d.Status.UpdatedReplicas != 1 || d.Status.ReadyReplicas != 1 || d.Status.AvailableReplicas != 1 || d.Status.UnavailableReplicas != 0 {
		return empty, false, nil
	}
	raw, e := c.request(ctx, http.MethodGet, "/apis/apps/v1/namespaces/"+consoleNS+"/replicasets?labelSelector=app%3Dsaferus-console&limit=32", nil)
	var sets apps.ReplicaSetList
	if e != nil || authority.DecodeStrict(raw, &sets) != nil || sets.Continue != "" || len(sets.Items) > 32 {
		return empty, false, ErrTransport
	}
	var selected *apps.ReplicaSet
	for i := range sets.Items {
		rs := &sets.Items[i]
		if rs.Namespace != consoleNS || !resourceUID.MatchString(string(rs.UID)) || rs.DeletionTimestamp != nil || !soleOwner(rs.ObjectMeta, "Deployment", consoleName, string(d.UID)) {
			return empty, false, ErrTransport
		}
		if rs.Spec.Replicas == nil {
			return empty, false, ErrTransport
		}
		if *rs.Spec.Replicas == 0 {
			if rs.Status.Replicas != 0 {
				return empty, false, nil
			}
			continue
		}
		if *rs.Spec.Replicas != 1 || selected != nil {
			return empty, false, nil
		}
		template := *rs.Spec.Template.DeepCopy()
		delete(template.Labels, "pod-template-hash")
		if !reflect.DeepEqual(template, d.Spec.Template) {
			return empty, false, ErrTransport
		}
		if rs.Status.ObservedGeneration != rs.Generation || rs.Status.Replicas != 1 || rs.Status.ReadyReplicas != 1 || rs.Status.AvailableReplicas != 1 {
			return empty, false, nil
		}
		selected = rs
	}
	if selected == nil {
		return empty, false, nil
	}
	raw, e = c.request(ctx, http.MethodGet, "/api/v1/namespaces/"+consoleNS+"/pods?labelSelector=app%3Dsaferus-console&limit=32", nil)
	var pods core.PodList
	if e != nil || authority.DecodeStrict(raw, &pods) != nil || pods.Continue != "" || len(pods.Items) > 32 {
		return empty, false, ErrTransport
	}
	if len(pods.Items) != 1 {
		return empty, false, nil
	}
	p := pods.Items[0]
	if p.Namespace != consoleNS || !resourceUID.MatchString(string(p.UID)) || p.DeletionTimestamp != nil || !soleOwner(p.ObjectMeta, "ReplicaSet", selected.Name, string(selected.UID)) || p.Spec.NodeName == "" || !consolePodSpec(p.Spec, d.Spec.Template.Spec) {
		return empty, false, ErrTransport
	}
	labels := map[string]string{}
	for k, v := range p.Labels {
		if k != "pod-template-hash" {
			labels[k] = v
		}
	}
	if !reflect.DeepEqual(labels, d.Spec.Template.Labels) || !reflect.DeepEqual(p.Annotations, d.Spec.Template.Annotations) {
		return empty, false, ErrTransport
	}
	if p.Status.Phase != core.PodRunning || len(p.Status.ContainerStatuses) != 1 || len(p.Status.InitContainerStatuses) != 0 || len(p.Status.EphemeralContainerStatuses) != 0 {
		return empty, false, nil
	}
	ready := false
	for _, condition := range p.Status.Conditions {
		if condition.Type == core.PodReady && condition.Status == core.ConditionTrue {
			ready = true
		}
	}
	cs := p.Status.ContainerStatuses[0]
	digest := strings.Split(expected.Image, "@")
	if len(digest) != 2 {
		return empty, false, ErrTransport
	}
	runtimeDigest := digest[1]
	if expected.Image == c.target.BaselineImage {
		runtimeDigest = c.target.BaselineRuntimeDigest
	}
	imageID := cs.ImageID
	separator := strings.LastIndex(imageID, "@")
	if separator < 0 || imageID[separator+1:] != runtimeDigest {
		return empty, false, ErrTransport
	}
	if !ready || cs.Name != "console" || cs.State.Running == nil || !cs.Ready || cs.ContainerID == "" || cs.RestartCount != 0 {
		return empty, false, nil
	}
	if net.ParseIP(p.Status.PodIP) == nil {
		return empty, false, ErrTransport
	}
	raw, e = c.request(ctx, http.MethodGet, "/apis/discovery.k8s.io/v1/namespaces/"+consoleNS+"/endpointslices?labelSelector=kubernetes.io%2Fservice-name%3Dsaferus-console&limit=8", nil)
	var slices discovery.EndpointSliceList
	if e != nil || authority.DecodeStrict(raw, &slices) != nil || slices.Continue != "" || len(slices.Items) != 1 {
		return empty, false, ErrTransport
	}
	ep := slices.Items[0]
	if ep.Namespace != consoleNS || !resourceUID.MatchString(string(ep.UID)) || ep.DeletionTimestamp != nil || len(ep.OwnerReferences) != 1 || string(ep.OwnerReferences[0].UID) != string(s.UID) || ep.OwnerReferences[0].Kind != "Service" || ep.OwnerReferences[0].Name != consoleName || ep.OwnerReferences[0].APIVersion != "v1" || ep.OwnerReferences[0].Controller == nil || !*ep.OwnerReferences[0].Controller || len(ep.Ports) != 1 {
		return empty, false, ErrTransport
	}
	port := ep.Ports[0]
	if port.Name == nil || *port.Name != "https" || port.Port == nil || *port.Port != 8443 || port.Protocol == nil || *port.Protocol != core.ProtocolTCP {
		return empty, false, ErrTransport
	}
	if len(ep.Endpoints) != 1 {
		return empty, false, nil
	}
	endpoint := ep.Endpoints[0]
	if endpoint.TargetRef == nil || endpoint.TargetRef.Kind != "Pod" || endpoint.TargetRef.Namespace != consoleNS || endpoint.TargetRef.Name == "" || !resourceUID.MatchString(string(endpoint.TargetRef.UID)) {
		return empty, false, ErrTransport
	}
	// EndpointSlice reconciliation is asynchronous. A previous Pod reference
	// never becomes health authority; allow bounded convergence before smoke.
	if endpoint.TargetRef.Name != p.Name || endpoint.TargetRef.UID != p.UID {
		return empty, false, nil
	}
	if len(endpoint.Addresses) != 1 || endpoint.Addresses[0] != p.Status.PodIP || endpoint.NodeName == nil || *endpoint.NodeName != p.Spec.NodeName {
		return empty, false, ErrTransport
	}
	if endpoint.Conditions.Ready == nil || !*endpoint.Conditions.Ready || endpoint.Conditions.Terminating != nil && *endpoint.Conditions.Terminating || endpoint.Conditions.Serving != nil && !*endpoint.Conditions.Serving {
		return empty, false, nil
	}
	return ConsoleRollout{DeploymentUID: string(d.UID), Generation: d.Generation, ReplicaSetUID: string(selected.UID), PodUID: string(p.UID), PodIP: p.Status.PodIP, ImageID: imageID, Node: p.Spec.NodeName, ServiceUID: string(s.UID), ServiceIP: s.Spec.ClusterIP, EndpointSliceUID: string(ep.UID), TargetSHA256: c.target.Digest(), Image: expected.Image, ObservedAt: time.Now().UnixMilli()}, true, nil
}
