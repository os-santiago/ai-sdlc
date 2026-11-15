package main

import (
	"context"
	"log"
	"os"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/components/admission-controller/internal/admission"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/contract"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/intake"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/bootstrap"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/service"
)

func main() {
	component, err := bootstrap.Open(context.Background(), "admission-controller")
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
		trusted, err := contract.LoadShadow(os.Getenv("AI_SDLC_CONTRACT_PATH"), config.TriggerLabel)
		if err != nil {
			log.Fatal(err)
		}
		routes = append(routes, admission.SignedRoutes(component, config, trusted))
	}
	err = service.RunWithReadiness(service.Metadata{
		Name:           "admission-controller",
		Responsibility: "normalize issue intake and produce policy-backed admission decisions",
		Consumes:       []string{"IssueObserved.v1"},
		Produces:       []string{"IssueAdmissionDecided.v1"},
	}, component.Ready, routes...)
	if err != nil {
		log.Fatal(err)
	}
}
