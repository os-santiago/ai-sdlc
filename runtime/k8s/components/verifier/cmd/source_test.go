package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/sourcefetch"
)

// A fake value with a token's shape; never a real credential.
var fakeToken = "github_pat_" + strings.Repeat("F", 82)

func TestInstalledSourceAcceptsOnlyTheChartMount(t *testing.T) {
	env := func(value string) func(string) string {
		return func(key string) string {
			if key == sourceCredentialEnvironment {
				return value
			}
			return ""
		}
	}
	anonymous, err := installedSource(env(""))
	if err != nil || anonymous.Authenticated() {
		t.Fatal("an installation without the credential must read anonymously")
	}
	installed, err := installedSource(env(sourceCredentialPath))
	if err != nil || !installed.Authenticated() {
		t.Fatal("the chart's credential mount was ignored")
	}
	// Any other path could name another mounted Secret, such as the identity.
	for _, path := range []string{"/etc/ai-sdlc/identity/identity.json", "/var/run/ai-sdlc/review-model/token", "credential.json"} {
		if _, err := installedSource(env(path)); err == nil {
			t.Fatal("unexpected credential path accepted: " + path)
		}
	}
}

func TestSourceReadinessChecksTheCredentialWithoutRevealingIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential.json")
	document := `{"version":1,"host":"github.com","repository":"fixture/product","access":"read","token":"` + fakeToken + `"}`
	if err := os.WriteFile(path, []byte(document), 0o400); err != nil {
		t.Fatal(err)
	}
	reader, err := sourcefetch.InstalledReader(path)
	if err != nil {
		t.Fatal(err)
	}
	policy := func(repository string, err error) func() (authority.Policy, authority.Identity, error) {
		return func() (authority.Policy, authority.Identity, error) {
			return authority.Policy{Repository: repository}, authority.Identity{}, err
		}
	}
	if sourceReady(reader, policy("fixture/product", nil)) != nil {
		t.Fatal("a valid credential for the policy repository is ready")
	}
	unavailable := errors.New("policy unavailable")
	if !errors.Is(sourceReady(reader, policy("fixture/product", unavailable)), unavailable) {
		t.Fatal("readiness ignored the policy")
	}
	err = sourceReady(reader, policy("other/product", nil))
	if err == nil || strings.Contains(err.Error(), fakeToken) {
		t.Fatal("a credential bound to another repository must keep the verifier unready, without revealing it")
	}
	called := false
	if sourceReady(sourcefetch.Reader{}, func() (authority.Policy, authority.Identity, error) {
		called = true
		return authority.Policy{}, authority.Identity{}, unavailable
	}) != nil || called {
		t.Fatal("the anonymous reader needs no credential check")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if sourceReady(reader, policy("fixture/product", nil)) == nil {
		t.Fatal("a missing mounted credential must keep the verifier unready")
	}
}

// The credential's location is known only to this command. The packages that
// build verification, test and reviewer pods and their ConfigMaps never name
// it, so no pod or ConfigMap they create can mount or reference it.
func TestPodBuildersNeverNameTheSourceCredential(t *testing.T) {
	for _, dir := range []string{"podtransport", "verificationflow", "reviewflow", "verification", "repositorytests"} {
		files, err := filepath.Glob(filepath.Join("..", "..", "..", "internal", dir, "*.go"))
		if err != nil || len(files) == 0 {
			t.Fatal("package sources unavailable: " + dir)
		}
		for _, file := range files {
			if strings.HasSuffix(file, "_test.go") {
				continue
			}
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{sourceCredentialEnvironment, "/var/run/ai-sdlc/source", "ai-sdlc-source-reader"} {
				if strings.Contains(string(raw), name) {
					t.Fatalf("%s names the source credential (%s)", file, name)
				}
			}
		}
	}
}
