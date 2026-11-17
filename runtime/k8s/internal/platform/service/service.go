// Package service provides the minimal HTTP runtime every ai-sdlc k8s/OpenShift
// control-plane component shares: health/readiness/metadata endpoints, a bounded
// HTTP server, structured logging and strict JSON (de)serialization.
//
// Ported from os-santiago/homedir-ai-sdlc (future-go/internal/platform/service),
// unchanged in behavior — see docs/architecture/adr/0004-cloud-native-k8s-openshift-runtime.md
// for provenance and the rest of the ecosystem layering.
package service

import (
	"context"
	"encoding/json"
	"io"

	"errors"
	"fmt"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/bootstrap"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// Metadata self-describes a component's place in the pipeline (consumed
// events/commands in, events/commands produced out). Exposed at GET /metadata
// so operators can inspect wiring without reading source.
type Metadata struct {
	Name           string   `json:"name"`
	Responsibility string   `json:"responsibility"`
	Consumes       []string `json:"consumes"`
	Produces       []string `json:"produces"`
}

// RouteRegistrar attaches component-specific HTTP routes to the shared mux.
type RouteRegistrar func(*http.ServeMux)

// Run starts the component's HTTP server and blocks until SIGINT/SIGTERM.
// Every component (admission-controller, orchestrator, worker, release-manager)
// calls this from main(); it is the only place that touches the network.
func Run(metadata Metadata, registrars ...RouteRegistrar) error {
	return RunWithReadiness(metadata, nil, registrars...)
}

func RunWithReadiness(metadata Metadata, ready func(context.Context) error, registrars ...RouteRegistrar) error {
	if metadata.Name == "" || metadata.Responsibility == "" {
		return errors.New("component metadata is incomplete")
	}

	if err := bootstrap.ValidateMode(); err != nil {
		return err
	}

	address := valueOrDefault("AI_SDLC_HTTP_ADDRESS", ":8080")
	writeTimeout := durationOrDefault("AI_SDLC_HTTP_WRITE_TIMEOUT", 15*time.Second)
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("component", metadata.Name)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		WriteJSON(w, http.StatusOK, map[string]string{"status": "healthy"})
	})
	mux.Handle("GET /readyz", readiness(ready))
	mux.HandleFunc("GET /metadata", func(w http.ResponseWriter, _ *http.Request) {
		WriteJSON(w, http.StatusOK, metadata)
	})
	for _, register := range registrars {
		if register != nil {
			register(mux)
		}
	}

	// Delivery handlers will enqueue bounded work; model execution never
	// belongs inside a control-plane HTTP request.

	server := &http.Server{
		Addr:              address,
		Handler:           requestLog(logger, RuntimeBoundary(mux)),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       60 * time.Second,
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		logger.Info("shutdown requested")
		_ = server.Close()
	}()

	logger.Info("component started", "address", address)
	err := server.ListenAndServe()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve %s: %w", metadata.Name, err)
	}
	return nil
}

// Maintenance installations expose observation only. This is not component
// authentication: live authority enforcement is tracked separately.
func maintenanceBoundary(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || (r.URL.Path != "/healthz" && r.URL.Path != "/readyz" && r.URL.Path != "/metadata") {
			WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "runtime_in_maintenance", "delivery": "disabled"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RuntimeBoundary limits reachable paths; registered inboxes still authenticate every message.
func RuntimeBoundary(next http.Handler) http.Handler {
	mode := os.Getenv("AI_SDLC_RUNTIME_MODE")
	if mode != "shadow" && mode != "candidate" && mode != "verification" {
		return maintenanceBoundary(next)
	}
	observation := maintenanceBoundary(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.RawQuery == "" && (r.URL.Path == "/internal/admit-issue" || r.URL.Path == "/internal/plan-admission" || ((mode == "candidate" || mode == "verification") && r.URL.Path == "/internal/execute-work") || (mode == "verification" && r.URL.Path == "/internal/verify-artifact") || (bootstrap.PublicationEnabled() && r.URL.Path == "/internal/publish-candidate") || (bootstrap.BuildEnabled() && r.URL.Path == "/internal/build-merged") || (bootstrap.ProvenanceEnabled() && r.URL.Path == "/internal/verify-built") || (bootstrap.ReleaseEnabled() && (r.URL.Path == "/internal/promote-artifact" || r.URL.Path == "/internal/deploy-release"))) {
			next.ServeHTTP(w, r)
			return
		}
		observation.ServeHTTP(w, r)
	})
}

func readiness(ready func(context.Context) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ready != nil {
			ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
			defer cancel()
			if err := ready(ctx); err != nil {
				WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not_ready"})
				return
			}
		}
		mode, delivery := "maintenance", "disabled"
		if os.Getenv("AI_SDLC_RUNTIME_MODE") == "shadow" {
			mode, delivery = "shadow", "admission-only"
		} else if os.Getenv("AI_SDLC_RUNTIME_MODE") == "candidate" {
			mode, delivery = "candidate", "candidate-only"
		}
		if os.Getenv("AI_SDLC_RUNTIME_MODE") == "verification" {
			mode, delivery = "verification", "verification-only"
		}
		if bootstrap.PublicationEnabled() {
			delivery = "verified-publication-only"
		}
		if bootstrap.MergeEnabled() {
			delivery = "verified-low-risk-merge-only"
		}
		WriteJSON(w, http.StatusOK, map[string]string{"status": "ready", "mode": mode, "delivery": delivery})
	})
}

// WriteJSON writes a JSON response with the given status code.
func WriteJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// DecodeJSON strictly decodes a bounded (1MiB) JSON request body, rejecting
// unknown fields. Writes a 400 response and returns false on failure.
func DecodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil || authority.DecodeStrict(raw, target) != nil {
		WriteJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON request"})
		return false
	}
	return true
}

func requestLog(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		logger.Info("request", "method", r.Method, "path", r.URL.Path, "duration", time.Since(start))
	})
}

func valueOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

// durationOrDefault parses name as a Go duration string (e.g. "90s",
// "25m"); an unset or unparsable value falls back to fallback rather than
// erroring, since a misconfigured timeout shouldn't keep a component from
// starting.
func durationOrDefault(name string, fallback time.Duration) time.Duration {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return fallback
	}
	return parsed
}
