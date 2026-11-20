package reviewflow

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelbroker"
)

// Reviews are refused before any queue or claim unless the benchmarked gateway
// timeout plus the broker margin fits the reviewer call window.
func TestReviewGateRequiresTheBenchmarkedTimeoutToFitTheReviewerWindow(t *testing.T) {
	token := strings.Repeat("t", 32)
	body, status := `{"timeoutSeconds":95}`, 200
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/gateway/profile" || r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(401)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	provider, err := modelbroker.NewProvider(modelbroker.ProviderConfig{Endpoint: server.URL + "/v1/chat/completions", BearerToken: token, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	profile := func(window int) []byte {
		p := executor.Profile{Version: executor.ReviewProfileVersion, Model: "fixture-model", MaxSteps: 8, MaxSeconds: 180, MaxTokens: 1024, ADevSHA256: strings.Repeat("a", 64), CallSeconds: window}
		raw, _ := json.Marshal(p)
		return raw
	}
	if err = gatewayProfileFits(context.Background(), provider, profile(170)); err != nil {
		t.Fatal("fitting gateway refused", err)
	}
	for name, c := range map[string]struct {
		body    string
		status  int
		profile []byte
	}{
		"slower than the window":     {`{"timeoutSeconds":160}`, 200, profile(170)},
		"default 60 s window":        {`{"timeoutSeconds":95}`, 200, profile(0)},
		"gateway still benchmarking": {`{"timeoutSeconds":95}`, 503, profile(170)},
		"invalid reviewer profile":   {`{"timeoutSeconds":95}`, 200, []byte(`{"version":"x"}`)},
	} {
		body, status = c.body, c.status
		if err = gatewayProfileFits(context.Background(), provider, c.profile); err == nil {
			t.Fatal("review admitted:", name)
		}
	}
}

// The broker timeout is installation-only: the verifier and the publisher
// compute the same review configuration digest whatever each was given.
func TestReviewConfigurationDigestIgnoresTheBrokerTimeout(t *testing.T) {
	c := Configuration{Namespace: "ai-sdlc-reviewers", Profile: []byte("{}"), Model: modelbroker.ProviderConfig{Endpoint: "http://127.0.0.1:8787/v1/chat/completions", AllowLoopbackHTTP: true}}
	before := c.Digest()
	c.Model.TimeoutSeconds = 330
	c.RequireGatewayProfile = true
	if c.Digest() != before {
		t.Fatal("broker timeout or gate changed the review configuration digest")
	}
}
