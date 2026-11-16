// Trusted isolated transport fixture. Its synthetic response is NOT a vault or
// cryptographic test and must never be used as Saferus or workflow evidence.
package main

import (
	"net/http"
	"time"
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<title>Saferus fixture</title><button>Run verification</button>"))
	})
	mux.HandleFunc("POST /api/demo", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"verified":true,"algorithm":"AES-256-GCM","tokenTTLSeconds":60,"tokenRevoked":true,"agentless":true}`))
	})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; object-src 'none'; frame-ancestors 'none'; base-uri 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		mux.ServeHTTP(w, r)
	})
	server := &http.Server{Addr: ":8443", Handler: handler, ReadHeaderTimeout: time.Second, ReadTimeout: 3 * time.Second, WriteTimeout: 3 * time.Second}
	if server.ListenAndServeTLS("/tls/tls.crt", "/tls/tls.key") != nil {
		panic("fixture server failed")
	}
}
