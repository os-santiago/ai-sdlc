package modelbroker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInstallationTimeoutIsBoundedAndDefaultUnchanged(t *testing.T) {
	for value, want := range map[string]int{"": 0, "30": 30, "330": 330, "600": 600} {
		if got, err := GatewayTimeoutSeconds(value); err != nil || got != want {
			t.Fatal(value, got, err)
		}
	}
	for _, value := range []string{"29", "601", "abc", "060", "-1", " 60"} {
		if _, err := GatewayTimeoutSeconds(value); err == nil {
			t.Fatal("accepted", value)
		}
	}
	base := ProviderConfig{Endpoint: "http://127.0.0.1:8787/v1/chat/completions", BearerToken: strings.Repeat("t", 32), AllowLoopbackHTTP: true}
	for _, seconds := range []int{0, 30, 600} {
		p, err := NewProvider(ProviderConfig{Endpoint: base.Endpoint, BearerToken: base.BearerToken, AllowLoopbackHTTP: true, TimeoutSeconds: seconds})
		if err != nil {
			t.Fatal(seconds, err)
		}
		want := seconds
		if want == 0 {
			want = 30
		}
		if int(p.client.Timeout.Seconds()) != want {
			t.Fatal("timeout", seconds, p.client.Timeout)
		}
	}
	for _, seconds := range []int{1, 29, 601, -5} {
		if _, err := NewProvider(ProviderConfig{Endpoint: base.Endpoint, BearerToken: base.BearerToken, AllowLoopbackHTTP: true, TimeoutSeconds: seconds}); err == nil {
			t.Fatal("unbounded timeout accepted", seconds)
		}
	}
}

func TestGatewayProfileIsAuthenticatedStrictAndBounded(t *testing.T) {
	token := strings.Repeat("t", 32) // synthetic, low-entropy fixture
	body := ""
	status := 200
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/gateway/profile" || r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(401)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	p, err := NewProvider(ProviderConfig{Endpoint: server.URL + "/v1/chat/completions", BearerToken: token, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	body = `{"timeoutSeconds":225}`
	if got, err := p.GatewayProfile(context.Background()); err != nil || got != 225 {
		t.Fatal(got, err)
	}
	for _, bad := range []string{`{"timeoutSeconds":29}`, `{"timeoutSeconds":571}`, `{"timeoutSeconds":225,"x":1}`, `{"timeoutSeconds":"225"}`, `{}`} {
		body = bad
		if _, err := p.GatewayProfile(context.Background()); err == nil {
			t.Fatal("accepted", bad)
		}
	}
	body, status = `{"timeoutSeconds":225}`, 503
	if _, err := p.GatewayProfile(context.Background()); err == nil {
		t.Fatal("accepted a gateway that is still benchmarking")
	}
}
