package main

import (
	"context"
	"log"
	"net/http"
	"os"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/components/worker/internal/cycle"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/engine"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/service"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

func main() {
	ctx := context.Background()
	deps := cycle.Deps{Engine: selectEngine()}

	if url := os.Getenv("DATABASE_URL"); url != "" {
		db, err := store.Open(ctx, url)
		if err != nil {
			log.Printf("worker: database unavailable, running without persistence (attempt dispatch disabled): %v", err)
		} else {
			if err := db.Migrate(ctx); err != nil {
				log.Fatalf("worker: migrate database: %v", err)
			}
			deps.DB = db
		}
	}

	err := service.Run(service.Metadata{
		Name:           "worker",
		Responsibility: "evaluate one bounded implementation attempt and decide retry/escalate/complete",
		Consumes:       []string{"WorkItemReady.v1"},
		Produces:       []string{"ImplementationAttemptDecided.v1"},
	}, func(mux *http.ServeMux) { cycle.Routes(mux, deps) })
	if err != nil {
		log.Fatal(err)
	}
}

// selectEngine picks the Engine implementation by AI_SDLC_ENGINE ("stub",
// the default, or "scc" — reserved, not yet implemented; see
// docs/architecture/adr/0005-runtime-k8s-event-bus-and-engine-invocation.md
// "Explicitly still out of scope"). An unrecognized value falls back to
// the stub rather than failing startup — this component degrades, it
// never crashes a running pod over a misconfigured env var.
func selectEngine() engine.Engine {
	switch os.Getenv("AI_SDLC_ENGINE") {
	case "scc":
		log.Println("worker: AI_SDLC_ENGINE=scc is reserved for the real sc-agent-cli subprocess engine (not implemented yet); falling back to the stub engine")
		return engine.StubEngine{}
	default:
		return engine.StubEngine{}
	}
}
