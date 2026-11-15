package main

import (
	"context"
	"log"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/bootstrap"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/service"
)

func main() {
	component, err := bootstrap.Open(context.Background(), "orchestrator")
	if err != nil {
		log.Fatal(err)
	}
	defer component.DB.Close()
	// Only observation endpoints are installed during this integration stage.
	// Legacy unsigned routes remain available to isolated Go fixtures only.
	err = service.RunWithReadiness(service.Metadata{
		Name:           "orchestrator",
		Responsibility: "maintain bounded plans and reconcile authenticated component facts",
		Consumes:       []string{"IssueAdmissionDecided.v1", "ImplementationAttemptDecided.v1"},
		Produces:       []string{"WorkItemReady.v1"},
	}, component.Ready)
	if err != nil {
		log.Fatal(err)
	}
}
