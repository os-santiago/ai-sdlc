// Package cycle evaluates one bounded implementation attempt against policy
// and decides whether to retry, escalate or complete — the ADR-0003
// "bounded edit" / "independent validation" stage contract, as a pure
// function instead of a bash reconciliation loop.
//
// Ported from os-santiago/homedir-ai-sdlc (future-go/components/worker).
// PolicyFromContract is new: it sources MaximumAttempts from a repo's
// .ai-sdlc.yaml review.max_fix_iterations instead of a hardcoded constant —
// see docs/architecture/adr/0004-cloud-native-k8s-openshift-runtime.md.
//
// This component evaluates attempts; it does not invoke an engine. Engine
// invocation (sc-agent-cli, headless contract: --prompt-file --audit-log
// --summary-file --no-commit) stays a single integration point shared with
// the GHA runtime — see runtime/k8s/README.md "Non-goals".
package cycle

import (
	"errors"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/contract"
)

type Outcome string

const (
	Continue  Outcome = "continue"
	Completed Outcome = "completed"
	Failed    Outcome = "failed"
	Escalated Outcome = "needs-human"
)

type Attempt struct {
	Number             int      `json:"number"`
	ExitCode           int      `json:"exitCode"`
	TimedOut           bool     `json:"timedOut"`
	ChangedFiles       []string `json:"changedFiles"`
	ValidationPassed   bool     `json:"validationPassed"`
	ActionableFeedback []string `json:"actionableFeedback"`
	UnsafeOrAmbiguous  bool     `json:"unsafeOrAmbiguous"`
}

type Policy struct {
	MaximumAttempts     int `json:"maximumAttempts"`
	MaximumChangedFiles int `json:"maximumChangedFiles"`
}

type Decision struct {
	Outcome  Outcome `json:"outcome"`
	Reason   string  `json:"reason"`
	Reprompt bool    `json:"reprompt"`
}

// DefaultPolicy returns the standalone (contract-free) policy, used only
// when no .ai-sdlc.yaml is available (e.g. local unit tests, dry-run CLI).
func DefaultPolicy() Policy {
	return Policy{MaximumAttempts: 5, MaximumChangedFiles: 5}
}

// PolicyFromContract sources MaximumAttempts from review.max_fix_iterations
// (spec/ai-sdlc.schema.json default: 3 — the "bounded review→fix
// convergence" field, issue #28) and MaximumChangedFiles from
// review.max_changed_files (schema field closed in this port — see
// docs/architecture/adr/0004 "Schema gaps closed"). MaximumChangedFiles is
// 0 (unbounded) unless the contract sets an explicit ceiling, so adopting
// this policy changes no existing attempt's outcome until a repo opts in.
func PolicyFromContract(c contract.Contract) Policy {
	policy := DefaultPolicy()
	policy.MaximumAttempts = c.MaxFixIterationsOrDefault()
	policy.MaximumChangedFiles = c.MaxChangedFilesOrUnbounded()
	return policy
}

func Evaluate(attempt Attempt, policy Policy) (Decision, error) {
	// MaximumChangedFiles == 0 means unbounded (contract: review.
	// max_changed_files unset) — never treated as "zero files allowed", so
	// it is excluded from this positivity check. MaximumAttempts has no
	// unbounded mode: ADR-0003's bounded-delivery principle requires every
	// attempt loop to terminate.
	if attempt.Number < 1 || policy.MaximumAttempts < 1 || policy.MaximumChangedFiles < 0 {
		return Decision{}, errors.New("attempt number and maximum attempts must be positive; maximum changed files must not be negative")
	}
	if attempt.UnsafeOrAmbiguous {
		return Decision{Outcome: Escalated, Reason: "Implementation requires unsafe or business-sensitive judgment."}, nil
	}
	if policy.MaximumChangedFiles > 0 && len(attempt.ChangedFiles) > policy.MaximumChangedFiles {
		return Decision{Outcome: Escalated, Reason: "Change exceeds the configured atomic file limit."}, nil
	}
	if attempt.TimedOut {
		return terminalOrRetry(attempt, policy, "Agent execution timed out."), nil
	}
	if attempt.ExitCode != 0 {
		return terminalOrRetry(attempt, policy, "Agent execution failed."), nil
	}
	if len(attempt.ChangedFiles) == 0 {
		return terminalOrRetry(attempt, policy, "Agent completed without producing changes."), nil
	}
	if !attempt.ValidationPassed {
		return terminalOrRetry(attempt, policy, "Scoped validation failed."), nil
	}
	if len(attempt.ActionableFeedback) > 0 {
		return terminalOrRetry(attempt, policy, "Actionable review feedback remains."), nil
	}
	return Decision{Outcome: Completed, Reason: "Implementation and scoped validation succeeded."}, nil
}

func terminalOrRetry(attempt Attempt, policy Policy, reason string) Decision {
	if attempt.Number >= policy.MaximumAttempts {
		return Decision{Outcome: Escalated, Reason: reason + " Maximum attempts reached."}
	}
	return Decision{Outcome: Continue, Reason: reason, Reprompt: true}
}
