package project

import (
	"encoding/json"
	"strings"
	"testing"
)

func withTekton() Manifest {
	m := generic()
	m.Merge.Required = append(m.Merge.Required, Check{Context: "tekton-ci/maven", AppID: 4242})
	m.Tekton = &Tekton{
		Modules: map[string]TektonModule{"rest-villains": {Services: []string{"postgres"},
			MavenArgs: []string{"-Dtest=!UIResourceTests,!*IT", "-Dit.test=*IT,!UIResourceIT"}}},
		NpmDirectories: []string{"ui-super-heroes/src/main/webui"},
		Pre:            []TektonInvocation{{Pipeline: TektonMavenBuildTest, Target: "rest-villains"}},
		Tools:          []TektonTool{{Name: "build_test", Pipeline: TektonMavenBuildTest}, {Name: "npm_lock_update", Pipeline: TektonNpmLockUpdate}},
		Post:           []TektonInvocation{{Pipeline: TektonMavenBuildTest, Target: "rest-villains", Check: "tekton-ci/maven"}},
	}
	return m
}

func TestTektonSectionRoundTripsAndChangesOnlyItsDigest(t *testing.T) {
	m := withTekton()
	raw, _ := json.Marshal(m)
	parsed, err := ParseManifest(raw)
	if err != nil || parsed.Digest() != m.Digest() || parsed.Tekton == nil || len(parsed.Tekton.Tools) != 2 {
		t.Fatal("round trip", err)
	}
	if m.Digest() == generic().Digest() {
		t.Fatal("the section is not bound into the digest")
	}
	if strings.Contains(string(generic().Canonical()), "tekton") {
		t.Fatal("absent tekton section is encoded")
	}
}

func TestInvalidTektonSectionsAreRefused(t *testing.T) {
	for name, change := range map[string]func(*Tekton){
		"empty":              func(s *Tekton) { s.Pre, s.Tools, s.Post = nil, nil, nil },
		"unknown pipeline":   func(s *Tekton) { s.Pre[0].Pipeline = "shell" },
		"undeclared module":  func(s *Tekton) { s.Pre[0].Target = "rest-heroes" },
		"module for npm":     func(s *Tekton) { s.Pre = []TektonInvocation{{Pipeline: TektonNpmLockUpdate, Target: "rest-villains"}} },
		"pre check":          func(s *Tekton) { s.Pre[0].Check = "tekton-ci/maven" },
		"post unknown check": func(s *Tekton) { s.Post[0].Check = "other" },
		"post no check":      func(s *Tekton) { s.Post[0].Check = "" },
		"post dependencies":  func(s *Tekton) { s.Post[0].Pipeline = TektonMavenDependencies },
		"tool dependencies":  func(s *Tekton) { s.Tools[0].Pipeline = TektonMavenDependencies },
		"tool name":          func(s *Tekton) { s.Tools[0].Name = "Build-Test" },
		"duplicate tool":     func(s *Tekton) { s.Tools[1].Name = "build_test" },
		"npm tool no dirs":   func(s *Tekton) { s.NpmDirectories = nil },
		"service":            func(s *Tekton) { s.Modules["rest-villains"] = TektonModule{Services: []string{"redis"}} },
		"none and service":   func(s *Tekton) { s.Modules["rest-villains"] = TektonModule{Services: []string{"none", "postgres"}} },
		"no services":        func(s *Tekton) { s.Modules["rest-villains"] = TektonModule{} },
		"offline switch": func(s *Tekton) {
			s.Modules["rest-villains"] = TektonModule{Services: []string{"none"}, MavenArgs: []string{"-o"}}
		},
		"settings switch": func(s *Tekton) {
			s.Modules["rest-villains"] = TektonModule{Services: []string{"none"}, MavenArgs: []string{"-s"}}
		},
		"shell in arg": func(s *Tekton) {
			s.Modules["rest-villains"] = TektonModule{Services: []string{"none"}, MavenArgs: []string{"-Dx=$(id)"}}
		},
		"module traversal":    func(s *Tekton) { s.Modules["../x"] = TektonModule{Services: []string{"none"}} },
		"npm traversal":       func(s *Tekton) { s.NpmDirectories = []string{"a/../b"} },
		"npm root":            func(s *Tekton) { s.NpmDirectories = []string{"."} },
		"duplicate directory": func(s *Tekton) { s.NpmDirectories = []string{"a", "a"} },
		"too many tools": func(s *Tekton) {
			for i := 0; i < MaxTektonTools; i++ {
				s.Tools = append(s.Tools, TektonTool{Name: "t" + string(rune('a'+i)), Pipeline: TektonMavenBuildTest})
			}
		},
	} {
		m := withTekton()
		change(m.Tekton)
		raw, _ := json.Marshal(m)
		if _, err := ParseManifest(raw); err == nil {
			t.Fatal("accepted:", name)
		}
	}
}
