package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestShadowBoundaryExposesOnlySignedOperationPaths(t *testing.T) {
	t.Setenv("AI_SDLC_RUNTIME_MODE", "shadow")
	for _, path := range []string{"/v1/admissions/evaluate", "/v1/attempts/dispatch", "/internal/execute-work", "/internal/admit-issue?policy=override"} {
		called := false
		handler := runtimeBoundary(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, nil))
		if called || w.Code != http.StatusServiceUnavailable {
			t.Fatal("legacy or unsupported operation exposed")
		}
	}
	for _, path := range []string{"/internal/admit-issue", "/internal/plan-admission"} {
		called := false
		handler := runtimeBoundary(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, path, nil))
		if !called {
			t.Fatal("signed route unreachable")
		}
	}
}

func TestReadinessReportsDependencyFailure(t *testing.T) {
	w := httptest.NewRecorder()
	readiness(func(context.Context) error { return errors.New("unavailable dependency") }).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatal("unavailable dependency reported ready")
	}
}

func TestUnknownRuntimeModeCannotStartServer(t *testing.T) {
	for _, mode := range []string{"active", "typo"} {
		t.Setenv("AI_SDLC_RUNTIME_MODE", mode)
		if err := Run(Metadata{Name: "worker", Responsibility: "test"}); err == nil {
			t.Fatal("unsupported mode started")
		}
	}
}

func TestDecoderRejectsAmbiguousRequests(t *testing.T) {
	for _, body := range []string{`{"value":1} {}`, `{"value":1,"value":2}`, `{"unknown":1}`} {
		var target struct {
			Value int `json:"value"`
		}
		w := httptest.NewRecorder()
		if DecodeJSON(w, httptest.NewRequest(http.MethodPost, "/fixture", strings.NewReader(body)), &target) {
			t.Fatal("ambiguous request accepted")
		}
		if w.Code != http.StatusBadRequest {
			t.Fatal("invalid request not rejected")
		}
	}
}

func TestMaintenanceCannotReachDeliveryHandlers(t *testing.T) {
	t.Setenv("AI_SDLC_RUNTIME_MODE", "maintenance")
	for _, path := range []string{"/v1/admissions/evaluate", "/v1/plans/reconcile", "/v1/attempts/dispatch", "/v1/releases/evaluate", "/metadata"} {
		called := false
		h := maintenanceBoundary(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, nil))
		if called || w.Code != http.StatusServiceUnavailable {
			t.Fatalf("maintenance allowed mutation at %s: called=%v status=%d", path, called, w.Code)
		}
	}
	for _, path := range []string{"/healthz", "/readyz", "/metadata"} {
		w := httptest.NewRecorder()
		maintenanceBoundary(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })).ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("observation denied at %s", path)
		}
	}
}

func TestMetadataRequiresNameAndResponsibility(t *testing.T) {
	tests := []Metadata{
		{},
		{Name: "worker"},
		{Responsibility: "implementation"},
	}
	for _, metadata := range tests {
		if err := Run(metadata); err == nil {
			t.Fatalf("expected invalid metadata to fail: %#v", metadata)
		}
	}
}

func TestValueOrDefault(t *testing.T) {
	t.Setenv("AI_SDLC_TEST_VALUE", "configured")
	if got := valueOrDefault("AI_SDLC_TEST_VALUE", "fallback"); got != "configured" {
		t.Fatalf("got %q", got)
	}
	if got := valueOrDefault("AI_SDLC_MISSING_VALUE", "fallback"); got != "fallback" {
		t.Fatalf("got %q", got)
	}
}

func TestDurationOrDefault(t *testing.T) {
	t.Setenv("AI_SDLC_TEST_DURATION", "90s")
	if got := durationOrDefault("AI_SDLC_TEST_DURATION", time.Minute); got != 90*time.Second {
		t.Fatalf("got %v, want 90s", got)
	}
	if got := durationOrDefault("AI_SDLC_MISSING_DURATION", time.Minute); got != time.Minute {
		t.Fatalf("got %v, want fallback of 1m", got)
	}
}

func TestDurationOrDefaultFallsBackOnUnparsableValue(t *testing.T) {
	// A misconfigured timeout shouldn't keep the component from starting
	// at all — fall back rather than erroring.
	t.Setenv("AI_SDLC_TEST_DURATION", "not-a-duration")
	if got := durationOrDefault("AI_SDLC_TEST_DURATION", time.Minute); got != time.Minute {
		t.Fatalf("got %v, want fallback of 1m for an unparsable value", got)
	}
}
