package verificationflow

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/podtransport"
)

// Forward to the real API with the restricted fixture identity, then discard
// the successful create reply. This models a lost UID, not a failed create.
func lostConfigReply(t *testing.T, input podtransport.Config) (podtransport.Config, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(input.CA) {
		t.Fatal("fixture CA")
	}
	upstream := &http.Client{Timeout: 8 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}}
	base, err := url.Parse(input.APIServer)
	if err != nil {
		t.Fatal("fixture API")
	}
	creates, deletes := new(atomic.Int32), new(atomic.Int32)
	proxy := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target := *base
		target.Path = r.URL.Path
		target.RawQuery = r.URL.RawQuery
		request, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), r.Body)
		if err != nil {
			w.WriteHeader(502)
			return
		}
		request.Header = r.Header.Clone()
		response, err := upstream.Do(request)
		if err != nil {
			w.WriteHeader(502)
			return
		}
		defer response.Body.Close()
		if r.Method == http.MethodDelete {
			deletes.Add(1)
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/configmaps") && response.StatusCode == http.StatusCreated {
			creates.Add(1)
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 2*1024*1024))
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}
		for k, values := range response.Header {
			for _, v := range values {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, io.LimitReader(response.Body, 2*1024*1024))
	}))
	t.Cleanup(proxy.Close)
	t.Cleanup(upstream.CloseIdleConnections)
	input.APIServer = proxy.URL
	input.CA = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: proxy.Certificate().Raw})
	return input, creates, deletes
}
