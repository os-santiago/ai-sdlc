package main

import (
	"context"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/delivery"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/productionflow"
	"log"
	"net/http"
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
	if bootstrap.IntakeEnabled() {
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
	ready := component.Ready
	if bootstrap.ReleaseEnabled() {
		load := func() (authority.Policy, authority.Identity, error) {
			p, k, e := component.Authority()
			if e != nil {
				return authority.Policy{}, authority.Identity{}, e
			}
			return productionflow.LoadOuter(p, k, "/etc/ai-sdlc/outer-policy/policy.json", "orchestrator")
		}
		routes = append(routes, func(mux *http.ServeMux) {
			inbox := delivery.Inbox{DB: component.DB, Target: "orchestrator"}
			mux.Handle("POST /internal/record-production", inbox.Handler(load, "record-production", "/internal/record-production", productionflow.InboxEffect(func() (authority.Policy, error) { p, _, e := load(); return p, e })))
		})
		ready = func(ctx context.Context) error {
			if _, _, e := load(); e != nil {
				return e
			}
			return component.Ready(ctx)
		}
	}
	// Legacy unsigned routes remain available to isolated Go fixtures only.
	err = service.RunWithReadiness(service.Metadata{
		Name:           "orchestrator",
		Responsibility: "maintain bounded plans and reconcile authenticated component facts",
		Consumes:       []string{"IssueAdmissionDecided.v1", "ImplementationAttemptDecided.v1"},
		Produces:       []string{"durable shadow plans"},
	}, ready, routes...)
	if err != nil {
		log.Fatal(err)
	}
}
