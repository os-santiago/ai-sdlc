package main

import (
	"context"
	"log"
	"net/http"
	"os"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/components/orchestrator/internal/planning"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/service"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

func main() {
	ctx := context.Background()
	deps := planning.Deps{}

	if url := os.Getenv("DATABASE_URL"); url != "" {
		db, err := store.Open(ctx, url)
		if err != nil {
			log.Printf("orchestrator: database unavailable, running without persistence (event ingress disabled): %v", err)
		} else {
			if err := db.Migrate(ctx); err != nil {
				log.Fatalf("orchestrator: migrate database: %v", err)
			}
			deps.DB = db
		}
	}

	err := service.Run(service.Metadata{
		Name:           "orchestrator",
		Responsibility: "turn an admitted work item into a bounded, dependency-ordered plan",
		Consumes:       []string{"IssueAdmissionDecided.v1", "ImplementationAttemptDecided.v1"},
		Produces:       []string{"WorkItemReady.v1"},
	}, func(mux *http.ServeMux) { planning.Routes(mux, deps) })
	if err != nil {
		log.Fatal(err)
	}
}
