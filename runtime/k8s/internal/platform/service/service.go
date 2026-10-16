// Package service provides the minimal HTTP runtime every ai-sdlc k8s/OpenShift
// control-plane component shares: health/readiness/metadata endpoints, a bounded
// HTTP server, structured logging and strict JSON (de)serialization.
//
// Ported from os-santiago/homedir-ai-sdlc (future-go/internal/platform/service),
// unchanged in behavior — see docs/architecture/adr/0004-cloud-native-k8s-openshift-runtime.md
// for provenance and the rest of the ecosystem layering.
package service

import (
	"encoding/json"
	"errors"
	"fmt"
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
	if metadata.Name == "" || metadata.Responsibility == "" {
		return errors.New("component metadata is incomplete")
	}

	address := valueOrDefault("AI_SDLC_HTTP_ADDRESS", ":8080")
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("component", metadata.Name)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		WriteJSON(w, http.StatusOK, map[string]string{"status": "healthy"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		WriteJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	mux.HandleFunc("GET /metadata", func(w http.ResponseWriter, _ *http.Request) {
		WriteJSON(w, http.StatusOK, metadata)
	})
	for _, register := range registrars {
		if register != nil {
			register(mux)
		}
	}

	server := &http.Server{
		Addr:              address,
		Handler:           requestLog(logger, mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
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

// WriteJSON writes a JSON response with the given status code.
func WriteJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// DecodeJSON strictly decodes a bounded (1MiB) JSON request body, rejecting
// unknown fields. Writes a 400 response and returns false on failure.
func DecodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		WriteJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
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
