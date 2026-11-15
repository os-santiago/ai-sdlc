package main

import (
	"context"
	"log"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/bootstrap"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/service"
)

func main() {
	component, err := bootstrap.Open(context.Background(), "relay")
	if err != nil {
		log.Fatal(err)
	}
	defer component.DB.Close()
	// Only observation endpoints are installed during this integration stage.
	// Legacy unsigned routes remain available to isolated Go fixtures only.
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
