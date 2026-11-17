package main

import (
	"context"
	"errors"
	"log"
	"os"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildflow"
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
	if bootstrap.IntakeEnabled() {
		routes, err := reviewedRoutes(os.Getenv("AI_SDLC_RELAY_ROUTES"))
		if err != nil {
			log.Fatal(err)
		}
		stop := bootstrap.Periodic(time.Second, func(ctx context.Context) {
			policy, identity, err := component.Authority()
			if err != nil || policy.Freeze {
				return
			}
			relay := delivery.Relay{DB: component.DB, Identity: identity, Routes: routes}
			if bootstrap.BuildEnabled() {
				installed, e := buildflow.LoadCurrent("/etc/ai-sdlc/build/config.json", component.PolicyPath, "/etc/ai-sdlc/outer-policy/policy.json")
				if e != nil || installed.Inner.Revision() != policy.Revision() || identity.Validate(installed.Outer) != nil {
					log.Print("build relay authority unavailable")
					return
				}
				if _, e = relay.PollLoops(ctx, policy, installed.Outer, time.Now()); e != nil {
					log.Print("signed loop relay poll failed")
				}
				return
			}
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

// reviewedRoutes admits exactly the finite operations enabled by installation.
// Delivery still authenticates each receipt, transport and destination path.
func reviewedRoutes(raw string) (map[string]string, error) {
	if bootstrap.ValidateMode() != nil || !bootstrap.IntakeEnabled() || len(raw) > 32768 {
		return nil, errors.New("invalid relay capability")
	}
	required := []string{"admit-issue", "plan-admission"}
	if bootstrap.CandidateEnabled() {
		required = append(required, "execute-work")
	}
	if os.Getenv("AI_SDLC_RUNTIME_MODE") == "verification" {
		required = append(required, "verify-artifact")
	}
	if bootstrap.PublicationEnabled() {
		required = append(required, "publish-candidate")
	}
	if bootstrap.BuildEnabled() {
		required = append(required, "build-merged")
	}
	if bootstrap.ProvenanceEnabled() {
		required = append(required, "verify-built")
	}
	if bootstrap.ReleaseEnabled() {
		required = append(required, "promote-artifact", "deploy-release")
	}
	var routes map[string]string
	if authority.DecodeStrict([]byte(raw), &routes) != nil || len(routes) != len(required) {
		return nil, errors.New("relay requires exactly its reviewed routes")
	}
	for _, operation := range required {
		if routes[operation] == "" {
			return nil, errors.New("relay route missing")
		}
	}
	return routes, nil
}
