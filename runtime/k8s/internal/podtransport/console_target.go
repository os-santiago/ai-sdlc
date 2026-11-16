package podtransport

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasing"
	"io"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"net"
	"net/http"
	"regexp"
	"time"
)

var consoleRV = regexp.MustCompile(`^[1-9][0-9]{0,30}$`)

const consoleNS = "ai-sdlc-production"
const consoleName = "saferus-console"
const consolePath = "/apis/apps/v1/namespaces/" + consoleNS + "/deployments/" + consoleName

// ConsoleSnapshot is read-only observation, never authority to modify anything.
// Its exact identity is recorded in the deployment effect journal before PATCH.
type ConsoleSnapshot = releasing.Snapshot

// ConsoleDeployment supports only the installed one-container Saferus console.
// Callers must fence methods with their own durable current effect authority.
type ConsoleDeployment struct {
	client *Client
	target releasing.Target
}

func NewConsoleDeployment(client *Client, target releasing.Target) (*ConsoleDeployment, error) {
	if client == nil || client.http == nil || client.config == nil || target.Validate(time.Now()) != nil {
		return nil, ErrTransport
	}
	return &ConsoleDeployment{client, target}, nil
}

// ConsoleSpecDigest binds all defaulted Deployment spec fields except the sole
// container image. The installer and live observer use this same canonical form.
func ConsoleSpecDigest(spec apps.DeploymentSpec) (string, error) {
	if spec.Replicas == nil || *spec.Replicas != 1 || len(spec.Template.Spec.Containers) != 1 || spec.Template.Spec.Containers[0].Name != "console" || len(spec.Template.Spec.InitContainers) != 0 || len(spec.Template.Spec.EphemeralContainers) != 0 {
		return "", ErrTransport
	}
	p := spec.Template.Spec
	sc := p.SecurityContext
	security := p.Containers[0].SecurityContext
	if p.HostNetwork || p.HostPID || p.HostIPC || p.AutomountServiceAccountToken == nil || *p.AutomountServiceAccountToken || p.ServiceAccountName != "saferus-client" || sc == nil || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot || sc.RunAsUser == nil || *sc.RunAsUser != 1000 || sc.SeccompProfile == nil || sc.SeccompProfile.Type != core.SeccompProfileTypeRuntimeDefault || security == nil || security.AllowPrivilegeEscalation == nil || *security.AllowPrivilegeEscalation || security.ReadOnlyRootFilesystem == nil || !*security.ReadOnlyRootFilesystem || security.Capabilities == nil || len(security.Capabilities.Add) != 0 || len(security.Capabilities.Drop) != 1 || security.Capabilities.Drop[0] != "ALL" || security.Privileged != nil && *security.Privileged {
		return "", ErrTransport
	}
	spec = *spec.DeepCopy()
	spec.Template.Spec.Containers[0].Image = "<retained-console-image>"
	raw, e := json.Marshal(spec)
	if e != nil {
		return "", ErrTransport
	}
	return authority.Digest(raw), nil
}
func ConsoleServiceDigest(spec core.ServiceSpec) (string, error) {
	if spec.Type != core.ServiceTypeClusterIP || net.ParseIP(spec.ClusterIP) == nil || len(spec.ExternalIPs) != 0 || spec.ExternalName != "" || len(spec.Ports) != 1 || spec.Ports[0].Port != 8443 || spec.Ports[0].Protocol != core.ProtocolTCP || spec.Ports[0].TargetPort.String() != "https" || len(spec.Selector) != 1 || spec.Selector["app"] != consoleName {
		return "", ErrTransport
	}
	raw, e := json.Marshal(spec)
	if e != nil {
		return "", ErrTransport
	}
	return authority.Digest(raw), nil
}
func (c *ConsoleDeployment) request(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	if c == nil || c.client == nil || len(body) > 8192 {
		return nil, ErrTransport
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, e := http.NewRequestWithContext(ctx, method, c.client.base+path, bytes.NewReader(body))
	if e != nil {
		return nil, ErrTransport
	}
	req.Header.Set("Accept", "application/json")
	if method == http.MethodPatch {
		req.Header.Set("Content-Type", "application/json-patch+json")
	}
	if c.client.config.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.client.config.BearerToken)
	}
	res, e := c.client.http.Do(req)
	if e != nil {
		return nil, ErrTransport
	}
	defer res.Body.Close()
	raw, e := io.ReadAll(io.LimitReader(res.Body, 2<<20+1))
	if e != nil || len(raw) > 2<<20 || res.StatusCode != 200 || res.Header.Get("Content-Encoding") != "" || ctx.Err() != nil {
		return nil, ErrTransport
	}
	return raw, nil
}
func consoleIdentity(m meta.ObjectMeta, name, namespace, uid string) bool {
	return m.Name == name && m.Namespace == namespace && string(m.UID) == uid && resourceUID.MatchString(uid) && m.DeletionTimestamp == nil && len(m.OwnerReferences) == 0 && len(m.Finalizers) == 0
}
func (c *ConsoleDeployment) deployment(raw []byte) (apps.Deployment, error) {
	var d apps.Deployment
	if authority.DecodeStrict(raw, &d) != nil || d.APIVersion != "apps/v1" || d.Kind != "Deployment" || !consoleIdentity(d.ObjectMeta, consoleName, consoleNS, c.target.DeploymentUID) || !consoleRV.MatchString(d.ResourceVersion) || d.Generation < 1 {
		return d, ErrTransport
	}
	digest, e := ConsoleSpecDigest(d.Spec)
	if e != nil || digest != c.target.DeploymentSpecSHA256 {
		return d, ErrTransport
	}
	if len(d.Spec.Template.Spec.ImagePullSecrets) != 1 || d.Spec.Template.Spec.ImagePullSecrets[0].Name != c.target.PullSecret {
		return d, ErrTransport
	}
	return d, nil
}
func (c *ConsoleDeployment) readTarget(ctx context.Context) (apps.Deployment, core.Service, error) {
	var d apps.Deployment
	var service core.Service
	if c == nil || c.target.Validate(time.Now()) != nil {
		return d, service, ErrTransport
	}
	for _, ns := range []struct{ name, uid string }{{"kube-system", c.target.ClusterUID}, {consoleNS, c.target.NamespaceUID}} {
		raw, e := c.request(ctx, http.MethodGet, "/api/v1/namespaces/"+ns.name, nil)
		var obj core.Namespace
		if e != nil || authority.DecodeStrict(raw, &obj) != nil || obj.Kind != "Namespace" || obj.APIVersion != "v1" || !consoleIdentity(obj.ObjectMeta, ns.name, "", ns.uid) {
			return d, service, ErrTransport
		}
	}
	for _, secret := range []struct{ name, uid string }{{c.target.PullSecret, c.target.PullSecretUID}, {"saferus-console-tls", c.target.TLSSecretUID}} {
		raw, e := c.request(ctx, http.MethodGet, "/api/v1/namespaces/"+consoleNS+"/secrets/"+secret.name, nil)
		var obj core.Secret
		if e != nil || authority.DecodeStrict(raw, &obj) != nil || obj.Kind != "Secret" || obj.APIVersion != "v1" || !consoleIdentity(obj.ObjectMeta, secret.name, consoleNS, secret.uid) {
			return d, service, ErrTransport
		}
		if secret.name == c.target.PullSecret && (obj.Immutable == nil || !*obj.Immutable || obj.Type != core.SecretTypeDockerConfigJson) {
			return d, service, ErrTransport
		}
		if secret.name == "saferus-console-tls" && obj.Type != core.SecretTypeTLS {
			return d, service, ErrTransport
		}
	}
	raw, e := c.request(ctx, http.MethodGet, "/api/v1/namespaces/"+consoleNS+"/services/"+consoleName, nil)
	if e != nil || authority.DecodeStrict(raw, &service) != nil || service.Kind != "Service" || service.APIVersion != "v1" || !consoleIdentity(service.ObjectMeta, consoleName, consoleNS, c.target.ServiceUID) {
		return d, service, ErrTransport
	}
	digest, e := ConsoleServiceDigest(service.Spec)
	if e != nil || digest != c.target.ServiceSpecSHA256 {
		return d, service, ErrTransport
	}
	raw, e = c.request(ctx, http.MethodGet, consolePath, nil)
	if e != nil {
		return d, service, ErrTransport
	}
	d, e = c.deployment(raw)
	return d, service, e
}
func (c *ConsoleDeployment) Observe(ctx context.Context) (ConsoleSnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	d, _, e := c.readTarget(ctx)
	if e != nil {
		return ConsoleSnapshot{}, e
	}
	return ConsoleSnapshot{DeploymentUID: string(d.UID), ResourceVersion: d.ResourceVersion, Generation: d.Generation, Image: d.Spec.Template.Spec.Containers[0].Image, TargetSHA256: c.target.Digest(), ObservedAt: time.Now().UnixMilli()}, nil
}
