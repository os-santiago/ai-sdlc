package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/podtransport"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasing"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"strings"
	"testing"
	"time"
)

func releasePtr[T any](v T) *T { return &v }
func TestReleaseInstallationCompilerRejectsChangedApprovedTarget(t *testing.T) {
	trust, _ := provenanceInstallationFixture(t)
	uid := func(i int) string { return fmt.Sprintf("00000000-0000-0000-0000-%012d", i) }
	target := releasing.Target{Version: releasing.TargetVersion, Scope: "kind-test-production", Repository: trust.Repository, Environment: trust.Environment, ClusterUID: trust.ClusterUID, NamespaceUID: uid(2), Namespace: "ai-sdlc-production", Deployment: "saferus-console", DeploymentUID: uid(3), Container: "console", Service: "saferus-console", ServiceUID: uid(4), PullSecret: "ai-sdlc-release-reader", PullSecretUID: uid(5), TLSSecretUID: uid(6), TLSCA: trust.RegistryCA, TLSHostname: "saferus-console", BaselineImage: "localhost/saferus-console@sha256:" + strings.Repeat("7", 64), BaselineRuntimeDigest: "sha256:" + strings.Repeat("8", 64), RolloutSeconds: 300, SmokeSeconds: 30, ApprovedAt: time.Now().Add(-time.Second).UnixMilli(), ExpiresAt: time.Now().Add(30 * time.Minute).UnixMilli()}
	d := apps.Deployment{TypeMeta: meta.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"}, ObjectMeta: meta.ObjectMeta{Name: target.Deployment, Namespace: target.Namespace, UID: types.UID(target.DeploymentUID)}, Spec: apps.DeploymentSpec{Replicas: releasePtr(int32(1)), Template: core.PodTemplateSpec{Spec: core.PodSpec{ServiceAccountName: "saferus-client", AutomountServiceAccountToken: releasePtr(false), SecurityContext: &core.PodSecurityContext{RunAsNonRoot: releasePtr(true), RunAsUser: releasePtr(int64(1000)), SeccompProfile: &core.SeccompProfile{Type: core.SeccompProfileTypeRuntimeDefault}}, ImagePullSecrets: []core.LocalObjectReference{{Name: target.PullSecret}}, Containers: []core.Container{{Name: "console", Image: target.BaselineImage, SecurityContext: &core.SecurityContext{AllowPrivilegeEscalation: releasePtr(false), ReadOnlyRootFilesystem: releasePtr(true), Capabilities: &core.Capabilities{Drop: []core.Capability{"ALL"}}}}}}}}}
	s := core.Service{TypeMeta: meta.TypeMeta{APIVersion: "v1", Kind: "Service"}, ObjectMeta: meta.ObjectMeta{Name: target.Service, Namespace: target.Namespace, UID: types.UID(target.ServiceUID)}, Spec: core.ServiceSpec{Type: core.ServiceTypeClusterIP, ClusterIP: "10.96.0.8", Selector: map[string]string{"app": "saferus-console"}, Ports: []core.ServicePort{{Port: 8443, Protocol: core.ProtocolTCP, TargetPort: intstr.FromString("https")}}}}
	target.DeploymentSpecSHA256, _ = podtransport.ConsoleSpecDigest(d.Spec)
	target.ServiceSpecSHA256, _ = podtransport.ConsoleServiceDigest(s.Spec)
	observations, _ := json.Marshal(map[string]any{"deployment": d, "service": s})
	var description bytes.Buffer
	if e := describeReleaseSpecs(bytes.NewReader(observations), &description); e != nil {
		t.Fatal(e)
	}
	var specs map[string]string
	if json.Unmarshal(description.Bytes(), &specs) != nil || len(specs) != 4 || specs["version"] != "ai-sdlc.release-specs/v1" || specs["deploymentSpecSHA256"] != target.DeploymentSpecSHA256 || specs["serviceSpecSHA256"] != target.ServiceSpecSHA256 || specs["baselineImage"] != target.BaselineImage {
		t.Fatal("canonical public observation changed")
	}
	foreign := *d.DeepCopy()
	foreign.Namespace = "foreign"
	rejected, _ := json.Marshal(map[string]any{"deployment": foreign, "service": s})
	description.Reset()
	if describeReleaseSpecs(bytes.NewReader(rejected), &description) == nil || description.Len() != 0 {
		t.Fatal("foreign observation accepted")
	}

	raw, _ := json.Marshal(map[string]any{"target": target, "trust": trust, "deployment": d, "service": s})
	var output bytes.Buffer
	if e := compileReleaseInstallation(bytes.NewReader(raw), &output); e != nil {
		t.Fatal(e)
	}
	var result struct {
		Target releasing.Target `json:"target"`
		Digest string           `json:"targetSHA256"`
	}
	if json.Unmarshal(output.Bytes(), &result) != nil || result.Target != target || result.Digest != target.Digest() {
		t.Fatal("changed approved input")
	}
	for _, fault := range []string{"expired", "alias", "unknown", "null", "cluster", "deploymentUID", "baseline", "service", "privileged", "pull"} {
		t.Run(fault, func(t *testing.T) {
			var v map[string]any
			_ = json.Unmarshal(raw, &v)
			ta := v["target"].(map[string]any)
			dep := v["deployment"].(map[string]any)
			spec := dep["spec"].(map[string]any)
			pod := spec["template"].(map[string]any)["spec"].(map[string]any)
			switch fault {
			case "expired":
				ta["expiresAt"] = 1
			case "alias":
				ta["Scope"] = ta["scope"]
				delete(ta, "scope")
			case "unknown":
				v["approveObserved"] = true
			case "null":
				ta["rolloutSeconds"] = nil
			case "cluster":
				ta["clusterUID"] = uid(8)
			case "deploymentUID":
				dep["metadata"].(map[string]any)["uid"] = uid(9)
			case "baseline":
				pod["containers"].([]any)[0].(map[string]any)["image"] = "evil:latest"
			case "service":
				v["service"].(map[string]any)["spec"].(map[string]any)["clusterIP"] = "10.96.0.9"
			case "privileged":
				pod["containers"].([]any)[0].(map[string]any)["securityContext"].(map[string]any)["privileged"] = true
			case "pull":
				pod["imagePullSecrets"] = nil
			}
			changed, _ := json.Marshal(v)
			var out bytes.Buffer
			if compileReleaseInstallation(bytes.NewReader(changed), &out) == nil || out.Len() != 0 {
				t.Fatal("changed authority accepted")
			}
		})
	}
}
