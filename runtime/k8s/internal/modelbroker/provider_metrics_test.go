package modelbroker

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/metrics"
)

func sample(role, outcome string) (calls, observed float64) {
	calls, _ = metrics.Default.Sample("ai_sdlc_model_calls_total", role, outcome)
	observed, _ = metrics.Default.Sample("ai_sdlc_model_call_duration_seconds", role, outcome)
	return calls, observed
}

// Each broker call to the gateway is counted once under its role, with a
// fixed outcome class and its duration; never the response or error text.
func TestObservedInvokeCountsOneBoundedOutcomePerCall(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		switch r.Header.Get("X-Test-Case") {
		case "ok":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"secret model output"}}]}`)
		case "unauthorized":
			w.WriteHeader(http.StatusUnauthorized)
		case "unavailable":
			w.WriteHeader(http.StatusServiceUnavailable)
		case "redirect":
			w.Header().Set("Location", "http://example.invalid/")
			w.WriteHeader(http.StatusFound)
		case "html":
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, "<html>")
		case "user-role":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"user"}}]}`)
		case "slow":
			<-release
		}
	}))
	defer server.Close()
	defer close(release)
	provider, err := NewProvider(ProviderConfig{Endpoint: server.URL + "/v1/chat/completions", BearerToken: strings.Repeat("x", 32), AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	// The test case travels in a header the provider never sets: wrap the
	// transport to add it.
	base := provider.client.Transport
	for _, tc := range []struct{ name, role, outcome string }{
		{"ok", "generator", metrics.ModelSuccess},
		{"unauthorized", "reviewer", metrics.ModelStatus4xx},
		{"unavailable", "generator", metrics.ModelStatus5xx},
		{"redirect", "generator", metrics.ModelStatusOther},
		{"html", "reviewer", metrics.ModelInvalidResponse},
		{"user-role", "generator", metrics.ModelInvalidResponse},
		{"slow", "reviewer", metrics.ModelTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider.client.Transport = roundTripper(func(r *http.Request) (*http.Response, error) {
				r.Header.Set("X-Test-Case", tc.name)
				return base.RoundTrip(r)
			})
			calls, observed := sample(tc.role, tc.outcome)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			if tc.name == "slow" {
				ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
			}
			defer cancel()
			raw, err := provider.observedInvoke(ctx, tc.role, []byte(`{}`), 1024)
			if (err == nil) != (tc.outcome == metrics.ModelSuccess) || (err == nil && len(raw) == 0) {
				t.Fatal(err)
			}
			after, afterObserved := sample(tc.role, tc.outcome)
			if after != calls+1 || afterObserved != observed+1 {
				t.Fatalf("outcome %s not counted once: %v -> %v", tc.outcome, calls, after)
			}
		})
	}
	// An explicit cancellation before the deadline is not a timeout.
	provider.client.Transport = roundTripper(func(r *http.Request) (*http.Response, error) {
		r.Header.Set("X-Test-Case", "slow")
		return base.RoundTrip(r)
	})
	canceled, _ := sample("reviewer", metrics.ModelCanceled)
	timeouts, _ := sample("reviewer", metrics.ModelTimeout)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	if _, err := provider.observedInvoke(ctx, "reviewer", []byte(`{}`), 1024); err == nil {
		t.Fatal("canceled call answered")
	}
	if after, _ := sample("reviewer", metrics.ModelCanceled); after != canceled+1 {
		t.Fatal("cancellation not counted as canceled")
	}
	if after, _ := sample("reviewer", metrics.ModelTimeout); after != timeouts {
		t.Fatal("cancellation counted as a timeout")
	}
	provider.client.Transport = base
	calls, _ := sample("generator", metrics.ModelRejected)
	if _, err := provider.observedInvoke(context.Background(), "generator", nil, 1024); err == nil {
		t.Fatal("empty request accepted")
	}
	if after, _ := sample("generator", metrics.ModelRejected); after != calls+1 {
		t.Fatal("refused request not counted as rejected")
	}
	stopped := httptest.NewServer(http.NotFoundHandler())
	endpoint := stopped.URL + "/v1/chat/completions"
	stopped.Close()
	closed, err := NewProvider(ProviderConfig{Endpoint: endpoint, BearerToken: strings.Repeat("x", 32), AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	calls, _ = sample("reviewer", metrics.ModelTransport)
	if _, err := closed.observedInvoke(context.Background(), "reviewer", []byte(`{}`), 1024); err == nil {
		t.Fatal("closed gateway answered")
	}
	if after, _ := sample("reviewer", metrics.ModelTransport); after != calls+1 {
		t.Fatal("connection failure not counted as transport")
	}
	var text strings.Builder
	metrics.Default.Write(&text)
	if strings.Contains(text.String(), "secret model output") || strings.Contains(text.String(), "example.invalid") {
		t.Fatal("response content reached the metrics")
	}
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
