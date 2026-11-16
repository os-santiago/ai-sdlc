package main

import (
	"context"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/delivery"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/execution"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/forge"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/intake"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/mergeflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/mergegate"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/mergewriter"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/bootstrap"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/service"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/publication"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/publicationflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/reviewflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	c, e := bootstrap.Open(context.Background(), "release-manager")
	if e != nil {
		log.Fatal(e)
	}
	defer c.DB.Close()
	metadata := service.Metadata{Name: "release-manager", Responsibility: "publish independently verified proposals under explicit authority", Consumes: []string{"publish-candidate"}, Produces: []string{"ai-sdlc.publication-proof/v1"}}
	if !bootstrap.PublicationEnabled() {
		if e = service.RunWithReadiness(metadata, c.Ready); e != nil {
			log.Fatal(e)
		}
		return
	}
	policy := func() (authority.Policy, intake.Config, string, error) {
		p, _, e := c.Authority()
		if e != nil || p.Repository != "os-santiago/homedir" || !bootstrap.PublicationEnabled() {
			return p, intake.Config{}, "", publication.ErrPublication
		}
		_, profile, e := execution.LoadRuntime(os.Getenv("AI_SDLC_EXECUTION_CONFIG_PATH"))
		if e != nil {
			return p, intake.Config{}, "", e
		}
		config, e := intake.LoadConfig(os.Getenv("AI_SDLC_INTAKE_CONFIG_PATH"), p.Repository)
		return p, config, authority.Digest(profile), e
	}
	review := func() (publicationflow.ReviewPolicy, error) {
		config, e := reviewflow.LoadPublicInstalled("/etc/ai-sdlc/review/config.json")
		return publicationflow.ReviewPolicy{Profile: authority.Digest(config.Profile), Configuration: config.Digest()}, e
	}
	credential := func() (forge.Credential, error) {
		raw, e := execution.ReadBounded("/etc/ai-sdlc/publication/credential.json", 16384)
		if e != nil {
			return forge.Credential{}, publication.ErrPublication
		}
		var credential forge.Credential
		if authority.DecodeStrict(raw, &credential) != nil {
			return forge.Credential{}, publication.ErrPublication
		}
		return credential, nil
	}
	writer := func() (*publication.Writer, error) {
		value, e := credential()
		if e != nil {
			return nil, e
		}
		return publication.NewWriter(value)
	}
	mergeRuntime := func() (mergeflow.Runtime, error) {
		value, e := credential()
		if e != nil {
			return mergeflow.Runtime{}, e
		}
		if _, e := mergegate.LoadInstalled("/etc/ai-sdlc/merge/policy.json"); e != nil {
			return mergeflow.Runtime{}, e
		}
		gates, e := mergegate.New(value)
		if e != nil {
			return mergeflow.Runtime{}, e
		}
		writer, e := mergewriter.New(value)
		if e != nil {
			return mergeflow.Runtime{}, e
		}
		return mergeflow.Runtime{DB: c.DB, Policy: policy, Review: review,
			Identity:   func() (authority.Identity, error) { _, key, e := c.Authority(); return key, e },
			GatePolicy: func() (mergegate.Policy, error) { return mergegate.LoadInstalled("/etc/ai-sdlc/merge/policy.json") },
			Gates:      gates, Writer: writer}, nil
	}
	ready := func(ctx context.Context) error {
		if e := c.Ready(ctx); e != nil {
			return e
		}
		if _, _, _, e := policy(); e != nil {
			return e
		}
		if _, e := review(); e != nil {
			return e
		}
		if _, e := writer(); e != nil {
			return e
		}
		if bootstrap.MergeEnabled() {
			if _, e := mergeRuntime(); e != nil {
				return e
			}
			journal := store.NewMergeJournal(c.DB, func() (authority.Policy, error) { p, _, e := c.Authority(); return p, e })
			blocked, e := journal.MergeBlocked(ctx)
			if e != nil || blocked {
				return mergeflow.ErrMerge
			}
		}
		blocked, e := c.DB.PublicationBlocked(ctx)
		if e != nil || blocked {
			return publication.ErrPublication
		}
		return nil
	}
	stop := bootstrap.Periodic(5*time.Second, func(ctx context.Context) {
		w, e := writer()
		if e != nil {
			log.Print("publication credential unavailable")
			return
		}
		r := publicationflow.Runtime{DB: c.DB, Policy: policy, Review: review, Writer: w}
		state, e := r.PollOnce(ctx)
		if e != nil {
			log.Print("publication reconciliation stopped safely")
		} else if state != "publication-idle" {
			log.Print("publication reconciliation: " + state)
		}
	})
	defer stop()
	if bootstrap.MergeEnabled() {
		stopMerge := bootstrap.Periodic(time.Minute, func(ctx context.Context) {
			r, e := mergeRuntime()
			if e != nil {
				log.Print("merge credential unavailable")
				return
			}
			state, e := r.PollOnce(ctx)
			if e != nil {
				log.Print("merge reconciliation stopped safely")
			} else if state != "" {
				log.Print("merge reconciliation: " + state)
			}
		})
		defer stopMerge()
	}
	e = service.RunWithReadiness(metadata, ready, func(mux *http.ServeMux) {
		inbox := delivery.Inbox{DB: c.DB, Target: "release-manager"}
		mux.Handle("POST /internal/publish-candidate", inbox.Handler(c.Authority, "publish-candidate", "/internal/publish-candidate", publicationflow.Effect(func() (authority.Policy, error) { p, _, e := c.Authority(); return p, e })))
	})
	if e != nil {
		log.Fatal(e)
	}
}
