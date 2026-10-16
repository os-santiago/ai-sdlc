package main

import (
	"log"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/components/orchestrator/internal/planning"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/service"
)

func main() {
	err := service.Run(service.Metadata{
		Name:           "orchestrator",
		Responsibility: "turn an admitted work item into a bounded, dependency-ordered plan",
		Consumes:       []string{"IssueAdmissionDecided.v1"},
		Produces:       []string{"WorkItemReady.v1"},
	}, planning.Routes)
	if err != nil {
		log.Fatal(err)
	}
}
