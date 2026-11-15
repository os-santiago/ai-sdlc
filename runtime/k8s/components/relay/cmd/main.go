package main

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/delivery"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/bootstrap"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/service"
)

func main() {
	component, err := bootstrap.Open(context.Background(), "relay")
	if err != nil {
		log.Fatal(err)
	}
	defer component.DB.Close()
	if os.Getenv("AI_SDLC_RUNTIME_MODE") == "shadow" {
		var routes map[string]string
		if authority.DecodeStrict([]byte(os.Getenv("AI_SDLC_RELAY_ROUTES")), &routes) != nil || len(routes) != 2 || routes["admit-issue"] == "" || routes["plan-admission"] == "" {
			log.Fatal("shadow relay requires exactly its two reviewed routes")
		}
		stop := bootstrap.Periodic(time.Second, func(ctx context.Context) {
			policy, identity, err := component.Authority()
			if err != nil || policy.Freeze {
				return
			}
			relay := delivery.Relay{DB: component.DB, Identity: identity, Routes: routes}
			if _, err := relay.PollOnce(ctx, policy, time.Now()); err != nil {
				log.Print("signed relay poll failed")
			}
		})
		defer stop()
	}
	err = service.RunWithReadiness(service.Metadata{
		Name:           "relay",
		Responsibility: "deliver signed durable facts with bounded transport leases",
		Consumes:       []string{"authorized_outbox"},
		Produces:       []string{"authenticated component delivery"},
	}, component.Ready)
	if err != nil {
		log.Fatal(err)
	}
}
