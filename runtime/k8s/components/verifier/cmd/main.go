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
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/bootstrap"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/service"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/podtransport"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/verificationflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

func main() {
	c, err := bootstrap.Open(context.Background(), "verifier")
	if err != nil {
		log.Fatal(err)
	}
	defer c.DB.Close()
	if os.Getenv("AI_SDLC_RUNTIME_MODE") != "verification" {
		log.Fatal("explicit verification mode required")
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

	ready := func(ctx context.Context) error {
		if err := c.Ready(ctx); err != nil {
			return err
		}
		if _, _, _, err := newLoader("")(); err != nil {
			return err
		}
		blocked, err := c.DB.VerificationCleanupBlocked(ctx)
		if err != nil || blocked {
			return verificationflow.ControllerError{Stage: "cleanup"}
		}
		return nil
	}
	stop := bootstrap.Periodic(5*time.Second, func(ctx context.Context) {
		config, _, err := execution.LoadRuntime(os.Getenv("AI_SDLC_EXECUTION_CONFIG_PATH"))
		if err != nil {
			log.Print("verification configuration unavailable")
			return
		}
		token, err := execution.ReadBounded("/var/run/ai-sdlc/kubernetes/token", 8192)
		if err != nil {
			return
		}
		ca, err := execution.ReadBounded("/var/run/ai-sdlc/kubernetes/ca.crt", 65536)
		if err != nil {
			return
		}
		client, err := podtransport.NewClient(podtransport.Config{APIServer: "https://kubernetes.default.svc", CA: ca, BearerToken: string(token)})
		if err != nil {
			return
		}
		// Recheck the complete installation config, including pinned images, before every effect.
		sealed, _ := json.Marshal(config)
		r := verificationflow.Runtime{DB: c.DB, Load: newLoader(workspacebundle.Digest(sealed)), Client: client, Image: config.Placement.WorkerImage}
		state, err := r.PollOnce(ctx)
		if err != nil {
			log.Print("verification reconciliation stopped safely: " + verificationflow.DiagnosticStage(err))
		} else if state != "" {
			log.Print("verification reconciliation: " + state)
		}
	})
	defer stop()
	err = service.RunWithReadiness(service.Metadata{Name: "verifier", Responsibility: "independently verify bounded candidate behavior", Consumes: []string{"verify-artifact"}, Produces: []string{"ai-sdlc.behavior-proof/v1"}}, ready, func(mux *http.ServeMux) {
		inbox := delivery.Inbox{DB: c.DB, Target: "verifier"}
		mux.Handle("POST /internal/verify-artifact", inbox.Handler(c.Authority, "verify-artifact", "/internal/verify-artifact", verificationflow.Effect(func() (authority.Policy, error) { p, _, e := c.Authority(); return p, e })))
	})
	if err != nil {
		log.Fatal(err)
	}
}
