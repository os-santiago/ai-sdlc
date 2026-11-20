package main

import (
	"context"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/delivery"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/execution"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelbroker"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/sourcefetch"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/bootstrap"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/service"
)

func main() {
	component, err := bootstrap.Open(context.Background(), "worker")
	if err != nil {
		log.Fatal(err)
	}
	defer component.DB.Close()
	var routes []service.RouteRegistrar
	ready := component.Ready
	if bootstrap.CandidateEnabled() {
		path := os.Getenv("AI_SDLC_EXECUTION_CONFIG_PATH")
		if _, _, e := execution.LoadRuntime(path); e != nil {
			log.Fatal("candidate configuration unavailable")
		}
		gatewaySeconds, e := modelbroker.GatewayTimeoutSeconds(os.Getenv("AI_SDLC_MODEL_GATEWAY_SECONDS"))
		if e != nil {
			log.Fatal("model gateway timeout invalid")
		}
		routes = append(routes, func(mux *http.ServeMux) {
			inbox := delivery.Inbox{DB: component.DB, Target: "worker"}
			mux.Handle("POST /internal/execute-work", inbox.Handler(component.Authority, "execute-work", "/internal/execute-work", execution.Effect(func() (authority.Policy, error) { p, _, e := component.Authority(); return p, e })))
		})
		runtime := execution.Runtime{SourceCache: sourcefetch.NewCache(), Component: component, ConfigPath: path, IntakePath: os.Getenv("AI_SDLC_INTAKE_CONFIG_PATH"), APIServer: "https://kubernetes.default.svc", TokenPath: "/var/run/ai-sdlc/kubernetes/token", CAPath: "/var/run/ai-sdlc/kubernetes/ca.crt", GatewayTokenPath: "/var/run/ai-sdlc/model/token", ProviderTimeoutSeconds: gatewaySeconds, AdoptGatewayProfile: os.Getenv("AI_SDLC_MODEL_GATEWAY_PROFILE") == "benchmark"}
		ready = func(ctx context.Context) error {
			if e := component.Ready(ctx); e != nil {
				return e
			}
			return runtime.Ready(ctx)
		}
		stop := bootstrap.Periodic(5*time.Second, func(ctx context.Context) {
			state, e := runtime.PollOnce(ctx)
			if e != nil {
				code, recorded := execution.WorkerFailureDiagnostic(e)
				log.Print("candidate reconciliation stopped safely: " + execution.DiagnosticStage(e) + " cleanupPending=" + strconv.FormatBool(execution.CleanupPending(e)) + " workerExitCode=" + strconv.Itoa(int(code)) + " failureRecorded=" + strconv.FormatBool(recorded))
			} else if state != "" {
				log.Print("candidate reconciliation: " + state)
			}
		})
		defer stop()
	}
	err = service.RunWithReadiness(service.Metadata{
		Name:           "worker",
		Responsibility: "coordinate isolated implementation attempts",
		Consumes:       []string{"WorkItemReady.v1"},
		Produces:       []string{"ImplementationAttemptDecided.v1"},
	}, ready, routes...)
	if err != nil {
		log.Fatal(err)
	}
}
