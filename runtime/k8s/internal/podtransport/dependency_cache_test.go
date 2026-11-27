package podtransport

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func sealedClaim(key string) core.PersistentVolumeClaim {
	return core.PersistentVolumeClaim{
		TypeMeta: meta.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
		ObjectMeta: meta.ObjectMeta{Name: "deps-" + key, Namespace: VerifierNamespace,
			Labels: map[string]string{DependencyStateLabel: DependencySealed}, Annotations: map[string]string{DependencyKeyAnnotation: key}},
		Spec:   core.PersistentVolumeClaimSpec{AccessModes: []core.PersistentVolumeAccessMode{core.ReadOnlyMany}, VolumeName: "pv-cache"},
		Status: core.PersistentVolumeClaimStatus{Phase: core.ClaimBound},
	}
}

func TestDependencyCacheMustBeSealedBoundAndKeyed(t *testing.T) {
	key := strings.Repeat("a", 64)
	for name, change := range map[string]func(*core.PersistentVolumeClaim, *int){
		"sealed": func(*core.PersistentVolumeClaim, *int) {},
		"rwo": func(c *core.PersistentVolumeClaim, _ *int) {
			c.Spec.AccessModes = []core.PersistentVolumeAccessMode{core.ReadWriteOnce}
		},
		"missing":   func(_ *core.PersistentVolumeClaim, status *int) { *status = 404 },
		"forbidden": func(_ *core.PersistentVolumeClaim, status *int) { *status = 403 },
		"unsealed":  func(c *core.PersistentVolumeClaim, _ *int) { c.Labels[DependencyStateLabel] = "prefetching" },
		"other key": func(c *core.PersistentVolumeClaim, _ *int) {
			c.Annotations[DependencyKeyAnnotation] = strings.Repeat("b", 64)
		},
		"pending":  func(c *core.PersistentVolumeClaim, _ *int) { c.Status.Phase = core.ClaimPending },
		"unbound":  func(c *core.PersistentVolumeClaim, _ *int) { c.Spec.VolumeName = "" },
		"deleting": func(c *core.PersistentVolumeClaim, _ *int) { now := meta.Now(); c.DeletionTimestamp = &now },
		"rwx": func(c *core.PersistentVolumeClaim, _ *int) {
			c.Spec.AccessModes = append(c.Spec.AccessModes, core.ReadWriteMany)
		},
		"no modes":    func(c *core.PersistentVolumeClaim, _ *int) { c.Spec.AccessModes = nil },
		"other name":  func(c *core.PersistentVolumeClaim, _ *int) { c.Name = "deps-other" },
		"other space": func(c *core.PersistentVolumeClaim, _ *int) { c.Namespace = "ai-sdlc-dependencies" },
	} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != "GET" || r.URL.Path != "/api/v1/namespaces/ai-sdlc-verifiers/persistentvolumeclaims/deps-"+key {
					t.Error("unexpected request", r.Method, r.URL.Path)
				}
				claim, status := sealedClaim(key), 200
				change(&claim, &status)
				writeResource(w, status, claim)
			}))
			defer server.Close()
			client, err := NewClient(Config{APIServer: server.URL, CA: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), BearerToken: strings.Repeat("x", 32)})
			if err != nil {
				t.Fatal(err)
			}
			err = client.CheckDependencyCache(context.Background(), key)
			if (err == nil) != (name == "sealed" || name == "rwo") || calls.Load() != 1 {
				t.Fatal(name, err, calls.Load())
			}
		})
	}
	var nilClient *Client
	if nilClient.CheckDependencyCache(context.Background(), strings.Repeat("a", 64)) == nil {
		t.Fatal("nil client")
	}
	client, _ := NewClient(Config{APIServer: "https://127.0.0.1:1", BearerToken: strings.Repeat("x", 32)})
	if client != nil && client.CheckDependencyCache(context.Background(), "../x") == nil {
		t.Fatal("invalid key")
	}
}
