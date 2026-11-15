package main

import (
	"context"
	"log"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/bootstrap"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/service"
)

func main() {
	component, err := bootstrap.Open(context.Background(), "worker")
	if err != nil {
		log.Fatal(err)
	}
	defer component.DB.Close()
	// Only observation endpoints are installed during this integration stage.
	// Legacy unsigned routes remain available to isolated Go fixtures only.
	err = service.RunWithReadiness(service.Metadata{
		Name:           "worker",
		Responsibility: "coordinate isolated implementation attempts",
		Consumes:       []string{"WorkItemReady.v1"},
		Produces:       []string{"ImplementationAttemptDecided.v1"},
	}, component.Ready)
	if err != nil {
		log.Fatal(err)
	}
}
