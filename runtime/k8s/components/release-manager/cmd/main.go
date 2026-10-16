package main

import (
	"log"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/components/release-manager/internal/gates"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/service"
)

func main() {
	err := service.Run(service.Metadata{
		Name:           "release-manager",
		Responsibility: "evaluate merge/release gates: required checks, review, issue coverage, validation evidence",
		Consumes:       []string{"ImplementationAttemptDecided.v1"},
		Produces:       []string{"ReleaseGateDecided.v1"},
	}, gates.Routes)
	if err != nil {
		log.Fatal(err)
	}
}
