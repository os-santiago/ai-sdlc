package project

import (
	"encoding/json"
	"strings"
	"testing"
)

func generic() Manifest {
	return Manifest{Version: ManifestVersion, Repository: "example/service", BaseBranch: "main",
		Verification: &Verification{Image: "registry.example/toolchains/go@sha256:" + strings.Repeat("a", 64),
			Command: []string{"go", "test", "./..."}, TimeoutSeconds: 300, CPU: "2", Memory: "2Gi", EphemeralStorage: "1Gi",
			Network: "none", Result: Result{ExitCode: true}, Repeat: 2, TestGlobs: []string{"**/*_test.go"}},
		Publication: Publication{Allow: []string{"**/*.go"}, Deny: []string{"go.mod", "go.sum", "vendor/**"},
			MaxChangedFiles: 10, MaxChangedBytes: 65536, MaxFileBytes: 32768},
		Merge: Merge{RulesetID: 1, Required: []Check{{Context: "ci", AppID: 15368}}, StrictChecks: true, ResolveThreads: true}}
}

func TestBuiltinManifestsParseAndAreStable(t *testing.T) {
	for _, repository := range []string{Homedir, Saferus} {
		m, ok := Builtin(repository)
		if !ok || m.Repository != repository || len(m.Recipes) != 1 || m.Verification != nil {
			t.Fatal("built-in manifest", repository, m)
		}
		again, _ := Builtin(repository)
		if m.Digest() != again.Digest() || len(m.Digest()) != 64 {
			t.Fatal("unstable digest", repository)
		}
	}
	if _, ok := Builtin("example/service"); ok {
		t.Fatal("unknown repository has a built-in manifest")
	}
}

func TestGenericManifestRoundTripsStrictly(t *testing.T) {
	m := generic()
	raw, _ := json.Marshal(m)
	parsed, err := ParseManifest(raw)
	if err != nil || parsed.Digest() != m.Digest() {
		t.Fatal("round trip", err)
	}
	for _, bad := range []string{
		strings.Replace(string(raw), `"version"`, `"unknown":1,"version"`, 1),
		strings.Replace(string(raw), `"baseBranch":"main"`, `"baseBranch":"main","baseBranch":"dev"`, 1),
		string(raw) + strings.Repeat(" ", MaxManifestBytes),
		"",
	} {
		if _, err := ParseManifest([]byte(bad)); err == nil {
			t.Fatal("accepted", bad[:min(len(bad), 60)])
		}
	}
}

func TestInvalidManifestsAreRefused(t *testing.T) {
	for name, change := range map[string]func(*Manifest){
		"version":        func(m *Manifest) { m.Version = "ai-sdlc.project/v2" },
		"repository":     func(m *Manifest) { m.Repository = "example/../x" },
		"branch":         func(m *Manifest) { m.BaseBranch = "a..b" },
		"no verifier":    func(m *Manifest) { m.Verification = nil },
		"mutable image":  func(m *Manifest) { m.Verification.Image = "golang:latest" },
		"network":        func(m *Manifest) { m.Verification.Network = "default" },
		"shell control":  func(m *Manifest) { m.Verification.Command = []string{"go", "test\n"} },
		"timeout":        func(m *Manifest) { m.Verification.TimeoutSeconds = 601 },
		"repeat":         func(m *Manifest) { m.Verification.Repeat = 4 },
		"exit code":      func(m *Manifest) { m.Verification.Result.ExitCode = false },
		"junit path":     func(m *Manifest) { m.Verification.Result.JUnitPath = "../report.xml" },
		"working dir":    func(m *Manifest) { m.Verification.WorkingDir = "/abs" },
		"no test globs":  func(m *Manifest) { m.Verification.TestGlobs = nil },
		"no allow":       func(m *Manifest) { m.Publication.Allow = nil },
		"class glob":     func(m *Manifest) { m.Publication.Allow = []string{"src/[a-z].go"} },
		"brace glob":     func(m *Manifest) { m.Publication.Allow = []string{"src/{a,b}.go"} },
		"partial double": func(m *Manifest) { m.Publication.Allow = []string{"src/**.go"} },
		"file over max":  func(m *Manifest) { m.Publication.MaxFileBytes = m.Publication.MaxChangedBytes + 1 },
		"no checks":      func(m *Manifest) { m.Merge.Required = nil },
		"dup check":      func(m *Manifest) { m.Merge.Required = append(m.Merge.Required, m.Merge.Required[0]) },
		"recipe name":    func(m *Manifest) { m.Recipes = []Recipe{{Name: "Bad Name", Marker: "none"}} },
		"recipe marker":  func(m *Manifest) { m.Recipes = []Recipe{{Name: "x/v1", Marker: "maybe"}} },
	} {
		m := generic()
		v := *m.Verification
		m.Verification = &v
		change(&m)
		if m.Validate() == nil {
			t.Fatal("accepted invalid manifest:", name)
		}
	}
}

func TestPublicationGlobsDenyTraversalAndAutomation(t *testing.T) {
	p := generic().Publication
	for file, allowed := range map[string]bool{
		"main.go": true, "internal/add/add.go": true, "internal/add/add_test.go": true,
		"go.mod": false, "vendor/x/y.go": false, "README.md": false,
		".github/workflows/ci.go": false, "../main.go": false, "/main.go": false, "a//b.go": false,
		"a/./b.go": false, "a\\b.go": false, "a/b.go/": false, ".git/config.go": false, "a\x00.go": false,
	} {
		if p.Allows(file) != allowed {
			t.Fatal("publication decision", file, allowed)
		}
	}
	everything := Publication{Allow: []string{"**"}, MaxChangedFiles: 1, MaxChangedBytes: 1, MaxFileBytes: 1}
	if everything.Allows(".github/workflows/release.yml") || !everything.Allows("docs/a.md") {
		t.Fatal("repository automation must always be denied")
	}
	v := generic().Verification
	if !v.MatchesTest("pkg/a_test.go") || v.MatchesTest("pkg/a.go") || v.MatchesTest("../a_test.go") {
		t.Fatal("test glob")
	}
}

func TestDoubleStarMatchesZeroOrMoreSegments(t *testing.T) {
	for _, c := range []struct {
		glob, file string
		match      bool
	}{
		{"**/*.go", "a.go", true}, {"**/*.go", "a/b/c.go", true}, {"src/**", "src/a/b", true},
		{"src/**", "src", true}, {"src/**/x.go", "src/x.go", true}, {"src/*.go", "src/a/b.go", false},
		{"a?c", "abc", true}, {"a?c", "abbc", false},
	} {
		if matchGlob(c.glob, c.file) != c.match {
			t.Fatal(c.glob, c.file, c.match)
		}
	}
}
