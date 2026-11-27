package execution

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
)

func TestWorkspaceNoteOnlyForRepositoryTestsInstallations(t *testing.T) {
	t.Cleanup(func() { project.SetInstalled(nil) })
	if WorkspaceFor(project.Homedir, "") != "" || WorkspaceFor("example/service", "") != "" {
		t.Fatal("references and unbound repositories get no note")
	}
	m := project.Manifest{Version: project.ManifestVersion, Repository: "example/service", BaseBranch: "main",
		Verification: &project.Verification{Image: "registry.example/go@sha256:" + strings.Repeat("a", 64), Command: []string{"go", "test", "./..."},
			TimeoutSeconds: 60, CPU: "1", Memory: "1Gi", EphemeralStorage: "1Gi", Network: "none", Result: project.Result{ExitCode: true},
			Repeat: 1, TestGlobs: []string{"**/*_test.go"}}}
	project.SetInstalled(&m)
	if WorkspaceFor("example/service", m.Digest()) != executor.WorkspaceOfflineTests {
		t.Fatal("repository-tests installation must mark the workspace")
	}
	got := VerificationFor("example/service", m.Digest())
	if !reflect.DeepEqual(got, &executor.Verification{Command: []string{"go", "test", "./..."}}) {
		t.Fatalf("verification %+v", got)
	}
	got.Command[0] = "changed"
	if m.Verification.Command[0] != "go" {
		t.Fatal("aliased verification command")
	}
	if VerificationFor(project.Homedir, "") != nil {
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
	if ToolchainFor("example/service", m.Digest()) != nil || ToolchainFor(project.Homedir, "") != nil {
		t.Fatal("toolchain without generation.toolchain")
	}
	m.Generation = &project.Generation{Toolchain: &project.Toolchain{Root: "/usr/local/go", Path: []string{"bin"}}}
	project.SetInstalled(&m)
	got := ToolchainFor("example/service", m.Digest())
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
	if ToolchainFor("example/service", m.Digest()).Env != nil {
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
	got := ToolchainFor("example/service", m.Digest())
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
	got = ToolchainFor("example/service", m.Digest())
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
	if err := addRepositoryContext(&a, m.Digest()); err != nil {
		t.Fatal(err)
	}
	if a.Workspace != executor.WorkspaceOfflineTests || a.Toolchain != nil ||
		!reflect.DeepEqual(a.Verification, &executor.Verification{Command: []string{"python3", "-m", "unittest"}, WorkingDir: "svc"}) {
		t.Fatalf("offline assignment without a toolchain %+v", a)
	}
	m.Generation = &project.Generation{Toolchain: &project.Toolchain{Root: "/usr/local/go", Path: []string{"bin"}}}
	m.Verification.Command = []string{"go", "test", "./..."}
	project.SetInstalled(&m)
	a = executor.Assignment{Repository: "example/service"}
	if err := addRepositoryContext(&a, m.Digest()); err != nil {
		t.Fatal(err)
	}
	if a.Workspace != executor.WorkspaceOfflineTests || a.Toolchain == nil || a.Verification != nil {
		t.Fatalf("toolchain assignment %+v", a)
	}
	project.SetInstalled(nil)
	a = executor.Assignment{Repository: project.Homedir}
	if err := addRepositoryContext(&a, ""); err != nil {
		t.Fatal(err)
	}
	if a.Workspace != "" || a.Toolchain != nil || a.Verification != nil {
		t.Fatalf("reference assignment %+v", a)
	}
}

// The workspace context comes only from the manifest the authorizing policy
// binds: after a reload that installs another manifest or clears it,
// preparation refuses at "manifest" instead of taking that load's toolchain,
// verification command or start, or preparing without them.
func TestRepositoryContextRefusesAnotherPolicyLoadsManifest(t *testing.T) {
	t.Cleanup(func() { project.SetInstalled(nil) })
	image := "registry.example/go@sha256:" + strings.Repeat("a", 64)
	authorized := project.Manifest{Version: project.ManifestVersion, Repository: "example/service", BaseBranch: "main",
		Verification: &project.Verification{Image: image, Command: []string{"go", "test", "./..."},
			TimeoutSeconds: 60, CPU: "1", Memory: "1Gi", EphemeralStorage: "1Gi", Network: "none", Result: project.Result{ExitCode: true},
			Repeat: 1, TestGlobs: []string{"**/*_test.go"}}}
	other := authorized
	other.Generation = &project.Generation{Toolchain: &project.Toolchain{Root: "/usr/local/go", Path: []string{"bin"}}}
	digest := authorized.Digest()
	for name, install := range map[string]*project.Manifest{"another load": &other, "cleared": nil} {
		project.SetInstalled(install)
		a := executor.Assignment{Repository: "example/service"}
		err := addRepositoryContext(&a, digest)
		var named interface{ AuthorityStep() string }
		if !errors.Is(err, ErrAuthority) || !errors.As(err, &named) || named.AuthorityStep() != "manifest" {
			t.Fatalf("%s: %v", name, err)
		}
		if a.Workspace != "" || a.Toolchain != nil || a.Verification != nil {
			t.Fatalf("%s: context from another manifest %+v", name, a)
		}
		if WorkspaceFor("example/service", digest) != "" || VerificationFor("example/service", digest) != nil ||
			ToolchainFor("example/service", digest) != nil || StartSecondsFor("example/service", digest) != 0 {
			t.Fatalf("%s: context from another manifest", name)
		}
	}
	// A reference policy binds no manifest: an installed one is another load's.
	project.SetInstalled(&other)
	a := executor.Assignment{Repository: project.Homedir}
	if err := addRepositoryContext(&a, ""); !errors.Is(err, ErrAuthority) {
		t.Fatalf("reference with an installed manifest: %v", err)
	}
	project.SetInstalled(&authorized)
	if err := addRepositoryContext(&a, digest); err == nil {
		t.Fatal("a reference repository took the installed manifest")
	}
	a = executor.Assignment{Repository: "example/service"}
	if err := addRepositoryContext(&a, digest); err != nil || a.Workspace != executor.WorkspaceOfflineTests || a.Verification == nil {
		t.Fatalf("the bound manifest was refused: %v %+v", err, a)
	}
}
