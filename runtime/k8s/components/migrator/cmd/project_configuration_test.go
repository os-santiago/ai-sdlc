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
		"verification": map[string]any{"image": "registry.example/go@sha256:" + strings.Repeat("a", 64), "command": []string{"go", "test", "./..."},
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
	if err != nil || parsed.Digest() != result.ManifestSHA256 {
		t.Fatal("canonical manifest and digest disagree", err)
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
