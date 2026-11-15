// Package podtransport provides fixed, bounded helper operations for a trusted
// executor controller. It never authorizes a workflow or runs repository code.
package podtransport

import (
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"time"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/transport"
)

var ErrTransport = errors.New("executor Pod transport failed or exceeded a bound")
var ErrPermission = errors.New("executor Pod transport permission denied")
var tokenPattern = regexp.MustCompile(`^[A-Za-z0-9._~+/=-]+$`)

// Config is trusted provisioning input, never an issue or model parameter.
// Credentials and CA bytes are explicit; there is no kubeconfig, auth plugin,
// ambient proxy, insecure TLS switch or caller-provided transport callback.
type Config struct {
	APIServer                        string
	CA, ClientCertificate, ClientKey []byte
	BearerToken                      string
}

type Client struct {
	base   string
	config *rest.Config
	http   *http.Client
}

func NewClient(c Config) (*Client, error) {
	if len(c.APIServer) == 0 || len(c.APIServer) > 2048 || len(c.CA) == 0 || len(c.CA) > 64*1024 || len(c.ClientCertificate) > 64*1024 || len(c.ClientKey) > 64*1024 {
		return nil, ErrTransport
	}
	u, err := url.Parse(c.APIServer)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, ErrTransport
	}
	certificate := len(c.ClientCertificate) > 0 && len(c.ClientKey) > 0
	if (c.BearerToken != "" && (certificate || len(c.BearerToken) < 32 || len(c.BearerToken) > 8192 || !tokenPattern.MatchString(c.BearerToken))) || (c.BearerToken == "" && !certificate) || ((len(c.ClientCertificate) == 0) != (len(c.ClientKey) == 0)) {
		return nil, ErrTransport
	}
	u.Path = ""
	cfg := &rest.Config{Host: u.String(), BearerToken: c.BearerToken, Timeout: 10 * time.Second,
		Proxy:           func(*http.Request) (*url.URL, error) { return nil, nil },
		TLSClientConfig: rest.TLSClientConfig{CAData: append([]byte(nil), c.CA...), CertData: append([]byte(nil), c.ClientCertificate...), KeyData: append([]byte(nil), c.ClientKey...)},
	}
	transportConfig, err := cfg.TransportConfig()
	if err != nil {
		return nil, ErrTransport
	}
	tlsConfig, err := transport.TLSConfigFor(transportConfig)
	if err != nil || tlsConfig == nil {
		return nil, ErrTransport
	}
	tlsConfig.MinVersion = tls.VersionTLS12
	tlsConfig.NextProtos = []string{"http/1.1"}
	roundTripper := &http.Transport{Proxy: nil, TLSClientConfig: tlsConfig,
		DialContext:         (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: -1}).DialContext,
		TLSHandshakeTimeout: 3 * time.Second, ResponseHeaderTimeout: 5 * time.Second,
		MaxResponseHeaderBytes: 32768, DisableKeepAlives: true, DisableCompression: true,
		TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{},
	}
	return &Client{base: u.String(), config: cfg, http: &http.Client{Transport: roundTripper, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
