package main

import (
	"context"
	"log"
	"net/http"
	"os"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/components/release-manager/internal/gates"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/service"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

func main() {
	ctx := context.Background()
	deps := gates.Deps{}

	if url := os.Getenv("DATABASE_URL"); url != "" {
		db, err := store.Open(ctx, url)
		if err != nil {
			log.Printf("release-manager: database unavailable, running without persistence: %v", err)
		} else {
			if err := db.Migrate(ctx); err != nil {
				log.Fatalf("release-manager: migrate database: %v", err)
			}
			deps.DB = db
		}
	}

	err := service.Run(service.Metadata{
		Name:           "release-manager",
		Responsibility: "evaluate merge/release gates: required checks, review, issue coverage, validation evidence",
		Consumes:       []string{"ImplementationAttemptDecided.v1"},
		Produces:       []string{"ReleaseGateDecided.v1"},
	}, func(mux *http.ServeMux) { gates.Routes(mux, deps) })
	if err != nil {
		log.Fatal(err)
	}
}
