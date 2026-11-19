package execution

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// The worker claims no work until the gateway has benchmarked the model, and
// then bounds every call by the measured timeout under the installation ceiling.
func TestWorkerAdoptsTheBenchmarkedGatewayTimeoutWithinTheCeiling(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef"
	body, status := `{"timeoutSeconds":225}`, 200
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/gateway/profile" || r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(401)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	r := Runtime{GatewayTokenPath: path, ProviderEndpoint: server.URL + "/v1/chat/completions", ProviderTimeoutSeconds: 330}
	provider, err := r.gateway()
	if err != nil {
		t.Fatal(err)
	}
	if same, err := r.benchmarked(context.Background(), provider); err != nil || same != provider {
		t.Fatal("profile adoption must be opt-in", err)
	}
	r.AdoptGatewayProfile = true
	if adopted, err := r.benchmarked(context.Background(), provider); err != nil || adopted == provider {
		t.Fatal("benchmarked timeout not adopted", err)
	}
	for _, c := range []struct {
		body    string
		status  int
		ceiling int
	}{{`{"timeoutSeconds":301}`, 200, 330}, {`{"timeoutSeconds":225}`, 503, 330}, {`{"timeoutSeconds":225}`, 200, 0}} {
		body, status, r.ProviderTimeoutSeconds = c.body, c.status, c.ceiling
		if _, err := r.benchmarked(context.Background(), provider); err == nil {
			t.Fatal("accepted", c)
		}
	}
}
