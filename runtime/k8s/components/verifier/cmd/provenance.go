package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/bootstrap"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/metrics"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/service"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/provenanceflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releaseflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasing"
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
	releaseMounts := releaseflow.InstalledMounts(os.Getenv("AI_SDLC_PROVENANCE_VERIFIER_IMAGE"))
	releaseMounts.Inner, releaseMounts.Identity = c.PolicyPath, c.IdentityPath
	stop := bootstrap.Periodic(5*time.Second, func(ctx context.Context) {
		state, e := runtime.PollOnce(ctx)
		if e != nil {
			metrics.Stop("provenance-verifier", "provenance", metrics.Unclassified)
			log.Print("provenance reconciliation stopped safely")
		} else if state != "provenance-idle" {
			log.Print(state)
		}
		if bootstrap.ReleaseEnabled() {
			_, err := releaseflow.DispatchVerified(ctx, c.DB, func() (releasing.Configuration, authority.Identity, error) {
				if _, e := runtime.Load(); e != nil {
					return releasing.Configuration{}, authority.Identity{}, e
				}
				return releaseMounts.Load("verifier")
			})
			if err != nil {
				metrics.Stop("provenance-verifier", "promotion-dispatch", metrics.Unclassified)
				log.Print("verified-artifact promotion dispatch stopped safely")
			}
		}
	})
	defer stop()
	return service.RunWithReadiness(service.Metadata{Name: "provenance-verifier", Responsibility: "verify retained artifact provenance under fresh authority", Consumes: []string{"verify-built"}, Produces: []string{"ai-sdlc.saferus-provenance/v1"}}, runtime.Ready, func(mux *http.ServeMux) { mux.Handle("POST /internal/verify-built", runtime.Handler()) })
}
