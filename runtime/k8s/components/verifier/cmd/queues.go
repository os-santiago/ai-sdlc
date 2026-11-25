package main

import (
	"context"
	"log"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/metrics"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/reviewflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/verificationflow"
)

type queuePoll func(context.Context) (string, error)

// Each reconciler first retires its acknowledged resources. Publication handoff
// cannot prevent that recovery or monopolize another queue on the next cycle.
// At most one item per queue is processed; uncertain cleanup stops new work.
func reconcileQueues(ctx context.Context, review, verify queuePoll, handoff func(context.Context) (bool, error)) {
	if verify == nil {
		return
	}
	if review != nil {
		state, err := review(ctx)
		if err != nil {
			stage := reviewflow.DiagnosticStage(err)
			metrics.Stop("verifier", "review", stage)
			log.Print("independent review stopped safely: " + stage)
		} else if state != "" {
			log.Print("independent review: " + state)
		}
		if !reviewflow.VerificationMayProceed(state, err) {
			return
		}
	}
	state, err := verify(ctx)
	if err != nil {
		stage := verificationflow.DiagnosticStage(err)
		metrics.Stop("verifier", "verification", stage)
		log.Print("verification reconciliation stopped safely: " + stage)
		// A missing sandbox class stops only new claims; cleanup already
		// completed, so approved work is still handed off.
		if !verificationflow.SandboxUnavailable(err) {
			return
		}
	} else if state != "" {
		log.Print("verification reconciliation: " + state)
	}
	if handoff != nil {
		sent, err := handoff(ctx)
		if err != nil {
			metrics.Stop("verifier", "publication-handoff", metrics.Unclassified)
			log.Print("publication handoff stopped safely")
		} else if sent {
			log.Print("publication handoff dispatched")
		}
	}
}
