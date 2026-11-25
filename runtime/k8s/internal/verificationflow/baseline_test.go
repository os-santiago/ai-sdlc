package verificationflow

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/candidate"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/secretscan"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

// fakeToken assembles a GitHub-token-shaped value at run time, so the source
// holds no literal a secret scanner would report.
func fakeToken() string {
	const alphabet = "aB3dE5fG7hJ9kL2mN4pQ6rS8tU0vW1xYzACDFHKMPRTVXZ"
	var b strings.Builder
	b.WriteString("gh" + "p_")
	for i := 0; i < 36; i++ {
		b.WriteByte(alphabet[(i*7+1)%len(alphabet)])
	}
	return b.String()
}

func baselineDelta(t *testing.T, added string) (workspacebundle.Bundle, candidate.Delta) {
	t.Helper()
	source := workspacebundle.Bundle{Version: workspacebundle.Version, SourceSHA: strings.Repeat("a", 40), Files: []workspacebundle.File{
		{Path: "main.go", Mode: 0644, Data: []byte("package main\n")}}}
	result := source
	result.Files = []workspacebundle.File{{Path: "main.go", Mode: 0644, Data: []byte("package main\n" + added)}}
	d, err := candidate.Build(source, result)
	if err != nil {
		t.Fatal(err)
	}
	return source, d
}

func TestCandidateBaselineRefusesAnAddedSecretWithoutLeakingIt(t *testing.T) {
	token := fakeToken()
	source, d := baselineDelta(t, "const token = \""+token+"\"\n")
	err := candidateBaseline(source, d)
	var refused DecisionError
	if !errors.As(err, &refused) || refused.Reason != SecretRefusal ||
		refused.Secret != (secretscan.Finding{Rule: secretscan.RuleGitHubToken, PathClass: secretscan.ClassCode}) {
		t.Fatalf("refusal = %#v", err)
	}
	if got := DiagnosticStage(err); got != "refused-candidate-secret" {
		t.Fatal(got)
	}
	if got := RefusalDetail(fmt.Errorf("wrapped: %w", err)); got != "rule=github-token class=code" {
		t.Fatal(got)
	}
	for _, rendered := range []string{err.Error(), fmt.Sprintf("%v %+v %#v", err, err, refused), DiagnosticStage(err), RefusalDetail(err)} {
		if strings.Contains(rendered, token) || strings.Contains(rendered, token[4:16]) || strings.Contains(rendered, "main.go") {
			t.Fatalf("refusal text %q leaks the secret or the path", rendered)
		}
	}
}

func TestCandidateBaselinePassesOrdinaryChanges(t *testing.T) {
	source, d := baselineDelta(t, "func main() {}\n")
	if err := candidateBaseline(source, d); err != nil {
		t.Fatal(err)
	}
}

func TestRefusalDetailIsBoundedAndOnlyForSecrets(t *testing.T) {
	for _, err := range []error{
		nil,
		errors.New("rule=github-token class=code"),
		ControllerError{"recipe"},
		DecisionError{Reason: "regression"},
		DecisionError{Reason: "regression", Secret: secretscan.Finding{Rule: secretscan.RuleGitHubToken, PathClass: secretscan.ClassCode}},
		DecisionError{Reason: SecretRefusal, Secret: secretscan.Finding{Rule: "sensitive-fixture", PathClass: secretscan.ClassCode}},
		DecisionError{Reason: SecretRefusal, Secret: secretscan.Finding{Rule: secretscan.RuleGitHubToken, PathClass: "main.go"}},
	} {
		if got := RefusalDetail(err); got != "" {
			t.Fatalf("RefusalDetail(%#v) = %q", err, got)
		}
	}
	// A secret refusal without a valid finding still names its stage.
	if got := DiagnosticStage(DecisionError{Reason: SecretRefusal}); got != "refused-candidate-secret" {
		t.Fatal(got)
	}
	if got := DiagnosticStage(DecisionError{Reason: "candidate-analysis"}); got != "refused-candidate-analysis" {
		t.Fatal(got)
	}
}
