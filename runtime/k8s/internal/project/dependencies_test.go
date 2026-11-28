package project

import (
	"os"
	"slices"
	"strings"
	"testing"
)

func withDependencies() Manifest {
	m := generic()
	m.Verification.Image = "docker.io/library/maven@sha256:" + strings.Repeat("b", 64)
	m.Verification.Command = []string{"mvn", "-B", "-o", "-Dmaven.repo.local=/tmp/m2", "-Dmaven.repo.local.tail=/deps", "test"}
	m.Verification.TestGlobs = []string{"**/src/test/**"}
	m.Verification.Dependencies = &Dependencies{CachePath: "/deps", KeyGlobs: []string{"**/pom.xml", ".mvn/**", "mvnw"}}
	return m
}

func TestDependenciesRoundTripAndChangeOnlyOptedInDigests(t *testing.T) {
	m := withDependencies()
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseManifest(m.Canonical())
	if err != nil || parsed.Digest() != m.Digest() {
		t.Fatal("round trip", err)
	}
	if !strings.Contains(string(m.Canonical()), `"dependencies":{"cachePath":"/deps","keyGlobs":["**/pom.xml",".mvn/**","mvnw"]}`) {
		t.Fatal("dependencies must be bound into the canonical form", string(m.Canonical()))
	}
	if strings.Contains(string(generic().Canonical()), "dependencies") {
		t.Fatal("absent dependencies section is encoded")
	}
	for _, bad := range []string{
		strings.Replace(string(m.Canonical()), `"cachePath"`, `"extra":1,"cachePath"`, 1),
		strings.Replace(string(m.Canonical()), `"cachePath":"/deps"`, `"cachePath":"/deps","cachePath":"/opt"`, 1),
		strings.Replace(string(m.Canonical()), `"keyGlobs":["**/pom.xml",".mvn/**","mvnw"]`, `"keyGlobs":null`, 1),
		strings.Replace(string(m.Canonical()), `"cachePath":"/deps"`, `"cachePath":"/deps","prefetch":[["mvn"]]`, 1),
	} {
		if _, err := ParseManifest([]byte(bad)); err == nil {
			t.Fatal("accepted", bad)
		}
	}
}

func TestInvalidDependenciesSectionsAreRefused(t *testing.T) {
	globs := make([]string, MaxDependencyKeyGlobs+1)
	for i := range globs {
		globs[i] = "m" + strings.Repeat("x", i) + "/pom.xml"
	}
	for name, change := range map[string]func(*Dependencies){
		"relative cache":  func(d *Dependencies) { d.CachePath = "deps" },
		"no cache":        func(d *Dependencies) { d.CachePath = "" },
		"root cache":      func(d *Dependencies) { d.CachePath = "/" },
		"workspace cache": func(d *Dependencies) { d.CachePath = "/workspaces/deps" },
		"scratch cache":   func(d *Dependencies) { d.CachePath = "/tmp" },
		"runner cache":    func(d *Dependencies) { d.CachePath = "/runner/m2" },
		"settings cache":  func(d *Dependencies) { d.CachePath = "/etc/ai-sdlc/tests" },
		"identity cache":  func(d *Dependencies) { d.CachePath = "/var/run/ai-sdlc/pod" },
		"binary cache":    func(d *Dependencies) { d.CachePath = "/usr/bin" },
		"library cache":   func(d *Dependencies) { d.CachePath = "/usr/lib/jvm" },
		"usr cache":       func(d *Dependencies) { d.CachePath = "/usr" },
		"var cache":       func(d *Dependencies) { d.CachePath = "/var" },
		"traversal cache": func(d *Dependencies) { d.CachePath = "/deps/../tmp" },
		"trailing slash":  func(d *Dependencies) { d.CachePath = "/deps/" },
		"deep cache":      func(d *Dependencies) { d.CachePath = strings.Repeat("/d", 17) },
		"no key globs":    func(d *Dependencies) { d.KeyGlobs = nil },
		"many key globs":  func(d *Dependencies) { d.KeyGlobs = globs },
		"duplicate glob":  func(d *Dependencies) { d.KeyGlobs = []string{"pom.xml", "pom.xml"} },
		"traversal glob":  func(d *Dependencies) { d.KeyGlobs = []string{"../pom.xml"} },
		"git glob":        func(d *Dependencies) { d.KeyGlobs = []string{".git/config"} },
		"class glob":      func(d *Dependencies) { d.KeyGlobs = []string{"[a-z]/pom.xml"} },
		"empty glob":      func(d *Dependencies) { d.KeyGlobs = []string{""} },
	} {
		m := withDependencies()
		change(m.Verification.Dependencies)
		if m.Validate() == nil || m.Verification.Validate() == nil {
			t.Fatal("accepted", name)
		}
	}
	m := withDependencies()
	m.Verification.Dependencies.CachePath = "/opt/ai-sdlc/maven-cache"
	m.Verification.Dependencies.KeyGlobs = globs[:MaxDependencyKeyGlobs]
	if err := m.Validate(); err != nil {
		t.Fatal("refused the largest valid section", err)
	}
}

func TestDependencyFilesAreHarness(t *testing.T) {
	v := *withDependencies().Verification
	for _, file := range []string{"pom.xml", "rest-villains/pom.xml", ".mvn/wrapper/maven-wrapper.properties", "mvnw"} {
		if !v.MatchesDependencies(file) || !v.MatchesHarness(file) {
			t.Fatal("dependency file is not harness", file)
		}
	}
	for _, file := range []string{"rest-villains/src/main/java/A.java", "mvnw.cmd", "../pom.xml"} {
		if v.MatchesDependencies(file) || v.MatchesHarness(file) {
			t.Fatal("not a dependency file", file)
		}
	}
	if generic().Verification.MatchesDependencies("pom.xml") {
		t.Fatal("a manifest without dependencies has dependency files")
	}
}

// The key is a documented text format; tools/test_dependency_cache_install.py
// checks the same vector, so the operator's prefetch and the verifier name
// the same cache.
func TestDependencyKeyIsStableAndCoversItsInputs(t *testing.T) {
	v := *withDependencies().Verification
	files := []DependencyFile{
		{Path: "rest-villains/pom.xml", Data: []byte("<project>villains</project>\n")},
		{Path: "pom.xml", Data: []byte("<project>root</project>\n")},
		{Path: ".mvn/wrapper/maven-wrapper.properties", Data: []byte("distributionType=only-script\n")},
		{Path: "rest-villains/src/main/java/A.java", Data: []byte("class A {}\n")},
	}
	key, ok := v.DependencyKey(files)
	const want = "dc42c7e381a6295819ee1e442d42f621130901df35f6b4930b5598a5d28accea"
	if !ok || key != want {
		t.Fatal("dependency key changed:", key)
	}
	reordered := []DependencyFile{files[3], files[1], files[0], files[2]}
	if again, _ := v.DependencyKey(reordered); again != key {
		t.Fatal("key depends on file order")
	}
	source := append([]DependencyFile{}, files...)
	source[3].Data = []byte("class A { int x; }\n")
	if again, _ := v.DependencyKey(source); again != key {
		t.Fatal("a source file changed the key")
	}
	for name, change := range map[string]func(*Verification, []DependencyFile) []DependencyFile{
		"pom": func(_ *Verification, f []DependencyFile) []DependencyFile {
			f[0].Data = []byte("<project>v2</project>\n")
			return f
		},
		"new pom": func(_ *Verification, f []DependencyFile) []DependencyFile {
			return append(f, DependencyFile{"rest-heroes/pom.xml", nil})
		},
		"image": func(v *Verification, f []DependencyFile) []DependencyFile {
			v.Image = strings.Replace(v.Image, "b", "c", 1)
			return f
		},
		"globs": func(v *Verification, f []DependencyFile) []DependencyFile {
			v.Dependencies.KeyGlobs = []string{"**/pom.xml"}
			return f
		},
		"cache path": func(v *Verification, f []DependencyFile) []DependencyFile {
			v.Dependencies.CachePath = "/m2"
			return f
		},
	} {
		changed := *withDependencies().Verification
		deps := *changed.Dependencies
		changed.Dependencies = &deps
		if got, ok := changed.DependencyKey(change(&changed, append([]DependencyFile{}, files...))); !ok || got == key {
			t.Fatal("key ignores", name)
		}
	}
	if _, ok := v.DependencyKey(append(files, files[0])); ok {
		t.Fatal("duplicate path accepted")
	}
	if _, ok := v.DependencyKey([]DependencyFile{{Path: "../pom.xml"}}); ok {
		t.Fatal("unclean path accepted")
	}
	if _, ok := generic().Verification.DependencyKey(files); ok {
		t.Fatal("a key without a dependencies section")
	}
	if DependencyClaimName(key) != "deps-"+key || !ValidDependencyClaimName(DependencyClaimName(key)) ||
		DependencyClaimName("x") != "" || ValidDependencyClaimName("deps-x") {
		t.Fatal("claim name")
	}
}

// The Maven example is the reference for verification.dependencies: it must
// parse, mount its cache where its offline command reads it, and classify
// its dependency files as harness.
func TestMavenExampleManifestParses(t *testing.T) {
	raw, err := os.ReadFile("../../../../examples/quarkus-super-heroes-rest-villains.project.json")
	if err != nil {
		t.Fatal(err)
	}
	m, err := ParseManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	d := m.Verification.Dependencies
	if d == nil || !slices.Contains(m.Verification.Command, "-Dmaven.repo.local.tail="+d.CachePath) || !slices.Contains(m.Verification.Command, "-o") ||
		!m.Verification.MatchesHarness("rest-villains/pom.xml") || !m.Verification.MatchesHarness(".mvn/wrapper/maven-wrapper.properties") ||
		m.Publication.Allows("rest-villains/pom.xml") || !m.Verification.MatchesTest("rest-villains/src/test/java/A.java") {
		t.Fatalf("unexpected Maven example manifest: %+v", m)
	}
}
