package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/delivery"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/execution"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/intake"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelbroker"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/bootstrap"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/service"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/podtransport"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/publicationflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/repairflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/reviewflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/verificationflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "provenance" {
		if e := runProvenance(); e != nil {
			log.Fatal(e)
		}
		return
	}
	if len(os.Args) != 1 {
		log.Fatal("unknown verifier operation")
	}

	c, err := bootstrap.Open(context.Background(), "verifier")
	if err != nil {
		log.Fatal(err)
	}
	defer c.DB.Close()
	if os.Getenv("AI_SDLC_RUNTIME_MODE") != "verification" {
		log.Fatal("explicit verification mode required")
	}
	reviewOption := os.Getenv("AI_SDLC_INDEPENDENT_REVIEW")
	if reviewOption != "" && reviewOption != "false" && reviewOption != "true" {
		log.Fatal("invalid independent review option")
	}
	reviewEnabled := reviewOption == "true"
	loadReview := func(ctx context.Context) (reviewflow.Configuration, error) {
		if ctx.Err() != nil {
			return reviewflow.Configuration{}, reviewflow.ErrReview
		}
		return reviewflow.LoadInstalled("/etc/ai-sdlc/review/config.json", "/var/run/ai-sdlc/review-model/token")
	}
	newLoader := func(expected string) verificationflow.PolicyLoader {
		return func() (authority.Policy, intake.Config, string, error) {
			p, _, err := c.Authority()
			if err != nil {
				return p, intake.Config{}, "", err
			}
			current, profile, err := execution.LoadRuntime(os.Getenv("AI_SDLC_EXECUTION_CONFIG_PATH"))
			if err != nil {
				return p, intake.Config{}, "", err
			}
			raw, _ := json.Marshal(current)
			if expected != "" && workspacebundle.Digest(raw) != expected {
				return p, intake.Config{}, "", verificationflow.ControllerError{Stage: "configuration"}
			}
			config, err := intake.LoadConfig(os.Getenv("AI_SDLC_INTAKE_CONFIG_PATH"), p.Repository)
			return p, config, workspacebundle.Digest(profile), err
		}
	}

	kubernetes := func() (*podtransport.Client, error) {
		token, err := execution.ReadBounded("/var/run/ai-sdlc/kubernetes/token", 8192)
		if err != nil {
			return nil, err
		}
		ca, err := execution.ReadBounded("/var/run/ai-sdlc/kubernetes/ca.crt", 65536)
		if err != nil {
			return nil, err
		}
		return podtransport.NewClient(podtransport.Config{APIServer: "https://kubernetes.default.svc", CA: ca, BearerToken: string(token)})
	}
	// Verification and reviewer pods run only in the installed sandbox
	// RuntimeClass. Without it the verifier is not ready and claims no work.
	sandbox := &bootstrap.SandboxGate{}
	sandboxPreflight := func(ctx context.Context) error {
		config, _, err := execution.LoadRuntime(os.Getenv("AI_SDLC_EXECUTION_CONFIG_PATH"))
		if err != nil {
			return err
		}
		client, err := kubernetes()
		if err != nil {
			return err
		}
		if err := client.CheckRuntimeClass(ctx, config.Placement.SandboxRuntimeClass()); err != nil || !reviewEnabled {
			return err
		}
		review, err := reviewflow.LoadPublicInstalled("/etc/ai-sdlc/review/config.json")
		if err != nil {
			return err
		}
		return client.CheckRuntimeClass(ctx, review.SandboxRuntimeClass())
	}

	ready := func(ctx context.Context) error {
		if err := c.Ready(ctx); err != nil {
			return err
		}
		if _, _, _, err := newLoader("")(); err != nil {
			return err
		}
		if err := sandbox.Check(ctx, sandboxPreflight); err != nil {
			return err
		}
		blocked, err := c.DB.VerificationCleanupBlocked(ctx)
		if err != nil || blocked {
			return verificationflow.ControllerError{Stage: "cleanup"}
		}
		if reviewEnabled {
			config, e := loadReview(ctx)
			if e != nil {
				return e
			}
			blocked, e := c.DB.ReviewCleanupBlocked(ctx)
			if e != nil || blocked {
				return reviewflow.ErrReview
			}
			provider, e := modelbroker.NewProvider(config.Model)
			if e != nil {
				return e
			}
			return provider.CheckGateway(ctx)
		}
		return nil
	}
	stop := bootstrap.Periodic(5*time.Second, func(ctx context.Context) {
		config, _, err := execution.LoadRuntime(os.Getenv("AI_SDLC_EXECUTION_CONFIG_PATH"))
		if err != nil {
			log.Print("verification configuration unavailable")
			return
		}
		client, err := kubernetes()
		if err != nil {
			return
		}
		if sandbox.Check(ctx, sandboxPreflight) != nil {
			return
		}
		// Recheck the complete installation config, including pinned images, before every effect.
		sealed, _ := json.Marshal(config)
		r := verificationflow.Runtime{DB: c.DB, Load: newLoader(workspacebundle.Digest(sealed)), Client: client, Image: config.Placement.WorkerImage, MailboxImage: config.Placement.MailboxImage, RuntimeClass: config.Placement.SandboxRuntimeClass()}
		var reviewPoll queuePoll
		if reviewEnabled {
			review := reviewflow.Runtime{DB: c.DB, Policy: r.Load, Load: loadReview, Client: client}
			reviewPoll = review.PollOnce
		}
		var handoff func(context.Context) (bool, error)
		if bootstrap.PublicationEnabled() {
			handoff = func(ctx context.Context) (bool, error) {
				return publicationflow.Handoff(ctx, c.DB, r.Load, func() (publicationflow.ReviewPolicy, error) {
					current, e := loadReview(ctx)
					return publicationflow.ReviewPolicy{Profile: authority.Digest(current.Profile), Configuration: current.Digest()}, e
				}, c.Authority)
			}
		}
		if reviewEnabled {
			publish := handoff
			loadRepair := repairflow.InstalledLoader(c.Authority, os.Getenv("AI_SDLC_EXECUTION_CONFIG_PATH"), os.Getenv("AI_SDLC_INTAKE_CONFIG_PATH"), "/etc/ai-sdlc/review/config.json")
			handoff = func(ctx context.Context) (bool, error) {
				// A repair failure cannot starve approved publication, and neither
				// handoff runs before the existing cleanup reconcilers.
				repaired, repairErr := repairflow.Handoff(ctx, c.DB, loadRepair)
				if publish == nil {
					return repaired, repairErr
				}
				published, publishErr := publish(ctx)
				if repairErr != nil {
					log.Print("repair handoff stopped safely")
				}
				return repaired || published, publishErr
			}
		}
		reconcileQueues(ctx, reviewPoll, r.PollOnce, handoff)
	})
	defer stop()
	err = service.RunWithReadiness(service.Metadata{Name: "verifier", Responsibility: "independently verify bounded candidate behavior", Consumes: []string{"verify-artifact"}, Produces: []string{"ai-sdlc.behavior-proof/v1", "ai-sdlc.behavior-proof/v2"}}, ready, func(mux *http.ServeMux) {
		inbox := delivery.Inbox{DB: c.DB, Target: "verifier"}
		mux.Handle("POST /internal/verify-artifact", inbox.Handler(c.Authority, "verify-artifact", "/internal/verify-artifact", verificationflow.Effect(func() (authority.Policy, error) { p, _, e := c.Authority(); return p, e })))
	})
	if err != nil {
		log.Fatal(err)
	}
}
