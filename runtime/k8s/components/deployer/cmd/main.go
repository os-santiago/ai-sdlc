// Command deployer accepts only authenticated component delivery. It exposes no
// operator stage-advance, artifact selection or arbitrary workload endpoint.
package main

import (
	"context"
	"errors"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/completionflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/deploymentflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/bootstrap"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/service"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/podtransport"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releaseflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasing"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"io"
	"log"
	"net/http"
	"os"
	"time"
)

func bounded(path string, limit int64) ([]byte, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, deploymentflow.ErrFlow
	}
	defer f.Close()
	raw, e := io.ReadAll(io.LimitReader(f, limit+1))
	if e != nil || len(raw) == 0 || int64(len(raw)) > limit {
		return nil, deploymentflow.ErrFlow
	}
	return raw, nil
}
func run() error {
	if !bootstrap.ReleaseEnabled() {
		return errors.New("explicit complete release capability required")
	}
	if os.Getenv("DATABASE_URL") == "" {
		return errors.New("dedicated deployer database credential required")
	}
	db, e := store.Open(context.Background(), os.Getenv("DATABASE_URL"))
	if e != nil {
		return errors.New("deployer database unavailable")
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if store.AssertComponent(ctx, db.Pool, "deployer") != nil || db.CheckMigrations(ctx) != nil {
		return errors.New("deployer durable dependency unavailable")
	}
	// Policy readiness does not cancel an in-flight original containment attempt.
	// Liveness is independent; no restart loop is used to advance workflow state.
	mounts := releaseflow.InstalledMounts(os.Getenv("AI_SDLC_PROVENANCE_VERIFIER_IMAGE"))
	runtime := deploymentflow.Service{DB: db, Mounts: mounts, Transport: func(target releasing.Target) (deploymentflow.Transport, error) {
		token, e := bounded("/var/run/secrets/kubernetes.io/serviceaccount/token", 8192)
		if e != nil {
			return nil, e
		}
		ca, e := bounded("/var/run/secrets/kubernetes.io/serviceaccount/ca.crt", 65536)
		if e != nil {
			return nil, e
		}
		client, e := podtransport.NewClient(podtransport.Config{APIServer: "https://kubernetes.default.svc:443", CA: ca, BearerToken: string(token)})
		if e != nil {
			return nil, e
		}
		adapter, e := podtransport.NewConsoleDeployment(client, target)
		if e != nil {
			return nil, e
		}
		return deploymentflow.ConsoleTransport{ConsoleDeployment: adapter}, nil
	}}
	stopCompletion := bootstrap.Periodic(5*time.Second, func(ctx context.Context) {
		if _, e := completionflow.DispatchDeployed(ctx, db, func() (releasing.Configuration, authority.Identity, error) { return mounts.Load("deployer") }); e != nil {
			log.Print("deployment completion dispatch stopped safely")
		}
	})
	defer stopCompletion()
	stop := bootstrap.Periodic(5*time.Second, func(ctx context.Context) {
		state, e := runtime.PollOnce(ctx)
		if e != nil {
			log.Print("deployment reconciliation stopped safely")
		} else if state != "deployment-idle" {
			log.Print(state)
		}
	})
	defer stop()
	return service.RunWithReadiness(service.Metadata{Name: "deployer", Responsibility: "apply the approved console image under original finite authority", Consumes: []string{"deploy-release"}, Produces: []string{"ai-sdlc.saferus-health/v1", "record-deployment"}}, runtime.Ready, func(mux *http.ServeMux) { mux.Handle("POST /internal/deploy-release", runtime.Handler()) })
}
func main() {
	if e := run(); e != nil {
		log.Fatal(e)
	}
}
