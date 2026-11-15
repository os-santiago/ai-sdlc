package main

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/forge"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/intake"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/bootstrap"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/service"
)

func main() {
	component, err := bootstrap.Open(context.Background(), "intake")
	if err != nil {
		log.Fatal(err)
	}
	defer component.DB.Close()
	if os.Getenv("AI_SDLC_RUNTIME_MODE") == "shadow" {
		policy, _, err := component.Authority()
		if err != nil {
			log.Fatal(err)
		}
		config, err := intake.LoadConfig(os.Getenv("AI_SDLC_INTAKE_CONFIG_PATH"), policy.Repository)
		if err != nil {
			log.Fatal(err)
		}
		path := os.Getenv("AI_SDLC_GITHUB_CREDENTIAL_PATH")
		if _, err := forge.Load(path, policy.Repository); err != nil {
			log.Fatal(err)
		}
		stop := bootstrap.Periodic(5*time.Second, func(ctx context.Context) {
			current, _, err := component.Authority()
			if err != nil || current.Freeze {
				return
			}
			client, err := forge.Load(path, current.Repository)
			if err != nil {
				log.Print("intake credential unavailable")
				return
			}
			poller := intake.Poller{DB: component.DB, Config: config, Source: client, Authority: component.Authority}
			if n, err := poller.PollOnce(ctx, time.Now()); err != nil {
				log.Print("intake scan incomplete; cursor retained")
			} else if n > 0 {
				log.Printf("intake recorded %d issue transitions", n)
			}
		})
		defer stop()
	}
	err = service.RunWithReadiness(service.Metadata{Name: "intake", Responsibility: "observe authorized GitHub issues and emit durable signed snapshots", Consumes: []string{"GitHub issues"}, Produces: []string{"IssueObserved.v1"}}, component.Ready)
	if err != nil {
		log.Fatal(err)
	}
}
