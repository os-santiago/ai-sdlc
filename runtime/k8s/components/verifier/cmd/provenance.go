package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/bootstrap"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/service"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/provenanceflow"
)

// A separate process prevents long generative review and its gateway readiness
// from consuming the short, independently authorized provenance claim window.
// No additional image is built: installation uses verifier args ["provenance"].
func runProvenance() error {
	if !bootstrap.ProvenanceEnabled() {
		return errors.New("explicit provenance capability required")
	}
	c, e := bootstrap.Open(context.Background(), "verifier")
	if e != nil {
		return e
	}
	defer c.DB.Close()
	mounts := provenanceflow.InstalledMounts(os.Getenv("AI_SDLC_PROVENANCE_VERIFIER_IMAGE"))
	mounts.Inner, mounts.Identity = c.PolicyPath, c.IdentityPath
	runtime := provenanceflow.Service{DB: c.DB, Mounts: mounts}
	stop := bootstrap.Periodic(5*time.Second, func(ctx context.Context) {
		state, e := runtime.PollOnce(ctx)
		if e != nil {
			log.Print("provenance reconciliation stopped safely")
		} else if state != "provenance-idle" {
			log.Print(state)
		}
	})
	defer stop()
	return service.RunWithReadiness(service.Metadata{Name: "provenance-verifier", Responsibility: "verify retained artifact provenance under fresh authority", Consumes: []string{"verify-built"}, Produces: []string{"ai-sdlc.saferus-provenance/v1"}}, runtime.Ready, func(mux *http.ServeMux) { mux.Handle("POST /internal/verify-built", runtime.Handler()) })
}
