package verificationflow

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/repositorytests"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/verification"
)

func uidsFor(keys ...string) map[string]string {
	m := map[string]string{}
	for _, k := range keys {
		m[k] = "11111111-1111-4111-8111-111111111111"
	}
	return m
}

func TestBehaviorProofVersionsMatchTheirRecipe(t *testing.T) {
	reference := verification.Command{Recipe: verification.LocaleRecipe, SourceSHA: strings.Repeat("a", 40)}
	tests := verification.Command{Recipe: repositorytests.Recipe, SourceSHA: strings.Repeat("a", 40)}
	passed, _ := json.Marshal(repositorytests.Decision{Passed: true, Summary: repositorytests.Summary{ChangedTestFiles: []string{"a_test.go"}}})
	refused, _ := json.Marshal(repositorytests.Decision{Passed: false, Reason: "no-failing-test"})
	v1 := BehaviorProof{Version: BehaviorProofV1, Command: reference, BehaviorVerified: true, ResourceUIDs: uidsFor("config", "baseline", "candidate", "reference", "tests")}
	v2 := BehaviorProof{Version: BehaviorProofV2, Command: tests, BehaviorVerified: true, Decision: passed, CrossResultSHA256: strings.Repeat("c", 64), ResourceUIDs: uidsFor("config", "baseline", "tests", "candidate")}
	if !ValidBehaviorProof(v1, reference) || !ValidBehaviorProof(v2, tests) {
		t.Fatal("valid proofs refused")
	}
	cases := map[string]func() (BehaviorProof, verification.Command){
		"v1 for repository tests": func() (BehaviorProof, verification.Command) { p := v1; p.Command = tests; return p, tests },
		"v2 for a reference":      func() (BehaviorProof, verification.Command) { p := v2; p.Command = reference; return p, reference },
		"v2 with a refusal":       func() (BehaviorProof, verification.Command) { p := v2; p.Decision = refused; return p, tests },
		"v2 with a reference UID": func() (BehaviorProof, verification.Command) {
			p := v2
			p.ResourceUIDs = uidsFor("config", "baseline", "tests", "candidate", "reference")
			return p, tests
		},
		"v2 missing a run": func() (BehaviorProof, verification.Command) {
			p := v2
			p.ResourceUIDs = uidsFor("config", "baseline", "candidate")
			return p, tests
		},
		"v1 with a decision":     func() (BehaviorProof, verification.Command) { p := v1; p.Decision = passed; return p, reference },
		"publication authorized": func() (BehaviorProof, verification.Command) { p := v2; p.PublicationAuthorized = true; return p, tests },
		"another command": func() (BehaviorProof, verification.Command) {
			other := tests
			other.SourceSHA = strings.Repeat("b", 40)
			return v2, other
		},
		"unknown version": func() (BehaviorProof, verification.Command) {
			p := v2
			p.Version = "ai-sdlc.behavior-proof/v3"
			return p, tests
		},
		"malformed decision": func() (BehaviorProof, verification.Command) {
			p := v2
			p.Decision = json.RawMessage(`{"passed":true,"extra":1}`)
			return p, tests
		},
	}
	for name, build := range cases {
		p, c := build()
		if ValidBehaviorProof(p, c) {
			t.Fatal("accepted", name)
		}
	}
}
