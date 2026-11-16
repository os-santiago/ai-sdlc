package podtransport

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasing"
	"io"
	"net"
	"net/http"
	"reflect"
	"time"
)

func (c *ConsoleDeployment) WaitReady(ctx context.Context, expected ConsoleSnapshot) (ConsoleRollout, error) {
	if c == nil {
		return ConsoleRollout{}, ErrTransport
	}
	ctx, cancel := context.WithDeadline(ctx, minDeadline(time.Now().Add(time.Duration(c.target.RolloutSeconds)*time.Second), time.UnixMilli(c.target.ExpiresAt)))
	defer cancel()
	for {
		r, ready, e := c.ObserveRollout(ctx, expected)
		if e != nil {
			return ConsoleRollout{}, e
		}
		if ready {
			return r, nil
		}
		timer := time.NewTimer(2 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ConsoleRollout{}, ErrTransport
		case <-timer.C:
		}
	}
}

// VerifyHealth follows the real Service route. It cannot use port-forward,
// response-provided URLs, ambient proxies, redirects, insecure TLS or user data.
// Endpoint and workload identity are checked before and after the smoke; each
// response digest is evidence only and contains no secret response contents.
func (c *ConsoleDeployment) VerifyHealth(ctx context.Context, expected ConsoleSnapshot) (releasing.Health, error) {
	if c == nil {
		return releasing.Health{}, ErrTransport
	}
	ctx, cancel := context.WithDeadline(ctx, minDeadline(time.Now().Add(time.Duration(c.target.SmokeSeconds)*time.Second), time.UnixMilli(c.target.ExpiresAt)))
	defer cancel()
	before, ready, e := c.ObserveRollout(ctx, expected)
	if e != nil || !ready {
		return releasing.Health{}, ErrTransport
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(c.target.TLSCA)) {
		return releasing.Health{}, ErrTransport
	}
	host := c.target.TLSHostname + ":8443"
	destination := net.JoinHostPort(before.ServiceIP, "8443")
	tr := &http.Transport{Proxy: nil, DisableKeepAlives: true, DisableCompression: true, MaxResponseHeaderBytes: 16384, ResponseHeaderTimeout: 10 * time.Second, TLSHandshakeTimeout: 3 * time.Second, TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: c.target.TLSHostname, MinVersion: tls.VersionTLS13, NextProtos: []string{"http/1.1"}}, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" || address != host {
			return nil, ErrTransport
		}
		return (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: -1}).DialContext(ctx, "tcp", destination)
	}, TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{}}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 12 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	hashes := []string{}
	for _, step := range []struct {
		method, path string
		status       int
		body         []byte
	}{{"GET", "/health/live", 204, nil}, {"GET", "/", 200, nil}, {"POST", "/api/demo", 200, []byte("{}")}} {
		req, e := http.NewRequestWithContext(ctx, step.method, "https://"+host+step.path, bytes.NewReader(step.body))
		if e != nil {
			return releasing.Health{}, ErrTransport
		}
		if step.method == "POST" {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Origin", "https://"+host)
		}
		res, e := client.Do(req)
		if e != nil {
			return releasing.Health{}, ErrTransport
		}
		raw, readErr := io.ReadAll(io.LimitReader(res.Body, 65537))
		_ = res.Body.Close()
		if readErr != nil || len(raw) > 65536 || res.StatusCode != step.status || res.Header.Get("Content-Encoding") != "" || res.Header.Get("Content-Security-Policy") != "default-src 'self'; object-src 'none'; frame-ancestors 'none'; base-uri 'none'" || res.Header.Get("X-Content-Type-Options") != "nosniff" || res.Header.Get("Referrer-Policy") != "no-referrer" || res.Header.Get("Cache-Control") != "no-store" || ctx.Err() != nil {
			return releasing.Health{}, ErrTransport
		}
		if step.path == "/health/live" && len(raw) != 0 {
			return releasing.Health{}, ErrTransport
		}
		if step.path == "/" && (!bytes.Contains(raw, []byte("<title>Saferus")) || !bytes.Contains(raw, []byte("Run verification"))) {
			return releasing.Health{}, ErrTransport
		}
		if step.path == "/api/demo" {
			if len(raw) > 4096 || res.Header.Get("Content-Type") != "application/json" || !consoleDemoResult(raw) {
				return releasing.Health{}, ErrTransport
			}
		}
		hashes = append(hashes, authority.Digest(raw))
	}
	after, ready, e := c.ObserveRollout(ctx, expected)
	if e != nil || !ready {
		return releasing.Health{}, ErrTransport
	}
	// Observation timestamps differ; every workload and routing identity must not.
	comparable := after
	comparable.ObservedAt = before.ObservedAt
	if !reflect.DeepEqual(comparable, before) {
		return releasing.Health{}, ErrTransport
	}
	return releasing.Health{Version: "ai-sdlc.saferus-health/v1", Rollout: after, ResponseSHA256: hashes, ObservedAt: time.Now().UnixMilli()}, nil
}
func consoleDemoResult(raw []byte) bool {
	var result struct {
		Verified  bool   `json:"verified"`
		Algorithm string `json:"algorithm"`
		TTL       int    `json:"tokenTTLSeconds"`
		Revoked   bool   `json:"tokenRevoked"`
		Agentless bool   `json:"agentless"`
	}
	if authority.DecodeStrict(raw, &result) != nil || !result.Verified || result.Algorithm != "AES-256-GCM" || !result.Revoked || !result.Agentless || result.TTL < 1 || result.TTL > 60 {
		return false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || len(fields) != 5 {
		return false
	}
	for _, k := range []string{"verified", "algorithm", "tokenTTLSeconds", "tokenRevoked", "agentless"} {
		if _, ok := fields[k]; !ok {
			return false
		}
	}
	return true
}
