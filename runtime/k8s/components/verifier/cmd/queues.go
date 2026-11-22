package main

import (
	"context"
	"log"

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
			log.Print("independent review stopped safely: " + reviewflow.DiagnosticStage(err))
		} else if state != "" {
			log.Print("independent review: " + state)
		}
		if !reviewflow.VerificationMayProceed(state, err) {
			return
		}
	}
	state, err := verify(ctx)
	if err != nil {
		log.Print("verification reconciliation stopped safely: " + verificationflow.DiagnosticStage(err))
		return
	}
	if state != "" {
		log.Print("verification reconciliation: " + state)
	}
	if handoff != nil {
		sent, err := handoff(ctx)
		if err != nil {
			log.Print("publication handoff stopped safely")
		} else if sent {
			log.Print("publication handoff dispatched")
		}
	}
}
