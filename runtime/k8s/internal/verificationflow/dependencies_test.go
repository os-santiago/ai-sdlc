package verificationflow

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/podtransport"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

func dependencyVerification() project.Verification {
	return project.Verification{Image: "docker.io/library/maven@sha256:" + strings.Repeat("b", 64), Command: []string{"mvn", "-o", "test"},
		TimeoutSeconds: 300, CPU: "1", Memory: "1Gi", EphemeralStorage: "1Gi", Network: "none", Result: project.Result{ExitCode: true},
		Repeat: 1, TestGlobs: []string{"**/src/test/**"},
		Dependencies: &project.Dependencies{CachePath: "/deps", KeyGlobs: []string{"**/pom.xml"}}}
}

func TestDependencyCacheIsTheSealedClaimOfTheBaseKey(t *testing.T) {
	base := workspacebundle.Bundle{Version: workspacebundle.Version, SourceSHA: strings.Repeat("c", 40),
		Files: []workspacebundle.File{{Path: "pom.xml", Mode: 0o644, Data: []byte("<project/>")}, {Path: "src/main/java/A.java", Mode: 0o644, Data: []byte("class A {}")}}}
	v := dependencyVerification()
	key, _ := v.DependencyKey([]project.DependencyFile{{Path: "pom.xml", Data: []byte("<project/>")}})
	for _, sealed := range []bool{true, false} {
		var calls atomic.Int32
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			if r.URL.Path != "/api/v1/namespaces/ai-sdlc-verifiers/persistentvolumeclaims/deps-"+key {
				t.Error("unexpected claim", r.URL.Path)
			}
			state := "sealed"
			if !sealed {
				state = "prefetching"
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim",
				"metadata": map[string]any{"name": "deps-" + key, "namespace": "ai-sdlc-verifiers", "labels": map[string]any{podtransport.DependencyStateLabel: state},
					"annotations": map[string]any{podtransport.DependencyKeyAnnotation: key}},
				"spec": map[string]any{"accessModes": []string{"ReadOnlyMany"}, "volumeName": "pv"}, "status": map[string]any{"phase": "Bound"}})
		}))
		client, err := podtransport.NewClient(podtransport.Config{APIServer: server.URL, CA: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), BearerToken: strings.Repeat("x", 32)})
		if err != nil {
			t.Fatal(err)
		}
		claim, err := dependencyCache(context.Background(), client, v, base)
		server.Close()
		if sealed && (err != nil || claim != "deps-"+key) || !sealed && (err != stopDependencyUnavailable || claim != "") || calls.Load() != 1 {
			t.Fatal(sealed, claim, err)
		}
		if DiagnosticStage(ControllerError{string(stopDependencyUnavailable)}) != "dependencies-unavailable" {
			t.Fatal("stop not named")
		}
	}
	// Without the section nothing is read and no claim is mounted.
	v.Dependencies = nil
	if claim, err := dependencyCache(context.Background(), nil, v, base); claim != "" || err != nil {
		t.Fatal("a manifest without dependencies read a cache")
	}
	// An unusable tree is a named stop, before any request.
	v = dependencyVerification()
	duplicate := base
	duplicate.Files = append(append([]workspacebundle.File{}, base.Files...), base.Files[0])
	if _, err := dependencyCache(context.Background(), nil, v, duplicate); err != stopDependencyKey || DiagnosticStage(ControllerError{err.Error()}) != "dependencies-key" {
		t.Fatal("key stop", err)
	}
}
