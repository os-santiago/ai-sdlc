package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

func TestRegistryTransportRejectsExternalRedirectAndBearerRealm(t *testing.T) {
	for _, mode := range []string{"redirect", "realm"} {
		t.Run(mode, func(t *testing.T) {
			var outside atomic.Int32
			foreign := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { outside.Add(1); w.WriteHeader(200) }))
			defer foreign.Close()
			foreignAuthority := "foreign.example.test:" + strings.Split(foreign.Listener.Addr().String(), ":")[1]
			foreignURL := "https://" + foreignAuthority
			internal := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "redirect" {
					http.Redirect(w, r, foreignURL+"/upload", http.StatusTemporaryRedirect)
					return
				}
				w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="fixture"`, foreignURL))
				w.WriteHeader(401)
			}))
			defer internal.Close()
			roots := x509.NewCertPool()
			roots.AddCert(internal.Certificate())
			roots.AddCert(foreign.Certificate())
			tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12, ServerName: "example.com"}}
			defer tr.CloseIdleConnections()
			tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				if address == foreignAuthority {
					address = foreign.Listener.Addr().String()
				}
				return (&net.Dialer{}).DialContext(ctx, network, address)
			}
			host := strings.TrimPrefix(internal.URL, "https://")
			ref, e := name.NewTag(host+"/fixture/image:tag", name.StrictValidation)
			if e != nil {
				t.Fatal(e)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, e = remote.Head(ref, remote.WithContext(ctx), remote.WithTransport(registryTransport{tr, host}), remote.WithAuth(&authn.Basic{Username: "fixture", Password: "fixture-private"}))
			if e == nil {
				t.Fatal("accepted escaping registry exchange")
			}
			if outside.Load() != 0 {
				t.Fatal("registry client contacted external authority")
			}
			// Positive control: the same trusted TLS/DNS fixture reaches the foreign
			// endpoint if the authority guard is removed. This prevents a false pass
			// caused by a dependency rejecting private IP literals or TLS failure.
			_, _ = remote.Head(ref, remote.WithContext(ctx), remote.WithTransport(tr), remote.WithAuth(&authn.Basic{Username: "fixture", Password: "fixture-private"}))
			if outside.Load() == 0 {
				t.Fatal("external endpoint positive control never received a request")
			}

		})
	}
}
func TestRegistryTransportRejectsSchemePortAndUserInfo(t *testing.T) {
	tr := registryTransport{base: http.DefaultTransport, host: registryHost}
	for _, url := range []string{"http://" + registryHost + "/v2/", "https://" + registryHost + ":443/v2/", "https://user@" + registryHost + "/v2/", "https://" + registryHost + ".evil/v2/"} {
		r, e := http.NewRequest("GET", url, nil)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = tr.RoundTrip(r); e == nil {
			t.Fatal("accepted escaped authority")
		}
	}
}
