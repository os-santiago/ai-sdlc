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
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelmailbox"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/metrics"
)

var ErrProvider = errors.New("model provider request failed or exceeded a bound")

// CallSecondsHeader carries the caller's remaining wait to the loopback gateway.
const CallSecondsHeader = "X-AI-SDLC-Call-Seconds"

// MaxProviderTimeoutSeconds bounds an installation-selected completion timeout.
const MaxProviderTimeoutSeconds = 600

// GatewayTimeoutSeconds reads the installation's optional completion timeout.
// Empty keeps the default; any other invalid value is refused.
func GatewayTimeoutSeconds(value string) (int, error) {
	if value == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 30 || n > MaxProviderTimeoutSeconds || strconv.Itoa(n) != value {
		return 0, ErrProvider
	}
	return n, nil
}

var bearerPattern = regexp.MustCompile(`^[A-Za-z0-9._~+/=-]{32,512}$`)

// ProviderConfig comes only from reviewed out-of-band configuration. A request
// cannot supply an endpoint, credential, proxy, redirect or timeout override.
type ProviderConfig struct {
	Endpoint, BearerToken string
	AllowLoopbackHTTP     bool
	// TimeoutSeconds bounds one completion call (0 keeps the 30 s Codex-sized
	// ceiling). Installation-only; omitted from configuration digests when 0.
	TimeoutSeconds int `json:",omitempty"`
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
	total := 30 * time.Second
	if config.TimeoutSeconds != 0 {
		if config.TimeoutSeconds < 30 || config.TimeoutSeconds > MaxProviderTimeoutSeconds {
			return nil, ErrProvider
		}
		total = time.Duration(config.TimeoutSeconds) * time.Second
	}
	transport := &http.Transport{
		Proxy: nil, DisableKeepAlives: true, DisableCompression: true,
		DialContext:     (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: -1}).DialContext,
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}},
		TLSNextProto:    map[string]func(string, *tls.Conn) http.RoundTripper{},
		// The supervised completion adapter buffers its result for up to 24s.
		// Keep a bounded margin for its response; the entire request still has
		// a 30s ceiling and an earlier caller deadline always takes precedence.
		TLSHandshakeTimeout: 3 * time.Second, ResponseHeaderTimeout: total - 5*time.Second,
		MaxResponseHeaderBytes: 8192,
	}
	client := &http.Client{Transport: transport, Timeout: total, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &Provider{endpoint: u.String(), token: config.BearerToken, client: client}, nil
}

// Fresh HTTP/1 connections, no redirect following, no proxy and no replayable
// request body prevent transport retries. There is exactly one Do call here;
// this does not prove that a failed connection reached or did not reach a server.
func (p *Provider) invoke(ctx context.Context, request []byte, responseLimit int) ([]byte, error) {
	raw, _, err := p.call(ctx, request, responseLimit)
	return raw, err
}

// observedInvoke is invoke, counted in the model call metrics under role
// with only its bounded outcome class and duration.
func (p *Provider) observedInvoke(ctx context.Context, role string, request []byte, responseLimit int) ([]byte, error) {
	started := time.Now()
	raw, outcome, err := p.call(ctx, request, responseLimit)
	metrics.ModelCall(role, outcome, time.Since(started))
	return raw, err
}

// failedTransfer classifies a failed request or body read: an explicit
// cancellation of the caller's context, the caller's deadline or a transport
// timeout, otherwise a transport failure.
func failedTransfer(ctx context.Context, err error) string {
	if errors.Is(ctx.Err(), context.Canceled) {
		return metrics.ModelCanceled
	}
	var timeout interface{ Timeout() bool }
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()) {
		return metrics.ModelTimeout
	}
	return metrics.ModelTransport
}

func statusClass(code int) string {
	switch {
	case code >= 400 && code < 500:
		return metrics.ModelStatus4xx
	case code >= 500 && code < 600:
		return metrics.ModelStatus5xx
	}
	return metrics.ModelStatusOther
}

// call is one bounded gateway request. Its outcome is a fixed class from
// internal/platform/metrics, never response or error text.
func (p *Provider) call(ctx context.Context, request []byte, responseLimit int) ([]byte, string, error) {
	if p == nil || p.client == nil || len(request) == 0 || len(request) > modelmailbox.MaxRequestBytes || responseLimit < 1 || responseLimit > modelmailbox.MaxResponseBytes {
		return nil, metrics.ModelRejected, ErrProvider
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(request))
	if err != nil {
		return nil, metrics.ModelRejected, ErrProvider
	}
	req.GetBody = nil
	req.Close = true
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	// The gateway bounds its upstream call by the time this caller will still
	// wait, so it never keeps working (and holding its single model slot)
	// after the broker has given up on the call.
	if deadline, ok := ctx.Deadline(); ok {
		if seconds := int(time.Until(deadline)/time.Second) - 1; seconds >= 1 {
			req.Header.Set(CallSecondsHeader, strconv.Itoa(seconds))
		}
	}
	response, err := p.client.Do(req)
	if err != nil {
		return nil, failedTransfer(ctx, err), ErrProvider
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, statusClass(response.StatusCode), ErrProvider
	}
	media, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return nil, metrics.ModelInvalidResponse, ErrProvider
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, int64(responseLimit)+1))
	if err != nil {
		return nil, failedTransfer(ctx, err), ErrProvider
	}
	if len(raw) > responseLimit || !utf8.Valid(raw) || !json.Valid(raw) {
		return nil, metrics.ModelInvalidResponse, ErrProvider
	}
	var completion struct {
		Choices []struct {
			Message struct {
				Role string `json:"role"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(raw, &completion) != nil || len(completion.Choices) == 0 || len(completion.Choices) > 8 {
		return nil, metrics.ModelInvalidResponse, ErrProvider
	}
	for _, choice := range completion.Choices {
		if choice.Message.Role != "assistant" {
			return nil, metrics.ModelInvalidResponse, ErrProvider
		}
	}
	return raw, metrics.ModelSuccess, nil
}

// GatewayProfile reads the per-call timeout the loopback gateway derived from
// its startup benchmark. It is available only after the gateway is ready, and
// the caller must still bound it by the installation ceiling.
func (p *Provider) GatewayProfile(ctx context.Context) (int, error) {
	if p == nil || !strings.HasPrefix(p.endpoint, "http://127.0.0.1:") {
		return 0, ErrProvider
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(p.endpoint, "/v1/chat/completions")+"/v1/gateway/profile", nil)
	if e != nil {
		return 0, ErrProvider
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	response, e := p.client.Do(req)
	if e != nil {
		return 0, ErrProvider
	}
	defer response.Body.Close()
	raw, e := io.ReadAll(io.LimitReader(response.Body, 65))
	if e != nil || response.StatusCode != 200 || len(raw) > 64 {
		return 0, ErrProvider
	}
	var profile struct {
		TimeoutSeconds int `json:"timeoutSeconds"`
	}
	if authority.DecodeStrict(raw, &profile) != nil || profile.TimeoutSeconds < 30 || profile.TimeoutSeconds > MaxProviderTimeoutSeconds-30 {
		return 0, ErrProvider
	}
	return profile.TimeoutSeconds, nil
}

// CheckGateway checks only the trusted loopback sidecar. It neither reserves
// nor invokes a model completion, and never follows redirects.
func (p *Provider) CheckGateway(ctx context.Context) error {
	if p == nil || !strings.HasPrefix(p.endpoint, "http://127.0.0.1:") {
		return ErrProvider
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(p.endpoint, "/v1/chat/completions")+"/readyz", nil)
	if e != nil {
		return ErrProvider
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	response, e := p.client.Do(req)
	if e != nil {
		return ErrProvider
	}
	defer response.Body.Close()
	raw, e := io.ReadAll(io.LimitReader(response.Body, 65))
	if e != nil || response.StatusCode != 200 || len(raw) > 64 {
		return ErrProvider
	}
	var ready map[string]bool
	if authority.DecodeStrict(raw, &ready) != nil || len(ready) != 1 || !ready["ready"] {
		return ErrProvider
	}
	return nil
}
