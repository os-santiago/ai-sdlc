package execution

import (
	"strings"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
)

func TestWorkspaceNoteOnlyForRepositoryTestsInstallations(t *testing.T) {
	t.Cleanup(func() { project.SetInstalled(nil) })
	if WorkspaceFor(project.Homedir) != "" || WorkspaceFor("example/service") != "" {
		t.Fatal("references and unbound repositories get no note")
	}
	m := project.Manifest{Version: project.ManifestVersion, Repository: "example/service", BaseBranch: "main",
		Verification: &project.Verification{Image: "registry.example/go@sha256:" + strings.Repeat("a", 64), Command: []string{"go", "test", "./..."},
			TimeoutSeconds: 60, CPU: "1", Memory: "1Gi", EphemeralStorage: "1Gi", Network: "none", Result: project.Result{ExitCode: true},
			Repeat: 1, TestGlobs: []string{"**/*_test.go"}}}
	project.SetInstalled(&m)
	if WorkspaceFor("example/service") != executor.WorkspaceOfflineTests {
		t.Fatal("repository-tests installation must mark the workspace")
	}
}
