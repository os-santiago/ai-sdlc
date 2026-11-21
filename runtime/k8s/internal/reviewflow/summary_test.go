package reviewflow

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/repositorytests"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/verificationflow"
)

func TestReviewerSeesTheRepositoryTestSummary(t *testing.T) {
	d, _ := json.Marshal(repositorytests.Decision{Passed: true, Summary: repositorytests.Summary{CrossExitCode: 1, CandidateRuns: 2, Weak: true, FailToPass: 1, ChangedTestFiles: []string{"textutil/textutil_test.go"}}})
	text := verificationSummary(verificationflow.BehaviorProof{Version: verificationflow.BehaviorProofV2, Decision: d})
	if !strings.Contains(text, "textutil/textutil_test.go") || !strings.Contains(text, "weak result") {
		t.Fatal(text)
	}
	if verificationSummary(verificationflow.BehaviorProof{Version: verificationflow.BehaviorProofV1}) != "" {
		t.Fatal("v1 proofs add nothing")
	}
}
