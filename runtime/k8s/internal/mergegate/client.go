// Package mergegate collects bounded authenticated forge observations. It has
// no write operation, HTTP handler or authority to merge/release/deploy.
package mergegate

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/forge"
)

var ErrGate = errors.New("merge gate observation unavailable or denied")

const repository = "os-santiago/homedir"

type Client struct {
	token, base string
	http        *http.Client
}

func New(credential forge.Credential) (*Client, error) {
	if credential.Repository != repository {
		return nil, ErrGate
	}
	if _, e := forge.New(credential, repository); e != nil {
		return nil, ErrGate
	}
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, DisableCompression: true,
		DialContext:         (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: -1}).DialContext,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}},
		TLSNextProto:        map[string]func(string, *tls.Conn) http.RoundTripper{},
		TLSHandshakeTimeout: 3 * time.Second, ResponseHeaderTimeout: 5 * time.Second, MaxResponseHeaderBytes: 8192}
	return &Client{credential.Token, "https://api.github.com", &http.Client{Transport: transport, Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// A GraphQL POST below carries a fixed query, never a mutation. Public entry
// points expose no caller-controlled URL/query. More than one page fails closed;
// no unseen review, check or ruleset is silently omitted from a ready result.
func (c *Client) read(ctx context.Context, path string, body []byte, out any) error {
	method := http.MethodGet
	if body != nil {
		method = http.MethodPost
	}
	req, e := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
	if e != nil {
		return ErrGate
	}
	req.GetBody = nil
	req.Close = true
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, e := c.http.Do(req)
	if e != nil {
		return ErrGate
	}
	defer response.Body.Close()
	if response.StatusCode != 200 || response.ContentLength > 2*1024*1024 || response.Header.Get("Content-Encoding") != "" || response.Header.Get("Link") != "" {
		return ErrGate
	}
	raw, e := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024+1))
	if e != nil || len(raw) == 0 || len(raw) > 2*1024*1024 || !utf8.Valid(raw) || ctx.Err() != nil {
		return ErrGate
	}
	var untyped any
	if authority.DecodeStrict(raw, &untyped) != nil || json.Unmarshal(raw, out) != nil {
		return ErrGate
	}
	return nil
}
func repoPath(suffix string) string { return "/repos/" + repository + suffix }
func number(n int) string           { return strconv.Itoa(n) }
