package githubapp

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate test key: %v", err)
	}
	return key
}

func pemEncode(t *testing.T, key *rsa.PrivateKey) []byte {
	t.Helper()
	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
}

func TestSplitRepositoryValid(t *testing.T) {
	owner, name, err := splitRepository("os-santiago/homedir")
	if err != nil {
		t.Fatalf("splitRepository() error = %v", err)
	}
	if owner != "os-santiago" || name != "homedir" {
		t.Fatalf("splitRepository() = %q, %q", owner, name)
	}
}

func TestSplitRepositoryInvalid(t *testing.T) {
	for _, repo := range []string{"", "no-slash", "/missing-owner", "missing-name/"} {
		if _, _, err := splitRepository(repo); err == nil {
			t.Errorf("splitRepository(%q) expected error, got nil", repo)
		}
	}
}

func TestSignedJWTRequiresAppIDAndKey(t *testing.T) {
	if _, err := (App{}).signedJWT(); err == nil {
		t.Fatal("signedJWT() with no AppID/PrivateKey, want error")
	}
}

func TestSignedJWTHasThreeSegments(t *testing.T) {
	key := testKey(t)
	app := App{AppID: "123", PrivateKey: pemEncode(t, key)}
	jwt, err := app.signedJWT()
	if err != nil {
		t.Fatalf("signedJWT() error = %v", err)
	}
	if parts := strings.Split(jwt, "."); len(parts) != 3 {
		t.Fatalf("signedJWT() = %d segments, want 3", len(parts))
	}
}

func TestInstallationTokenEndToEnd(t *testing.T) {
	key := testKey(t)
	var sawInstallationAuth, sawTokenAuth string

	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/os-santiago/homedir/installation", func(w http.ResponseWriter, r *http.Request) {
		sawInstallationAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": 42})
	})
	mux.HandleFunc("POST /app/installations/42/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		sawTokenAuth = r.Header.Get("Authorization")
		var body struct {
			Repositories []string `json:"repositories"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if len(body.Repositories) != 1 || body.Repositories[0] != "homedir" {
			t.Errorf("access_tokens request body.repositories = %v, want [homedir]", body.Repositories)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"token":      "ghs_fake-installation-token",
			"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	app := App{AppID: "123", PrivateKey: pemEncode(t, key), BaseURL: server.URL}
	token, err := app.InstallationToken(t.Context(), "os-santiago/homedir")
	if err != nil {
		t.Fatalf("InstallationToken() error = %v", err)
	}
	if token.Value != "ghs_fake-installation-token" {
		t.Fatalf("InstallationToken().Value = %q", token.Value)
	}
	if !strings.HasPrefix(sawInstallationAuth, "Bearer ") || !strings.HasPrefix(sawTokenAuth, "Bearer ") {
		t.Fatalf("expected Bearer JWT auth on both calls, got %q / %q", sawInstallationAuth, sawTokenAuth)
	}
	if sawInstallationAuth != sawTokenAuth {
		t.Fatalf("expected the same JWT reused for both calls, got %q != %q", sawInstallationAuth, sawTokenAuth)
	}
}

func TestInstallationTokenPropagatesHTTPError(t *testing.T) {
	key := testKey(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"Not Found"}`))
	}))
	defer server.Close()

	app := App{AppID: "123", PrivateKey: pemEncode(t, key), BaseURL: server.URL}
	if _, err := app.InstallationToken(t.Context(), "os-santiago/homedir"); err == nil {
		t.Fatal("InstallationToken() expected error on 404, got nil")
	}
}

func TestInstallationTokenRejectsMalformedRepository(t *testing.T) {
	app := App{AppID: "123", PrivateKey: pemEncode(t, testKey(t))}
	if _, err := app.InstallationToken(t.Context(), "not-owner-slash-name"); err == nil {
		t.Fatal("InstallationToken() with malformed repository, want error")
	}
}
