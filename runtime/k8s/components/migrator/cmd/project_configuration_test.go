package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
)

func TestProjectInstallationCompilerBindsOnlyTheInstallationRepository(t *testing.T) {
	m, err := authority.Bootstrap("example/service", "ai-sdlc-production")
	if err != nil {
		t.Fatal(err)
	}
	manifest := map[string]any{"version": project.ManifestVersion, "repository": "example/service", "baseBranch": "main",
		"verification": map[string]any{"image": "registry.example/go@sha256:" + strings.Repeat("a", 64), "command": []string{"sh", "-c", "go vet ./... && go test ./... > <report>"},
			"timeoutSeconds": 300, "cpu": "2", "memory": "2Gi", "ephemeralStorage": "1Gi", "network": "none",
			"result": map[string]any{"exitCode": true}, "repeat": 1, "testGlobs": []string{"**/*_test.go"}},
		"publication": map[string]any{"allow": []string{"**/*.go"}, "maxChangedFiles": 5, "maxChangedBytes": 65536, "maxFileBytes": 32768},
		"merge":       map[string]any{"rulesetID": 1, "required": []any{map[string]any{"context": "ci", "appID": 1}}, "strictChecks": true, "resolveThreads": true, "humanApprovals": 0}}
	raw, _ := json.Marshal(map[string]any{"policy": m.Policy, "manifest": manifest})
	var out bytes.Buffer
	if err := compileProjectInstallation(bytes.NewReader(raw), &out); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Version, ManifestJSON, ManifestSHA256 string
	}
	if json.Unmarshal(out.Bytes(), &result) != nil || result.Version != "ai-sdlc.project-installation/v1" {
		t.Fatal("output", out.String())
	}
	parsed, err := project.ParseManifest([]byte(result.ManifestJSON))
	if err != nil || parsed.Digest() != result.ManifestSHA256 || strings.Contains(out.String(), `\u00`) {
		t.Fatal("canonical manifest with shell metacharacters must round-trip", err, out.String())
	}
	if strings.Contains(out.String(), "executorQuota") {
		t.Fatal("executor quota without a generation toolchain")
	}
	manifest["generation"] = map[string]any{"toolchain": map[string]any{"root": "/usr/local/go", "path": []string{"bin"}}}
	raw, _ = json.Marshal(map[string]any{"policy": m.Policy, "manifest": manifest})
	out.Reset()
	if err := compileProjectInstallation(bytes.NewReader(raw), &out); err != nil {
		t.Fatal(err)
	}
	var sized struct {
		ExecutorQuota map[string]string `json:"executorQuota"`
	}
	if json.Unmarshal(out.Bytes(), &sized) != nil || sized.ExecutorQuota["limits.memory"] != "6Gi" || sized.ExecutorQuota["requests.memory"] != "1Gi" || len(sized.ExecutorQuota) != 4 {
		t.Fatal("toolchain executor quota", out.String())
	}
	manifest["repository"] = "example/other"
	raw, _ = json.Marshal(map[string]any{"policy": m.Policy, "manifest": manifest})
	if compileProjectInstallation(bytes.NewReader(raw), &bytes.Buffer{}) == nil {
		t.Fatal("manifest for another repository compiled")
	}
	raw, _ = json.Marshal(map[string]any{"policy": m.Policy, "manifest": manifest, "extra": true})
	if compileProjectInstallation(bytes.NewReader(raw), &bytes.Buffer{}) == nil {
		t.Fatal("unknown request field accepted")
	}
}

func TestProjectInstallationCompilerRefusesReferenceRepositories(t *testing.T) {
	m, err := authority.Bootstrap(project.Saferus, "ai-sdlc-production")
	if err != nil {
		t.Fatal(err)
	}
	reference, _ := project.Builtin(project.Saferus)
	raw, _ := json.Marshal(map[string]any{"policy": m.Policy, "manifest": reference})
	if compileProjectInstallation(bytes.NewReader(raw), &bytes.Buffer{}) == nil {
		t.Fatal("a manifest for a reference repository compiled")
	}
}

func TestVerifierQuotaCoversThreeConcurrentRuns(t *testing.T) {
	m := project.Manifest{Verification: &project.Verification{CPU: "500m", Memory: "512Mi"}}
	quota, err := verifierQuota(m)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"requests.cpu": "1560m", "requests.memory": "1632Mi", "limits.cpu": "2100m", "limits.memory": "1728Mi"}
	for k, v := range want {
		if quota[k] != v {
			t.Fatal(k, quota[k], v)
		}
	}
	if q, err := verifierQuota(project.Manifest{}); err != nil || q != nil {
		t.Fatal("a manifest without verification needs no quota")
	}
}
