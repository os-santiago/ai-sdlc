package main

import (
	"context"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/delivery"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/deploymentflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/execution"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/forge"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/intake"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/memlimit"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/mergeflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/mergegate"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/mergerecovery"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/mergewriter"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/bootstrap"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/metrics"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/service"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/productionflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/publication"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/publicationflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/publicationrecovery"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releaseflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasing"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/reviewflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/terminalflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/terminalwriter"
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	// Large workspace trees: collect before the container limit (internal/memlimit).
	memlimit.Apply()
	c, e := bootstrap.Open(context.Background(), "release-manager")
	if e != nil {
		log.Fatal(e)
	}
	defer c.DB.Close()
	metadata := service.Metadata{Name: "release-manager", Responsibility: "publish independently verified proposals under explicit authority", Consumes: []string{"publish-candidate"}, Produces: []string{"ai-sdlc.publication-proof/v1"}}
	if bootstrap.ReleaseEnabled() {
		metadata.Responsibility = "publish reviewed proposals and promote verified artifacts under finite authority"
		metadata.Consumes = append(metadata.Consumes, "promote-artifact", "record-deployment")
		metadata.Produces = append(metadata.Produces, "deploy-release", "record-production")
	}
	if !bootstrap.PublicationEnabled() {
		defer c.StartQueueSampler("outbox")()
		if e = service.RunWithReadiness(metadata, c.Ready); e != nil {
			log.Fatal(e)
		}
		return
	}
	policy := func() (authority.Policy, intake.Config, string, error) {
		p, _, e := c.Authority()
		if e != nil || !project.Supported(p.Repository) || !bootstrap.PublicationEnabled() {
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
		p, _, e := c.Authority()
		if e != nil {
			return forge.Credential{}, publication.ErrPublication
		}
		if _, e = forge.New(credential, p.Repository); e != nil {
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
	// Pacing for read-only reconciliation of uncertain merge effects lives across polls.
	mergeReconciler := &mergerecovery.Reconciler{DB: c.DB}
	mergeRuntime := func() (mergeflow.Runtime, error) {
		value, e := credential()
		if e != nil {
			return mergeflow.Runtime{}, e
		}
		authorizing, _, _, e := policy()
		if e != nil {
			return mergeflow.Runtime{}, e
		}
		if _, e := mergegate.LoadInstalledForRepository("/etc/ai-sdlc/merge/policy.json", value.Repository, authorizing.ProjectSHA256); e != nil {
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
		observer, e := mergewriter.NewReader(value)
		if e != nil {
			return mergeflow.Runtime{}, e
		}
		return mergeflow.Runtime{DB: c.DB, Policy: policy, Review: review, Reconciler: mergeReconciler, Observer: observer,
			Identity: func() (authority.Identity, error) { _, key, e := c.Authority(); return key, e },
			GatePolicy: func(projectSHA256 string) (mergegate.Policy, error) {
				return mergegate.LoadInstalledForRepository("/etc/ai-sdlc/merge/policy.json", value.Repository, projectSHA256)
			},
			Gates: gates, Writer: writer}, nil
	}
	releaseMounts := releaseflow.InstalledMounts(os.Getenv("AI_SDLC_PROVENANCE_VERIFIER_IMAGE"))
	releaseMounts.Inner, releaseMounts.Identity = c.PolicyPath, c.IdentityPath
	loadRelease := func() (releasing.Configuration, error) {
		if !bootstrap.ReleaseEnabled() {
			return releasing.Configuration{}, releaseflow.ErrFlow
		}
		cfg, _, e := releaseMounts.Load("release-manager")
		return cfg, e
	}
	releaseIdentity := func() (authority.Policy, authority.Identity, error) {
		cfg, key, e := releaseMounts.Load("release-manager")
		if !bootstrap.ReleaseEnabled() || e != nil {
			return authority.Policy{}, authority.Identity{}, releaseflow.ErrFlow
		}
		return cfg.Provenance.Build.Outer, key, nil
	}
	productionIdentity := func() (authority.Policy, authority.Identity, error) {
		p, k, e := c.Authority()
		if e != nil || !bootstrap.ReleaseEnabled() {
			return authority.Policy{}, authority.Identity{}, productionflow.ErrFlow
		}
		return productionflow.LoadOuter(p, k, "/etc/ai-sdlc/outer-policy/policy.json", "release-manager")
	}
	terminalJournal := store.NewTerminalJournal(c.DB, loadRelease)
	ready := func(ctx context.Context) error {
		return releaseReadiness(ctx, c.DB,
			func(context.Context) error {
				if !bootstrap.ReleaseEnabled() {
					return nil
				}
				if _, _, e := productionIdentity(); e != nil {
					return e
				}
				_, e := loadRelease()
				return e
			},
			c.Ready,
			func(context.Context) error { _, _, _, e := policy(); return e },
			func(context.Context) error { _, e := review(); return e },
			func(context.Context) error { _, e := writer(); return e },
			func(context.Context) error {
				if !bootstrap.MergeEnabled() {
					return nil
				}
				_, e := mergeRuntime()
				return e
			})
	}
	// Pacing for read-only reconciliation of uncertain effects lives across polls.
	reconciler := &publicationrecovery.Reconciler{DB: c.DB, Policy: func() (authority.Policy, error) { p, _, _, e := policy(); return p, e }}
	stop := bootstrap.Periodic(5*time.Second, func(ctx context.Context) {
		value, e := credential()
		if e != nil {
			log.Print("publication credential unavailable")
			// The loop observed nothing: drop the series, do not keep its last value.
			metrics.ForgetQuarantined("publication")
			return
		}
		w, e := publication.NewWriter(value)
		if e != nil {
			log.Print("publication credential unavailable")
			// The loop observed nothing: drop the series, do not keep its last value.
			metrics.ForgetQuarantined("publication")
			return
		}
		observer, e := publication.NewReader(value)
		if e != nil {
			log.Print("publication credential unavailable")
			// The loop observed nothing: drop the series, do not keep its last value.
			metrics.ForgetQuarantined("publication")
			return
		}
		r := publicationflow.Runtime{DB: c.DB, Policy: policy, Review: review, Writer: w, Reconciler: reconciler, Observer: observer}
		state, e := r.PollOnce(ctx)
		countDelivery(state)
		if blocked, err := c.DB.PublicationBlocked(ctx); err != nil {
			metrics.ForgetQuarantined("publication")
		} else {
			metrics.Quarantined("publication", blocked)
		}
		if e != nil {
			stage := publicationflow.DiagnosticStage(e)
			if state == "publication-quarantined" {
				stage = "quarantined"
			}
			metrics.Stop("release-manager", "publication", metrics.Unclassified)
			log.Print("publication reconciliation stopped safely: " + stage)
		} else if state != "publication-idle" {
			log.Print("publication reconciliation: " + state)
		}
	})
	defer stop()
	if bootstrap.BuildEnabled() {
		loadBuild := func() (building.Configuration, authority.Identity, error) {
			p, key, e := c.Authority()
			if e != nil {
				return building.Configuration{}, key, e
			}
			i, e := buildflow.LoadCurrent("/etc/ai-sdlc/build/config.json", c.PolicyPath, "/etc/ai-sdlc/outer-policy/policy.json")
			if e != nil || i.Inner.Revision() != p.Revision() || key.Validate(i.Outer) != nil {
				return building.Configuration{}, key, buildflow.ErrBuildFlow
			}
			return i.Configuration(), key, nil
		}
		stopBuild := bootstrap.Periodic(5*time.Second, func(ctx context.Context) {
			if _, e := buildflow.DispatchMerged(ctx, c.DB, loadBuild); e != nil {
				metrics.Stop("release-manager", "merged-source-dispatch", metrics.Unclassified)
				log.Print("merged-source dispatch stopped safely")
			}
		})
		defer stopBuild()
	}
	if bootstrap.MergeEnabled() {
		// The rotation outlives each poll, so a publication whose gate stays
		// unready cannot hold later ones.
		rotation := &mergeflow.Rotation{}
		stopMerge := bootstrap.Periodic(time.Minute, func(ctx context.Context) {
			r, e := mergeRuntime()
			if e != nil {
				log.Print("merge credential unavailable")
				metrics.ForgetQuarantined("merge")
				return
			}
			r.Rotation = rotation
			state, e := r.PollOnce(ctx)
			countDelivery(state)
			sampleMergeQuarantine(ctx, c.DB, policy)
			if e != nil {
				// state is a fixed controller constant ("merge-quarantined",
				// "merge-stopped", ...); it names why the loop stopped.
				if state == "" {
					state = metrics.Unclassified
				}
				metrics.Stop("release-manager", "merge", state)
				if mergeflow.AttemptsExhausted(e) {
					metrics.MergeAttemptsExhausted()
				}
				log.Print(mergeStopLine(state, e))
			} else if state != "" {
				log.Print("merge reconciliation: " + state)
			}
		})
		defer stopMerge()
	}
	if bootstrap.ReleaseEnabled() {
		stopTerminal := bootstrap.Periodic(5*time.Second, func(ctx context.Context) {
			value, e := credential()
			if e != nil {
				log.Print("terminal credential unavailable")
				return
			}
			w, e := terminalwriter.New(value)
			if e != nil {
				log.Print("terminal writer unavailable")
				return
			}
			controller := terminalflow.Controller{Journal: terminalJournal, Writer: w}
			if _, e = controller.RunOne(ctx); e != nil {
				metrics.Stop("release-manager", "terminal", metrics.Unclassified)
				log.Print("terminal reconciliation stopped safely")
			}
		})
		defer stopTerminal()
		stopProduction := bootstrap.Periodic(5*time.Second, func(ctx context.Context) {
			if _, e := productionflow.DispatchCompleted(ctx, c.DB, productionIdentity); e != nil {
				metrics.Stop("release-manager", "production-dispatch", metrics.Unclassified)
				log.Print("production fact dispatch stopped safely")
			}
		})
		defer stopProduction()
		stopRelease := bootstrap.Periodic(5*time.Second, func(ctx context.Context) {
			state, e := (releaseflow.Runtime{DB: c.DB, Load: loadRelease}).PollOnce(ctx)
			if e != nil {
				metrics.Stop("release-manager", "promotion", metrics.Unclassified)
				log.Print("promotion reconciliation stopped safely")
			} else if state != "promotion-idle" {
				log.Print(state)
			}
			_, e = deploymentflow.DispatchPlanned(ctx, c.DB, func() (releasing.Configuration, authority.Identity, error) {
				return releaseMounts.Load("release-manager")
			})
			if e != nil {
				metrics.Stop("release-manager", "planned-release-dispatch", metrics.Unclassified)
				log.Print("planned-release dispatch stopped safely")
			}
		})
		defer stopRelease()
	}
	defer c.StartQueueSampler("outbox", "publication")()
	e = service.RunWithReadiness(metadata, ready, func(mux *http.ServeMux) {
		inbox := delivery.Inbox{DB: c.DB, Target: "release-manager"}
		if bootstrap.ReleaseEnabled() {
			mux.Handle("POST /internal/record-deployment", inbox.Handler(releaseIdentity, "record-deployment", "/internal/record-deployment", terminalflow.InboxEffect(terminalJournal)))
			mux.Handle("POST /internal/promote-artifact", inbox.Handler(releaseIdentity, "promote-artifact", "/internal/promote-artifact", releaseflow.InboxEffect(store.NewPromotionJournal(c.DB, loadRelease))))
		}
		mux.Handle("POST /internal/publish-candidate", inbox.Handler(c.Authority, "publish-candidate", "/internal/publish-candidate", publicationflow.Effect(func() (authority.Policy, error) { p, _, e := c.Authority(); return p, e })))
	})
	if e != nil {
		log.Fatal(e)
	}
}

// releaseReadiness runs the configuration checks in order, then requires an
// unblocked publication journal.
//
// A merge quarantine does not make the release manager unready: it blocks only
// later merges, never publication or the inbox routes, and failing readiness
// removed the Service endpoints so that every publish call timed out instead
// (battery 39). The merge loop reports it as the stop "merge-quarantined" and
// the gauge ai_sdlc_quarantined{journal="merge"}.
func releaseReadiness(ctx context.Context, db *store.DB, checks ...func(context.Context) error) error {
	for _, check := range checks {
		if e := check(ctx); e != nil {
			return e
		}
	}
	blocked, e := db.PublicationBlocked(ctx)
	if e != nil || blocked {
		return publication.ErrPublication
	}
	return nil
}

// mergeStopLine is the bounded merge stop log line. A stopped claimed run also
// names its stage (mergeflow.DiagnosticStage), for example
// "merge-stopped stage=gate-changed-checks". The stage is diagnostic only: the
// journal decides whether the run sent nothing and records "not-sent" only
// then; a publication-fence stop whose seal failed, or an effect-intent stop
// whose commit landed, stays uncertain. When that not-sent request was the
// publication's last allowed attempt, the line adds "attempts=exhausted".
func mergeStopLine(state string, e error) string {
	line := "merge reconciliation stopped safely: " + state
	if state == "merge-stopped" {
		line += " stage=" + mergeflow.DiagnosticStage(e)
		if mergeflow.AttemptsExhausted(e) {
			line += " attempts=exhausted"
		}
	}
	return line
}

// countDelivery counts the outcome of one claimed publication or merge
// attempt. Idle, quarantined and other standing states are not attempts.
func countDelivery(state string) {
	if outcome := deliveryOutcome(state); outcome != "" {
		metrics.Delivery(outcome)
	}
}

func deliveryOutcome(state string) string {
	switch state {
	case "pull-request-published":
		return "published"
	case "publication-stopped":
		return "publication-stopped"
	case "merge-complete":
		return "merged"
	case "merge-stopped":
		return "merge-stopped"
	}
	return ""
}

// sampleMergeQuarantine records whether an uncertain merge effect quarantines
// merges. A journal that cannot be read is dropped from the output rather than
// left at its last value.
func sampleMergeQuarantine(ctx context.Context, db *store.DB, policy func() (authority.Policy, intake.Config, string, error)) {
	journal := store.NewMergeJournal(db, func() (authority.Policy, error) { p, _, _, e := policy(); return p, e })
	blocked, e := journal.MergeBlocked(ctx)
	if e != nil {
		metrics.ForgetQuarantined("merge")
		return
	}
	metrics.Quarantined("merge", blocked)
}
