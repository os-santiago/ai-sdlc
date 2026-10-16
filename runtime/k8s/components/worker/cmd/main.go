package main

import (
	"log"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/components/worker/internal/cycle"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/service"
)

func main() {
	err := service.Run(service.Metadata{
		Name:           "worker",
		Responsibility: "evaluate one bounded implementation attempt and decide retry/escalate/complete",
		Consumes:       []string{"WorkItemReady.v1"},
		Produces:       []string{"ImplementationAttemptDecided.v1"},
	}, cycle.Routes)
	if err != nil {
		log.Fatal(err)
	}
}
