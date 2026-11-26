package execution

import (
	"reflect"
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
	got := VerificationFor("example/service")
	if !reflect.DeepEqual(got, &executor.Verification{Command: []string{"go", "test", "./..."}}) {
		t.Fatalf("verification %+v", got)
	}
	got.Command[0] = "changed"
	if m.Verification.Command[0] != "go" {
		t.Fatal("aliased verification command")
	}
	if VerificationFor(project.Homedir) != nil {
		t.Fatal("reference repositories name no verification command")
	}
}

func TestToolchainOnlyForManifestsThatDeclareOne(t *testing.T) {
	t.Cleanup(func() { project.SetInstalled(nil) })
	image := "registry.example/go@sha256:" + strings.Repeat("a", 64)
	m := project.Manifest{Version: project.ManifestVersion, Repository: "example/service", BaseBranch: "main",
		Verification: &project.Verification{Image: image, Command: []string{"go", "test", "./..."}, WorkingDir: "svc",
			TimeoutSeconds: 60, CPU: "1", Memory: "1Gi", EphemeralStorage: "1Gi", Network: "none", Result: project.Result{ExitCode: true},
			Repeat: 1, TestGlobs: []string{"**/*_test.go"}, Env: map[string]string{"GOTOOLCHAIN": "local", "PATH": "/usr/local/go/bin:/usr/bin", "GIT_DIR": "x", "OPENSSL_CONF": "/x"}}}
	project.SetInstalled(&m)
	if ToolchainFor("example/service") != nil || ToolchainFor(project.Homedir) != nil {
		t.Fatal("toolchain without generation.toolchain")
	}
	m.Generation = &project.Generation{Toolchain: &project.Toolchain{Root: "/usr/local/go", Path: []string{"bin"}}}
	project.SetInstalled(&m)
	got := ToolchainFor("example/service")
	want := &executor.Toolchain{Image: image, Root: "/usr/local/go", Path: []string{"bin"}, Command: []string{"go", "test", "./..."}, WorkingDir: "svc",
		Env: map[string]string{"GOTOOLCHAIN": "local"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("toolchain %+v", got)
	}
	// The assignment copy is independent of the installed manifest.
	got.Path[0] = "changed"
	if m.Generation.Toolchain.Path[0] != "bin" {
		t.Fatal("aliased toolchain path")
	}
	m.Verification.Env = map[string]string{"PATH": "/usr/local/go/bin"}
	project.SetInstalled(&m)
	if ToolchainFor("example/service").Env != nil {
		t.Fatal("only reserved variables must leave no env")
	}
}

// The worker mounts the copy at /toolchain/root: an absolute command and
// variable entries under the declared root are relocated for generation only.
func TestToolchainRelocatesPathsUnderTheRootForGeneration(t *testing.T) {
	t.Cleanup(func() { project.SetInstalled(nil) })
	image := "registry.example/go@sha256:" + strings.Repeat("a", 64)
	m := project.Manifest{Version: project.ManifestVersion, Repository: "example/service", BaseBranch: "main",
		Verification: &project.Verification{Image: image, Command: []string{"/opt/reviewed-go/bin/go", "test", "./..."},
			TimeoutSeconds: 60, CPU: "1", Memory: "1Gi", EphemeralStorage: "1Gi", Network: "none", Result: project.Result{ExitCode: true},
			Repeat: 1, TestGlobs: []string{"**/*_test.go"}, Env: map[string]string{"GOROOT": "/opt/reviewed-go", "GOTOOLCHAIN": "local"}},
		Generation: &project.Generation{Toolchain: &project.Toolchain{Root: "/opt/reviewed-go", Path: []string{"bin"}}}}
	project.SetInstalled(&m)
	got := ToolchainFor("example/service")
	if !reflect.DeepEqual(got.Command, []string{"/toolchain/root/bin/go", "test", "./..."}) ||
		!reflect.DeepEqual(got.Env, map[string]string{"GOROOT": "/toolchain/root", "GOTOOLCHAIN": "local"}) || got.Root != "/opt/reviewed-go" {
		t.Fatalf("relocated toolchain %+v", got)
	}
	// The verifier keeps the manifest's own command and environment.
	installed, _ := project.Lookup("example/service")
	if installed.Verification.Command[0] != "/opt/reviewed-go/bin/go" || installed.Verification.Env["GOROOT"] != "/opt/reviewed-go" {
		t.Fatal("verification settings changed")
	}
	// The prompt names the relocated command; the assignment accepts it.
	a := executor.Assignment{Workspace: executor.WorkspaceOfflineTests, Toolchain: got}
	if !strings.Contains(executor.BuildPrompt(a, "policy"), "\n/toolchain/root/bin/go test ./...\n") {
		t.Fatal("prompt does not name the relocated command")
	}
	// Without GOROOT and with a relative command nothing is rewritten.
	m.Verification.Command = []string{"go", "test", "./..."}
	m.Verification.Env = map[string]string{"GOTOOLCHAIN": "local"}
	project.SetInstalled(&m)
	got = ToolchainFor("example/service")
	if !reflect.DeepEqual(got.Command, []string{"go", "test", "./..."}) || !reflect.DeepEqual(got.Env, map[string]string{"GOTOOLCHAIN": "local"}) {
		t.Fatalf("toolchain without paths under the root %+v", got)
	}
}

// Preparation names the verification command only in an offline assignment
// without a toolchain; a toolchain carries the command itself.
func TestPreparationAddsVerificationOnlyWithoutAToolchain(t *testing.T) {
	t.Cleanup(func() { project.SetInstalled(nil) })
	image := "registry.example/python@sha256:" + strings.Repeat("a", 64)
	m := project.Manifest{Version: project.ManifestVersion, Repository: "example/service", BaseBranch: "main",
		Verification: &project.Verification{Image: image, Command: []string{"python3", "-m", "unittest"}, WorkingDir: "svc",
			TimeoutSeconds: 60, CPU: "1", Memory: "1Gi", EphemeralStorage: "1Gi", Network: "none", Result: project.Result{ExitCode: true},
			Repeat: 1, TestGlobs: []string{"**/test_*.py"}}}
	project.SetInstalled(&m)
	a := executor.Assignment{Repository: "example/service"}
	addRepositoryContext(&a)
	if a.Workspace != executor.WorkspaceOfflineTests || a.Toolchain != nil ||
		!reflect.DeepEqual(a.Verification, &executor.Verification{Command: []string{"python3", "-m", "unittest"}, WorkingDir: "svc"}) {
		t.Fatalf("offline assignment without a toolchain %+v", a)
	}
	m.Generation = &project.Generation{Toolchain: &project.Toolchain{Root: "/usr/local/go", Path: []string{"bin"}}}
	m.Verification.Command = []string{"go", "test", "./..."}
	project.SetInstalled(&m)
	a = executor.Assignment{Repository: "example/service"}
	addRepositoryContext(&a)
	if a.Workspace != executor.WorkspaceOfflineTests || a.Toolchain == nil || a.Verification != nil {
		t.Fatalf("toolchain assignment %+v", a)
	}
	a = executor.Assignment{Repository: project.Homedir}
	addRepositoryContext(&a)
	if a.Workspace != "" || a.Toolchain != nil || a.Verification != nil {
		t.Fatalf("reference assignment %+v", a)
	}
}
