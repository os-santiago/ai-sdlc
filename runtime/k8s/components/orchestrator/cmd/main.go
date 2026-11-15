package main

import (
	"context"
	"log"
	"os"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/components/orchestrator/internal/planning"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/intake"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/bootstrap"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/service"
)

func main() {
	component, err := bootstrap.Open(context.Background(), "orchestrator")
	if err != nil {
		log.Fatal(err)
	}
	defer component.DB.Close()
	var routes []service.RouteRegistrar
	if os.Getenv("AI_SDLC_RUNTIME_MODE") == "shadow" {
		policy, _, err := component.Authority()
		if err != nil {
			log.Fatal(err)
		}
		config, err := intake.LoadConfig(os.Getenv("AI_SDLC_INTAKE_CONFIG_PATH"), policy.Repository)
		if err != nil {
			log.Fatal(err)
		}
		routes = append(routes, planning.SignedRoutes(component, config))
	}
	// Legacy unsigned routes remain available to isolated Go fixtures only.
	err = service.RunWithReadiness(service.Metadata{
		Name:           "orchestrator",
		Responsibility: "maintain bounded plans and reconcile authenticated component facts",
		Consumes:       []string{"IssueAdmissionDecided.v1", "ImplementationAttemptDecided.v1"},
		Produces:       []string{"durable shadow plans"},
	}, component.Ready, routes...)
	if err != nil {
		log.Fatal(err)
	}
}
