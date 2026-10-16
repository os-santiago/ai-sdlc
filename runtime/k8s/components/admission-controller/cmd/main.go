package main

import (
	"log"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/components/admission-controller/internal/admission"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/service"
)

func main() {
	err := service.Run(service.Metadata{
		Name:           "admission-controller",
		Responsibility: "normalize work-item intake and produce policy-backed admission decisions",
		Consumes:       []string{"normalized-event.v1 (work_item)"},
		Produces:       []string{"IssueAdmissionDecided.v1"},
	}, admission.Routes)
	if err != nil {
		log.Fatal(err)
	}
}
