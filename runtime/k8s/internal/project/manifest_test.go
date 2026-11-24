package project

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
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
		"upper repo":     func(m *Manifest) { m.Repository = "Example/Service" },
		"dash branch":    func(m *Manifest) { m.BaseBranch = "-x" },
		"head branch":    func(m *Manifest) { m.BaseBranch = "HEAD" },
		"lock branch":    func(m *Manifest) { m.BaseBranch = "main.lock" },
		"cpu":            func(m *Manifest) { m.Verification.CPU = "99999m" },
		"memory":         func(m *Manifest) { m.Verification.Memory = "999Gi" },
		"bare memory":    func(m *Manifest) { m.Verification.Memory = "5" },
		"double double":  func(m *Manifest) { m.Publication.Allow = []string{"**/**/*.go"} },
		"many doubles":   func(m *Manifest) { m.Publication.Allow = []string{"**/a/**/b/**/c/**/d/**/e"} },
		"glob charset":   func(m *Manifest) { m.Publication.Allow = []string{"src/a b.go"} },
		"ruleset":        func(m *Manifest) { m.Merge.RulesetID = 0 },
		"approvals":      func(m *Manifest) { m.Merge.HumanApprovals = 7 },
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
		".GIT/config.go": false, "a\u200c.go": false, "pkg/CODEOWNERS": false, ".gitmodules": false,
		"x/.gitattributes": false,
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

func TestDoubleStarMatchingIsPolynomial(t *testing.T) {
	deep := strings.TrimSuffix(strings.Repeat("a/", 500), "/")
	started := time.Now()
	for i := 0; i < 50; i++ {
		matchGlob("**/a/**/a/**/a/**/x", deep)
	}
	if time.Since(started) > 2*time.Second {
		t.Fatal("glob matching is not polynomial")
	}
}

func TestJSONEscapesInValuesAreAccepted(t *testing.T) {
	m := generic()
	m.Verification.Command = []string{"sh", "-c", "go vet ./... && go test ./... > out"}
	raw, _ := json.Marshal(m) // escapes & and > as JSON unicode escapes
	if _, err := ParseManifest(raw); err != nil {
		t.Fatal("escaped value refused", err)
	}
	if _, err := ParseManifest(m.Canonical()); err != nil || strings.Contains(string(m.Canonical()), string(rune(92))+"u00") {
		t.Fatal("canonical form must be unescaped and parseable", err)
	}
}

func TestUnicodeFoldedDuplicateKeysAndOmittedMergeFieldsAreRefused(t *testing.T) {
	raw, _ := json.Marshal(generic())
	literal := strings.Replace(string(raw), `"baseBranch":"main"`, "\"baseBranch\":\"main\",\"ba\u017feBranch\":\"dev\"", 1)
	escaped := strings.Replace(string(raw), `"baseBranch":"main"`, `"baseBranch":"main","ba\u017feBranch":"dev"`, 1)
	for _, folded := range []string{literal, escaped} {
		if _, err := ParseManifest([]byte(folded)); err == nil {
			t.Fatal("Unicode-folded duplicate key accepted", folded[:80])
		}
	}
	for _, nested := range []string{
		strings.Replace(string(raw), `"network":"none"`, "\"network\":\"none\",\"netwo\u212ak\":\"x\"", 1),
		strings.Replace(string(raw), `{"context":"ci"`, "{\"conte\u017ft\":\"x\",\"context\":\"ci\"", 1),
	} {
		if _, err := ParseManifest([]byte(nested)); err == nil {
			t.Fatal("nested or array non-ASCII key accepted")
		}
	}
	for _, field := range []string{`"strictChecks":true,`, `"resolveThreads":true,`, `,"humanApprovals":0`} {
		if _, err := ParseManifest([]byte(strings.Replace(string(raw), field, "", 1))); err == nil {
			t.Fatal("omitted merge field accepted", field)
		}
	}
}

func TestManifestRepositoriesMatchThePublicationSchema(t *testing.T) {
	for repository, ok := range map[string]bool{"example/service": true, "a/b": true, "owner/name.go": true,
		"owner/.github": false, "-owner/x": false, "owner/_x": false, "Owner/x": false, "a/..": false} {
		if validRepository(repository) != ok {
			t.Fatal(repository)
		}
	}
}

func withToolchain() Manifest {
	m := generic()
	m.Generation = &Generation{Toolchain: &Toolchain{Root: "/usr/local/go", Path: []string{"bin"}}}
	return m
}

// Digests recorded before the optional generation section existed: adding it
// must not change the canonical form of any manifest that omits it.
func TestManifestDigestsWithoutGenerationAreUnchanged(t *testing.T) {
	raw, err := os.ReadFile("../../../../examples/ai-sdlc-sample.project.json")
	if err != nil {
		t.Fatal(err)
	}
	sample, err := ParseManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	homedir, _ := Builtin(Homedir)
	saferus, _ := Builtin(Saferus)
	for name, c := range map[string]struct{ got, want string }{
		"sample":  {sample.Digest(), "7a7ea92f7c14d88e67bef18ed32e73a8e17552d70c3f0cafd1f1005a75e249a4"},
		"generic": {generic().Digest(), "130030f755518f0193dfcc67a1b5dd9cad34658086af0829d0ba3db64599b7f1"},
		"homedir": {homedir.Digest(), "03be9eb19d21eae907e037ece521db6dd2e3eabd88d5e73ce5f5893bbc4e1375"},
		"saferus": {saferus.Digest(), "300190706040b21a35e924c79ad30891ad74381d1174511a9247605d494276c5"},
	} {
		if c.got != c.want {
			t.Fatal("digest changed:", name, c.got)
		}
	}
	if strings.Contains(string(generic().Canonical()), "generation") {
		t.Fatal("absent generation section is encoded")
	}
}

func TestGenerationToolchainRoundTripsAndChangesTheDigest(t *testing.T) {
	m := withToolchain()
	m.Generation.Toolchain.Path = []string{"bin", "pkg/tool/linux_amd64"}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseManifest(m.Canonical())
	if err != nil || parsed.Digest() != m.Digest() || parsed.Generation.Toolchain.Root != "/usr/local/go" {
		t.Fatal("round trip", err)
	}
	if m.Digest() == generic().Digest() || !strings.HasSuffix(string(m.Canonical()),
		`"generation":{"toolchain":{"root":"/usr/local/go","path":["bin","pkg/tool/linux_amd64"]}}}`) {
		t.Fatal("generation must be bound into the canonical form", string(m.Canonical()))
	}
	for _, bad := range []string{
		strings.Replace(string(m.Canonical()), `"root"`, `"extra":1,"root"`, 1),
		strings.Replace(string(m.Canonical()), `"toolchain"`, `"image":"x","toolchain"`, 1),
		strings.Replace(string(m.Canonical()), `"root":"/usr/local/go"`, `"root":"/usr/local/go","root":"/opt/go"`, 1),
	} {
		if _, err := ParseManifest([]byte(bad)); err == nil {
			t.Fatal("accepted", bad)
		}
	}
}

func TestInvalidGenerationSectionsAreRefused(t *testing.T) {
	for name, change := range map[string]func(*Manifest){
		"no verification":   func(m *Manifest) { m.Verification = nil; m.Recipes = []Recipe{{Name: "x/v1", Marker: "none"}} },
		"empty generation":  func(m *Manifest) { m.Generation.Toolchain = nil },
		"relative root":     func(m *Manifest) { m.Generation.Toolchain.Root = "usr/local/go" },
		"slash root":        func(m *Manifest) { m.Generation.Toolchain.Root = "/" },
		"empty root":        func(m *Manifest) { m.Generation.Toolchain.Root = "" },
		"unclean root":      func(m *Manifest) { m.Generation.Toolchain.Root = "/usr/local/../go" },
		"dot root":          func(m *Manifest) { m.Generation.Toolchain.Root = "/usr/./go" },
		"trailing slash":    func(m *Manifest) { m.Generation.Toolchain.Root = "/usr/local/go/" },
		"double slash":      func(m *Manifest) { m.Generation.Toolchain.Root = "//usr/local/go" },
		"workspaces":        func(m *Manifest) { m.Generation.Toolchain.Root = "/workspaces/go" },
		"workspaces itself": func(m *Manifest) { m.Generation.Toolchain.Root = "/workspaces" },
		"tmp":               func(m *Manifest) { m.Generation.Toolchain.Root = "/tmp/go" },
		"runner":            func(m *Manifest) { m.Generation.Toolchain.Root = "/runner" },
		"proc":              func(m *Manifest) { m.Generation.Toolchain.Root = "/proc/self/root" },
		"sys":               func(m *Manifest) { m.Generation.Toolchain.Root = "/sys/x" },
		"dev":               func(m *Manifest) { m.Generation.Toolchain.Root = "/dev/shm" },
		"root charset":      func(m *Manifest) { m.Generation.Toolchain.Root = "/usr/local/go\u200c" },
		"root length":       func(m *Manifest) { m.Generation.Toolchain.Root = "/" + strings.Repeat("a", MaxToolchainRootBytes) },
		"no path":           func(m *Manifest) { m.Generation.Toolchain.Path = nil },
		"absolute path":     func(m *Manifest) { m.Generation.Toolchain.Path = []string{"/bin"} },
		"parent path":       func(m *Manifest) { m.Generation.Toolchain.Path = []string{"../bin"} },
		"inner parent":      func(m *Manifest) { m.Generation.Toolchain.Path = []string{"bin/../../x"} },
		"dot path":          func(m *Manifest) { m.Generation.Toolchain.Path = []string{"."} },
		"unclean path":      func(m *Manifest) { m.Generation.Toolchain.Path = []string{"bin/"} },
		"empty path":        func(m *Manifest) { m.Generation.Toolchain.Path = []string{""} },
		"colon path":        func(m *Manifest) { m.Generation.Toolchain.Path = []string{"bin:/tmp"} },
		"duplicate path":    func(m *Manifest) { m.Generation.Toolchain.Path = []string{"bin", "bin"} },
		"too many paths":    func(m *Manifest) { m.Generation.Toolchain.Path = strings.Split("a b c d e f g h i", " ") },
	} {
		m := withToolchain()
		tc := *m.Generation.Toolchain
		m.Generation = &Generation{Toolchain: &tc}
		change(&m)
		if m.Validate() == nil {
			t.Fatal("accepted invalid generation section:", name)
		}
	}
	for _, root := range []string{"/usr/local/go", "/opt/toolchain", "/workspaces2/go", "/devtools", "/usr/lib/jvm"} {
		if ValidateToolchainRoot(root) != nil {
			t.Fatal("refused valid root", root)
		}
	}
}
