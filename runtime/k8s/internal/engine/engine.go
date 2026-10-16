// Package engine defines the worker component's one external integration
// point: invoking something that actually attempts an implementation.
//
// ADR-0004 Decision #2 deliberately dropped future-go's fifth component
// (a direct sc-agent-cli wrapper) so this runtime shares one headless
// engine contract with the GHA-native runtime
// (--prompt-file --audit-log --summary-file --no-commit) instead of
// duplicating it. This package is that integration point's boundary:
// components/worker calls Engine.Invoke and never knows whether the
// implementation underneath is a real subprocess or a test double.
//
// StubEngine and SCCEngine (scc.go) are the two implementations shipped
// here. SCCEngine shells out to sc-agent-cli with the headless contract
// and (via Checker) checks out the target repo with a GitHub App
// installation token first — see scc.go and internal/checkout. The one
// remaining prerequisite before AI_SDLC_ENGINE=scc does anything in a
// real cluster is a Node-capable worker image (currently
// distroless/static — see scc.go's SCCEngine doc comment and
// docs/architecture/adr/0005-runtime-k8s-event-bus-and-engine-invocation.md).
package engine

import "context"

// Request is what the worker component has available when it is ready to
// attempt one bounded implementation step: the fields of
// contracts/commands/v1/work-item-ready.schema.json plus the attempt
// number about to run.
type Request struct {
	WorkItemID         string
	Repository         string
	BaseRef            string
	IssueNumbers       []int
	AcceptanceCriteria []string
	AttemptNumber      int
}

// Result maps directly onto components/worker/internal/cycle.Attempt's
// fields (minus Number, which the caller already knows from Request).
// PullRequestURL is carried for forward-compatibility with a real engine
// that opens a PR as part of the same invocation; it is always nil from
// StubEngine and currently unused by any caller.
type Result struct {
	ExitCode           int
	TimedOut           bool
	ChangedFiles       []string
	ValidationPassed   bool
	ActionableFeedback []string
	UnsafeOrAmbiguous  bool
	PullRequestURL     *string
}

// Engine attempts one bounded implementation step and reports what
// happened. Implementations must be safe to call with a context that may
// be cancelled (the worker component bounds every attempt per
// ADR-0003) and must never block past that deadline.
type Engine interface {
	Invoke(ctx context.Context, request Request) (Result, error)
}

// StubEngine is a deterministic, no-subprocess, no-network implementation:
// every invocation "succeeds" with no changed files. It exists so the
// event-bus wiring (ADR-0005) is testable end-to-end — admission through
// release-manager — without scc or model credentials. Selected by default
// (AI_SDLC_ENGINE unset or "stub"); see components/worker/cmd/main.go.
type StubEngine struct{}

// Invoke always returns a successful, no-op result.
func (StubEngine) Invoke(_ context.Context, _ Request) (Result, error) {
	return Result{
		ExitCode:         0,
		ValidationPassed: true,
	}, nil
}
