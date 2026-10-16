package main

import (
	"context"
	"log"
	"net/http"
	"os"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/components/worker/internal/cycle"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/contract"
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
// the default, or "scc" — the real sc-agent-cli subprocess engine,
// internal/engine.SCCEngine). An unrecognized value, or "scc" without its
// required configuration, falls back to the stub rather than failing
// startup — this component degrades, it never crashes a running pod over
// a misconfigured env var (ADR-0005).
func selectEngine() engine.Engine {
	switch os.Getenv("AI_SDLC_ENGINE") {
	case "scc":
		return selectSCCEngine()
	default:
		return engine.StubEngine{}
	}
}

// selectSCCEngine builds internal/engine.SCCEngine from env vars. Two are
// mandatory and have no safe default:
//   - MODEL_API_KEY: the model provider credential (e.g. NVIDIA_API_KEY in
//     CLAUDE.md's VPS deployment — rendered into ~/.sc-agent/config.json,
//     never passed as a CLI flag; see root CLAUDE.md "Critical Lessons").
//     Source this from a Kubernetes Secret created out-of-band
//     (`kubectl create secret` or an External Secret), never a committed
//     manifest — see deploy/kustomize/base/README.md.
//   - AI_SDLC_WORKSPACE_DIR: must already be a checkout of the target
//     repo at the work item's BaseRef. Nothing in this runtime clones a
//     repo yet (see internal/engine.SCCEngine's doc comment, prerequisite
//     #2) — set this once that step exists.
//
// Missing either means AI_SDLC_ENGINE=scc degrades to StubEngine, logged
// once at startup so the gap is visible without crash-looping the pod.
func selectSCCEngine() engine.Engine {
	apiKey := os.Getenv("MODEL_API_KEY")
	workDir := os.Getenv("AI_SDLC_WORKSPACE_DIR")
	if apiKey == "" || workDir == "" {
		log.Printf("worker: AI_SDLC_ENGINE=scc requires MODEL_API_KEY and AI_SDLC_WORKSPACE_DIR (have: MODEL_API_KEY=%v, AI_SDLC_WORKSPACE_DIR=%v); falling back to the stub engine until both are configured",
			apiKey != "", workDir != "")
		return engine.StubEngine{}
	}
	c := contract.LoadFromEnv()
	return engine.SCCEngine{
		Binary:          valueOrDefault("SCC_BINARY", "scc"),
		WorkDir:         workDir,
		ProviderBaseURL: valueOrDefault("AI_SDLC_PROVIDER_BASE_URL", "https://integrate.api.nvidia.com/v1"),
		APIKey:          apiKey,
		Model:           valueOrDefault("AI_SDLC_MODEL", "nvidia/nemotron-3-ultra-550b-a55b"),
		MaxSteps:        c.Engine.MaxSteps,
		MaxSeconds:      c.Engine.MaxSeconds,
	}
}

func valueOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
