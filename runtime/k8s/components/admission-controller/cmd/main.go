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

	// DATABASE_URL is optional: unset (or unreachable) means this
	// component degrades to ADR-0004's original synchronous-only
	// behavior — it never crashes a running pod for a missing dependency
	// (internal/contract.LoadFromEnv's "degrade, never crash" posture,
	// applied here to persistence too).
	if url := os.Getenv("DATABASE_URL"); url != "" {
		db, err := store.Open(ctx, url)
		if err != nil {
			log.Printf("admission-controller: database unavailable, running without persistence: %v", err)
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
