package main

import (
	"context"
	"log"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/bootstrap"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/service"
)

func main() {
	component, err := bootstrap.Open(context.Background(), "admission-controller")
	if err != nil {
		log.Fatal(err)
	}
	defer component.DB.Close()
	// Only observation endpoints are installed during this integration stage.
	// Legacy unsigned routes remain available to isolated Go fixtures only.
	err = service.RunWithReadiness(service.Metadata{
		Name:           "admission-controller",
		Responsibility: "normalize issue intake and produce policy-backed admission decisions",
		Consumes:       []string{"IssueObserved.v1"},
		Produces:       []string{"IssueAdmissionDecided.v1"},
	}, component.Ready)
	if err != nil {
		log.Fatal(err)
	}
}
