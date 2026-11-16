// Command build-controller accepts only signed inbox delivery. Its periodic
// reconciler owns execution and archived containment; there is no advance API.
package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/delivery"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/bootstrap"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/service"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/podtransport"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/provenance"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/provenanceflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

func bounded(path string, limit int64) ([]byte, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, buildflow.ErrBuildFlow
	}
	defer f.Close()
	raw, e := io.ReadAll(io.LimitReader(f, limit+1))
	if e != nil || int64(len(raw)) > limit {
		return nil, buildflow.ErrBuildFlow
	}
	return raw, nil
}

func run() error {
	option := os.Getenv("AI_SDLC_BUILD")
	if option != "" && option != "false" && option != "true" {
		return errors.New("invalid build capability")
	}
	if bootstrap.ValidateMode() != nil || (option == "true" && !bootstrap.MergeEnabled()) {
		return errors.New("build requires verified merge capability")
	}
	if os.Getenv("DATABASE_URL") == "" {
		return errors.New("dedicated build database credential required")
	}
	db, e := store.Open(context.Background(), os.Getenv("DATABASE_URL"))
	if e != nil {
		return errors.New("build database unavailable")
	}
	defer db.Close()
	dependencies := func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if store.AssertComponent(ctx, db.Pool, "build-controller") != nil {
			return buildflow.ErrBuildFlow
		}
		return db.CheckMigrations(ctx)
	}
	if e = dependencies(context.Background()); e != nil {
		return e
	}
	// Start containment from archived authority even if current policy is missing
	// or frozen. Only new work and inbox delivery use the current-policy loader.
	load := func() (buildflow.Installed, error) {
		if option != "true" {
			return buildflow.Installed{}, buildflow.ErrBuildFlow
		}
		return buildflow.LoadCurrent("/etc/ai-sdlc/build/config.json", "/etc/ai-sdlc/authority/policy.json", "/etc/ai-sdlc/outer-policy/policy.json")
	}
	identity := func() (authority.Policy, authority.Identity, error) {
		i, e := load()
		if e != nil {
			return authority.Policy{}, authority.Identity{}, e
		}
		raw, e := bounded("/etc/ai-sdlc/identity/identity.json", 16384)
		var key authority.Identity
		if e != nil || authority.DecodeStrict(raw, &key) != nil || key.Role != "build-controller" || key.Validate(i.Outer) != nil {
			return authority.Policy{}, key, buildflow.ErrBuildFlow
		}
		return i.Outer, key, nil
	}
	client := func() (*podtransport.Client, error) {
		token, e := bounded("/var/run/secrets/kubernetes.io/serviceaccount/token", 8192)
		if e != nil {
			return nil, e
		}
		ca, e := bounded("/var/run/secrets/kubernetes.io/serviceaccount/ca.crt", 65536)
		if e != nil {
			return nil, e
		}
		return podtransport.NewClient(podtransport.Config{APIServer: "https://kubernetes.default.svc:443", CA: ca, BearerToken: string(token)})
	}
	runtime := buildflow.Runtime{DB: db, Load: load, Client: client}
	provenanceMounts := provenanceflow.InstalledMounts(os.Getenv("AI_SDLC_PROVENANCE_VERIFIER_IMAGE"))
	if bootstrap.ProvenanceEnabled() {
		runtime.AfterRecovery = func(ctx context.Context) (bool, error) {
			return provenanceflow.DispatchBuilt(ctx, db, func() (provenance.Configuration, authority.Identity, error) {
				return provenanceMounts.Load("build-controller")
			})
		}
	}
	stop := bootstrap.Periodic(5*time.Second, func(ctx context.Context) {
		state, e := runtime.PollOnce(ctx)
		if e != nil {
			log.Print("build reconciliation stopped safely")
		} else if state != "build-idle" && state != "build-other-replica" {
			log.Print(state)
		}

	})
	defer stop()
	ready := func(ctx context.Context) error {
		if e := dependencies(ctx); e != nil {
			return e
		}
		if bootstrap.ProvenanceEnabled() {
			if _, _, e := provenanceMounts.Load("build-controller"); e != nil {
				return e
			}
		}
		if option == "true" {
			_, _, e := identity()
			return e
		}
		return nil
	}
	journal := store.NewBuildJournal(db, func() (building.Configuration, error) { i, e := load(); return i.Configuration(), e })
	return service.RunWithReadiness(service.Metadata{Name: "build-controller", Responsibility: "execute signed merged-source builds and contain original workloads", Consumes: []string{"build-merged"}, Produces: []string{"ai-sdlc.build-result/v1"}}, ready, func(mux *http.ServeMux) {
		inbox := delivery.Inbox{DB: db, Target: "build-controller"}
		mux.Handle("POST /internal/build-merged", inbox.Handler(identity, "build-merged", "/internal/build-merged", buildflow.InboxEffect(journal)))
	})
}

func main() {
	if e := run(); e != nil {
		log.Fatal(e)
	}
}
