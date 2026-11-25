package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/metrics"
)

// /metrics is served read-only on the component port in every runtime mode,
// with only the runtime's bounded series.
func TestMetricsServedInEveryModeWithBoundedLabels(t *testing.T) {
	before, _ := metrics.Default.Sample("ai_sdlc_stops_total", "worker", "candidate", "model-provider")
	metrics.Stop("worker", "candidate", "model-provider")
	want := `ai_sdlc_stops_total{component="worker",loop="candidate",stage="model-provider"} ` + strconv.FormatFloat(before+1, 'g', -1, 64) + "\n"
	metrics.Stop("worker", "candidate", "token sk-live-0123456789 for os-santiago/private")
	metrics.SetQueueDepth("worker", "execution", 2)
	label := regexp.MustCompile(`[a-z_]+="([^"]*)"`)
	value := regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,79}$|^\+Inf$|^[0-9.]+$`)
	for _, mode := range []string{"maintenance", "shadow", "candidate", "verification"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("AI_SDLC_RUNTIME_MODE", mode)
			handler := RuntimeBoundary(routes(Metadata{Name: "worker", Responsibility: "test"}, func(context.Context) error { return nil }))
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
			if w.Code != http.StatusOK || w.Header().Get("Content-Type") != metrics.ContentType {
				t.Fatal(w.Code, w.Header())
			}
			body := w.Body.String()
			if !strings.Contains(body, want) ||
				!strings.Contains(body, `ai_sdlc_queue_depth{component="worker",stage="execution"} 2`) ||
				!strings.Contains(body, "# TYPE ai_sdlc_model_call_duration_seconds histogram") {
				t.Fatal(body)
			}
			if strings.Contains(body, "sk-live") || strings.Contains(body, "os-santiago") {
				t.Fatal("unbounded label value exposed")
			}
			for _, m := range label.FindAllStringSubmatch(body, -1) {
				if !value.MatchString(m[1]) {
					t.Fatalf("unbounded label value %q", m[1])
				}
			}
			for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, httptest.NewRequest(method, "/metrics", nil))
				if w.Code == http.StatusOK {
					t.Fatal("metrics accepted", method)
				}
			}
		})
	}
}

// The separate metrics listener serves only GET /metrics: no health,
// metadata or delivery route is reachable through it.
func TestMetricsListenerServesOnlyMetrics(t *testing.T) {
	handler := metricsOnly()
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != metrics.ContentType {
		t.Fatal(w.Code)
	}
	for _, request := range []struct{ method, path string }{
		{http.MethodGet, "/readyz"}, {http.MethodGet, "/healthz"}, {http.MethodGet, "/metadata"},
		{http.MethodPost, "/internal/execute-work"}, {http.MethodPost, "/metrics"}, {http.MethodGet, "/metrics/extra"},
	} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(request.method, request.path, nil))
		if w.Code == http.StatusOK {
			t.Fatal("metrics listener served", request)
		}
	}
}

func TestMetricsAddressMustDifferFromComponentAddress(t *testing.T) {
	t.Setenv("AI_SDLC_RUNTIME_MODE", "maintenance")
	t.Setenv("AI_SDLC_HTTP_ADDRESS", "127.0.0.1:0")
	t.Setenv("AI_SDLC_METRICS_ADDRESS", "127.0.0.1:0")
	if err := Run(Metadata{Name: "worker", Responsibility: "test"}); err == nil {
		t.Fatal("shared address accepted")
	}
	t.Setenv("AI_SDLC_METRICS_ADDRESS", "not an address")
	if err := Run(Metadata{Name: "worker", Responsibility: "test"}); err == nil {
		t.Fatal("invalid metrics address accepted")
	}
}
