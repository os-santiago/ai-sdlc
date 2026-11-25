package verificationflow

import (
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/candidate"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/secretscan"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

// candidateBaseline applies the always-on candidate baseline to a decoded
// delta that applies to source: it refuses a candidate that adds a
// high-confidence credential (secretscan). It returns nil or a DecisionError
// whose only candidate-derived content is the finding's rule and path class.
func candidateBaseline(source workspacebundle.Bundle, d candidate.Delta) error {
	if finding, found := secretscan.Scan(source, d); found {
		return DecisionError{Reason: SecretRefusal, Secret: finding}
	}
	return nil
}
