package main

import (
	"context"
	"log"
	"net/http"
	"os"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/components/admission-controller/internal/admission"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/service"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

func main() {
	ctx := context.Background()
	deps := admission.Deps{}

	// Unset DATABASE_URL retains the isolated synchronous evaluator.
	// A configured but unavailable database fails startup, so Kubernetes
	// retries rather than declaring a stateless component ready.
	if url := os.Getenv("DATABASE_URL"); url != "" {
		db, err := store.Open(ctx, url)
		if err != nil {
			log.Fatalf("admission-controller: configured database unavailable; refusing stateless fallback: %v", err)
		} else {
			if err := db.Migrate(ctx); err != nil {
				log.Fatalf("admission-controller: migrate database: %v", err)
			}
			deps.DB = db
		}
	}

	err := service.Run(service.Metadata{
		Name:           "admission-controller",
		Responsibility: "normalize work-item intake and produce policy-backed admission decisions",
		Consumes:       []string{"normalized-event.v1 (work_item)"},
		Produces:       []string{"IssueAdmissionDecided.v1"},
	}, func(mux *http.ServeMux) { admission.Routes(mux, deps) })
	if err != nil {
		log.Fatal(err)
	}
}
