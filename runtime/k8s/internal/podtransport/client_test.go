package podtransport

import (
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestClientVerifiesExplicitCAAndRejectsRedirect(t *testing.T) {
	var escaped atomic.Int32
	trap := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { escaped.Add(1) }))
	defer trap.Close()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			w.Header().Set("Location", trap.URL)
			w.WriteHeader(307)
			return
		}
		w.Write([]byte("verified"))
	}))
	defer server.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	client, err := NewClient(Config{APIServer: server.URL, CA: ca, BearerToken: strings.Repeat("x", 32)})
	if err != nil {
		t.Fatal(err)
	}
	// The caller cannot mutate the trusted bytes after client construction.
	for i := range ca {
		ca[i] = 0
	}
	for _, path := range []string{"/", "/redirect"} {
		response, err := client.http.Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if path == "/" && string(raw) != "verified" {
			t.Fatal("TLS exchange failed")
		}
		if path == "/redirect" && response.StatusCode != 307 {
			t.Fatal("redirect followed")
		}
	}
	if escaped.Load() != 0 {
		t.Fatal("redirect reached different endpoint")
	}
}

func TestClientRejectsUntrustedConfiguration(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	for _, endpoint := range []string{"http://127.0.0.1", server.URL + "/path", server.URL + "?token=x", server.URL + "#fragment", "https://user:password@example.invalid", "https://"} {
		if _, err := NewClient(Config{APIServer: endpoint, CA: ca, BearerToken: strings.Repeat("x", 32)}); err == nil {
			t.Fatal("unsafe endpoint accepted")
		}
	}
	for _, config := range []Config{
		{APIServer: server.URL, CA: ca},
		{APIServer: server.URL, CA: ca, BearerToken: "bad\r\nheader"},
		{APIServer: server.URL, CA: []byte("invalid"), BearerToken: strings.Repeat("x", 32)},
		{APIServer: server.URL, CA: ca, ClientCertificate: []byte("missing-key")},
		{APIServer: server.URL, CA: ca, ClientCertificate: []byte("cert"), ClientKey: []byte("key"), BearerToken: strings.Repeat("x", 32)},
	} {
		if _, err := NewClient(config); err == nil {
			t.Fatal("invalid credential/CA accepted")
		}
	}
}
