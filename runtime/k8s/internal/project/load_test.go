package project

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
)

func policyFor(t *testing.T, repository string) authority.Policy {
	t.Helper()
	m, err := authority.Bootstrap(repository, "ai-sdlc-production")
	if err != nil {
		t.Fatal(err)
	}
	return m.Policy
}

func TestPoliciesWithoutDigestUseOnlyReferenceBuiltins(t *testing.T) {
	m, err := Resolve(policyFor(t, Saferus), nil)
	if err != nil || m.Repository != Saferus {
		t.Fatal("built-in", err)
	}
	if _, err := Resolve(policyFor(t, "example/service"), nil); err == nil {
		t.Fatal("non-reference repository without a bound manifest accepted")
	}
	raw, _ := json.Marshal(generic())
	if _, err := Resolve(policyFor(t, Saferus), raw); err == nil {
		t.Fatal("an installed manifest without a policy digest must not be used")
	}
}

func TestInstalledManifestMustMatchDigestAndRepository(t *testing.T) {
	manifest := generic()
	raw, _ := json.Marshal(manifest)
	policy := policyFor(t, "example/service")
	policy.ProjectSHA256 = manifest.Digest()
	if policy.Validate() != nil {
		t.Fatal("policy with a project digest must validate")
	}
	if got, err := Resolve(policy, raw); err != nil || got.Digest() != manifest.Digest() {
		t.Fatal("bound manifest refused", err)
	}
	changed := manifest
	changed.Publication.Allow = []string{"**"}
	other, _ := json.Marshal(changed)
	if _, err := Resolve(policy, other); err == nil {
		t.Fatal("manifest with another digest accepted")
	}
	elsewhere := policyFor(t, "example/other")
	elsewhere.ProjectSHA256 = manifest.Digest()
	if _, err := Resolve(elsewhere, raw); err == nil {
		t.Fatal("manifest for another repository accepted")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, policy); err != nil {
		t.Fatal("load", err)
	}
	if _, err := Load(filepath.Join(dir, "missing.json"), policy); err == nil {
		t.Fatal("missing bound manifest accepted")
	}
	if err := os.WriteFile(path, append(raw, []byte(strings.Repeat(" ", MaxManifestBytes))...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, policy); err == nil {
		t.Fatal("oversized manifest accepted")
	}
}

func TestPolicyRevisionIsUnchangedWithoutAProjectDigest(t *testing.T) {
	policy := policyFor(t, Saferus)
	raw, _ := json.Marshal(policy)
	if strings.Contains(string(raw), "projectSHA256") {
		t.Fatal("empty project digest must be omitted from the policy revision")
	}
	reparsed, err := authority.ParsePolicy(raw)
	if err != nil || reparsed.Revision() != policy.Revision() {
		t.Fatal("revision changed", err)
	}
	policy.ProjectSHA256 = "not-a-digest"
	if policy.Validate() == nil {
		t.Fatal("malformed project digest accepted")
	}
	policy.ProjectSHA256 = strings.Repeat("a", 64)
	bound, _ := json.Marshal(policy)
	if parsed, err := authority.ParsePolicy(bound); err != nil || parsed.ProjectSHA256 != policy.ProjectSHA256 || parsed.Revision() == reparsed.Revision() {
		t.Fatal("bound digest must change the revision", err)
	}
}

func TestAMountedManifestWithoutAPolicyDigestIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.json")
	if _, err := Load(path, policyFor(t, Saferus)); err != nil {
		t.Fatal("reference without a mount must use the built-in", err)
	}
	raw, _ := json.Marshal(generic())
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, policyFor(t, Saferus)); err == nil {
		t.Fatal("unbound mounted manifest silently ignored")
	}
}

func TestAReferenceRepositoryCannotBindAnInstalledManifest(t *testing.T) {
	reference, _ := Builtin(Saferus)
	reference.Publication.Allow = []string{"**"}
	reference.Merge.HumanApprovals = 0
	raw, _ := json.Marshal(reference)
	policy := policyFor(t, Saferus)
	policy.ProjectSHA256 = reference.Digest()
	if _, err := Resolve(policy, raw); err == nil {
		t.Fatal("a reference repository's built-in manifest was replaced")
	}
}

func TestLoadRefusesAFIFOWithoutBlocking(t *testing.T) {
	manifest := generic()
	policy := policyFor(t, "example/service")
	policy.ProjectSHA256 = manifest.Digest()
	path := filepath.Join(t.TempDir(), "manifest.json")
	if err := syscallMkfifo(path); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, policy); err == nil {
		t.Fatal("a FIFO manifest accepted")
	}
}

func TestHiddenCharactersInCommandsAreRefused(t *testing.T) {
	for _, hidden := range []rune{0x202e, 0x200b, 0x2066} {
		manifest := generic()
		manifest.Verification.Command = []string{"go", "test", "./..." + string(hidden)}
		if manifest.Validate() == nil {
			t.Fatalf("hidden character %U in a command accepted", hidden)
		}
	}
}

// Go's decoder matches keys case-insensitively, so "Generation" or "TOOLCHAIN"
// parse to the same manifest and digest; the installed bytes must still be
// canonical, so the chart (case-sensitive) sees the same toolchain.
func TestInstalledManifestMustBeCanonical(t *testing.T) {
	manifest := generic()
	manifest.Generation = &Generation{Toolchain: &Toolchain{Root: "/usr/local/go", Path: []string{"bin"}}}
	canonical := manifest.Canonical()
	policy := policyFor(t, "example/service")
	policy.ProjectSHA256 = manifest.Digest()
	if _, err := Resolve(policy, canonical); err != nil {
		t.Fatal("canonical manifest refused", err)
	}
	for _, variant := range []string{
		strings.Replace(string(canonical), `"generation":`, `"Generation":`, 1),
		strings.Replace(string(canonical), `"toolchain":`, `"TOOLCHAIN":`, 1),
		strings.Replace(string(canonical), `"version":`, `"Version":`, 1),
		string(canonical) + "\n",
		" " + string(canonical),
	} {
		parsed, err := ParseManifest([]byte(variant))
		if strings.Contains(variant, "TOOLCHAIN") && (err != nil || parsed.Digest() != manifest.Digest()) {
			t.Fatal("fixture: Go accepts the capitalized key with the same digest", err)
		}
		if _, err := Resolve(policy, []byte(variant)); !errors.Is(err, ErrManifestNotCanonical) {
			t.Fatal("non-canonical installed manifest accepted", variant[:24], err)
		}
	}
}
