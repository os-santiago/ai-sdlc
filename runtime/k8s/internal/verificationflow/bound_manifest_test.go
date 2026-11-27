package verificationflow

import (
	"strings"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
)

func TestBoundManifestRequiresTheExactInstalledDigest(t *testing.T) {
	t.Cleanup(func() { project.SetInstalled(nil) })
	m := project.Manifest{Version: project.ManifestVersion, Repository: "example/service", BaseBranch: "main",
		Verification: &project.Verification{Image: "registry.example/go@sha256:" + strings.Repeat("a", 64), Command: []string{"go", "test", "./..."},
			TimeoutSeconds: 60, CPU: "1", Memory: "1Gi", EphemeralStorage: "1Gi", Network: "none", Result: project.Result{ExitCode: true},
			Repeat: 1, TestGlobs: []string{"**/*_test.go"}},
		Publication: project.Publication{Allow: []string{"**/*.go"}, MaxChangedFiles: 5, MaxChangedBytes: 65536, MaxFileBytes: 32768},
		Merge:       project.Merge{RulesetID: 1, Required: []project.Check{{Context: "ci", AppID: 1}}, StrictChecks: true, ResolveThreads: true}}
	p := authority.Policy{Repository: "example/service", ProjectSHA256: m.Digest()}
	if BoundManifest(p) {
		t.Fatal("an empty registry satisfied a bound policy")
	}
	project.SetInstalled(&m)
	if !BoundManifest(p) {
		t.Fatal("the installed manifest was refused")
	}
	p.ProjectSHA256 = strings.Repeat("f", 64)
	if BoundManifest(p) {
		t.Fatal("another digest accepted")
	}
	// A reference policy binds no manifest, so an installed one belongs to
	// another policy load; a cleared registry refuses a bound policy.
	if BoundManifest(authority.Policy{Repository: project.Homedir}) {
		t.Fatal("an unbound policy accepted another load's manifest")
	}
	project.SetInstalled(nil)
	if BoundManifest(authority.Policy{Repository: "example/service", ProjectSHA256: m.Digest()}) {
		t.Fatal("a cleared registry satisfied a bound policy")
	}
	if !BoundManifest(authority.Policy{Repository: project.Homedir}) || !BoundManifest(authority.Policy{Repository: project.Saferus}) {
		t.Fatal("an unbound reference policy needs no manifest")
	}
}
