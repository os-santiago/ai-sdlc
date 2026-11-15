// Package modelbroker coordinates a bounded provider call and mailbox handoff.
// It has no public route, workload controller, operational role or live wiring.
package modelbroker

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"time"
	"unicode/utf8"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelmailbox"
)

var ErrProvider = errors.New("model provider request failed or exceeded a bound")
var bearerPattern = regexp.MustCompile(`^[A-Za-z0-9._~+/=-]{32,512}$`)

// ProviderConfig comes only from reviewed out-of-band configuration. A request
// cannot supply an endpoint, credential, proxy, redirect or timeout override.
type ProviderConfig struct {
	Endpoint, BearerToken string
	AllowLoopbackHTTP     bool
}
type Provider struct {
	endpoint, token string
	client          *http.Client
}

func NewProvider(config ProviderConfig) (*Provider, error) {
	if len(config.Endpoint) == 0 || len(config.Endpoint) > 2048 {
		return nil, ErrProvider
	}
	u, err := url.Parse(config.Endpoint)
	if err != nil || u.User != nil || u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || u.Path != "/v1/chat/completions" || !bearerPattern.MatchString(config.BearerToken) {
		return nil, ErrProvider
	}
	if u.Scheme != "https" {
		ip := net.ParseIP(u.Hostname())
		if u.Scheme != "http" || !config.AllowLoopbackHTTP || ip == nil || !ip.IsLoopback() {
			return nil, ErrProvider
		}
	}
	transport := &http.Transport{
		Proxy: nil, DisableKeepAlives: true, DisableCompression: true,
		DialContext:         (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: -1}).DialContext,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}},
		TLSNextProto:        map[string]func(string, *tls.Conn) http.RoundTripper{},
		// The supervised completion adapter buffers its result for up to 24s.
		// Keep a bounded margin for its response; the entire request still has
		// a 30s ceiling and an earlier caller deadline always takes precedence.
		TLSHandshakeTimeout: 3 * time.Second, ResponseHeaderTimeout: 25 * time.Second,
		MaxResponseHeaderBytes: 8192,
	}
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &Provider{endpoint: u.String(), token: config.BearerToken, client: client}, nil
}

// Fresh HTTP/1 connections, no redirect following, no proxy and no replayable
// request body prevent transport retries. There is exactly one Do call here;
// this does not prove that a failed connection reached or did not reach a server.
func (p *Provider) invoke(ctx context.Context, request []byte, responseLimit int) ([]byte, error) {
	if p == nil || p.client == nil || len(request) == 0 || len(request) > modelmailbox.MaxRequestBytes || responseLimit < 1 || responseLimit > modelmailbox.MaxResponseBytes {
		return nil, ErrProvider
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(request))
	if err != nil {
		return nil, ErrProvider
	}
	req.GetBody = nil
	req.Close = true
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	response, err := p.client.Do(req)
	if err != nil {
		return nil, ErrProvider
	}
	defer response.Body.Close()
	media, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if response.StatusCode != http.StatusOK || err != nil || media != "application/json" {
		return nil, ErrProvider
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, int64(responseLimit)+1))
	if err != nil || len(raw) > responseLimit || !utf8.Valid(raw) || !json.Valid(raw) {
		return nil, ErrProvider
	}
	var completion struct {
		Choices []struct {
			Message struct {
				Role string `json:"role"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(raw, &completion) != nil || len(completion.Choices) == 0 || len(completion.Choices) > 8 {
		return nil, ErrProvider
	}
	for _, choice := range completion.Choices {
		if choice.Message.Role != "assistant" {
			return nil, ErrProvider
		}
	}
	return raw, nil
}
