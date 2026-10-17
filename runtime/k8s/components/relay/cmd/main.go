// Command relay is the 5th deployable binary ADR-0005 added: it polls the
// outbox table (internal/outbox) and delivers unpublished records to the
// other four components over HTTP, closing ADR-0004's "wiring to a real
// event bus" gap without introducing a message broker.
package main

import (
	"context"
	"encoding/json"
	"log"
	"maps"
	"os"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/outbox"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/service"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

func main() {
	ctx := context.Background()

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		log.Fatal("relay: DATABASE_URL is required (relay's only job is polling the outbox table)")
	}
	db, err := store.Open(ctx, databaseURL)
	if err != nil {
		log.Fatalf("relay: open database: %v", err)
	}
	if err := db.Migrate(ctx); err != nil {
		log.Fatalf("relay: migrate database: %v", err)
	}

	relay := &outbox.Relay{DB: db, Routes: routesFromEnv()}
	interval := intervalFromEnv()

	go func() {
		if err := relay.Run(ctx, interval); err != nil {
			log.Fatalf("relay: poll loop stopped: %v", err)
		}
	}()

	err = service.Run(service.Metadata{
		Name:           "relay",
		Responsibility: "poll the outbox table and deliver events/commands to the other components over HTTP",
		Consumes:       []string{"outbox table rows (any event/command type)"},
		Produces:       []string{"HTTP POST to each event type's routed destination"},
	})
	if err != nil {
		log.Fatal(err)
	}
}

// routesFromEnv starts from outbox.DefaultRoutes (the four-stage trail) and
// overrides any entries present in AI_SDLC_RELAY_ROUTES, a JSON object of
// eventType -> destination URL, so a deployment can repoint any single hop
// (e.g. a non-default Service name/namespace) without a code change.
func routesFromEnv() map[string]string {
	routes := maps.Clone(outbox.DefaultRoutes)
	raw := os.Getenv("AI_SDLC_RELAY_ROUTES")
	if raw == "" {
		return routes
	}
	var overrides map[string]string
	if err := json.Unmarshal([]byte(raw), &overrides); err != nil {
		log.Printf("relay: AI_SDLC_RELAY_ROUTES is not valid JSON, ignoring: %v", err)
		return routes
	}
	for eventType, url := range overrides {
		routes[eventType] = url
	}
	return routes
}

func intervalFromEnv() time.Duration {
	raw := os.Getenv("AI_SDLC_RELAY_POLL_INTERVAL")
	if raw == "" {
		return 5 * time.Second
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil || parsed <= 0 {
		log.Printf("relay: AI_SDLC_RELAY_POLL_INTERVAL=%q invalid, using 5s default", raw)
		return 5 * time.Second
	}
	return parsed
}
