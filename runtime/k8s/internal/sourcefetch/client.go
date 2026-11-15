package sourcefetch

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

var repoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var tokenPattern = regexp.MustCompile(`^[A-Za-z0-9_]{20,512}$`)

type Client struct {
	repository, token string
	api, archive      string
	http              *http.Client
}

// New accepts only reviewed installation configuration. The API and archive
// hosts are fixed; a request supplies an immutable SHA, never a URL or branch.
// An empty token supports public repositories. Private-repository support still
// requires validation with a separately scoped read-only credential.
func New(repository, token string) (*Client, error) {
	if len(repository) > 256 || !repoPattern.MatchString(repository) || token != "" && !tokenPattern.MatchString(token) {
		return nil, ErrSource
	}
	for _, part := range strings.Split(repository, "/") {
		if part == "." || part == ".." {
			return nil, ErrSource
		}
	}
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, TLSHandshakeTimeout: 3 * time.Second,
		ResponseHeaderTimeout: 5 * time.Second, MaxResponseHeaderBytes: 8192, DisableCompression: true,
		DisableKeepAlives: true, ForceAttemptHTTP2: false, TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{}}
	return &Client{repository: repository, token: token, api: "https://api.github.com", archive: "https://codeload.github.com",
		http: &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (c *Client) archiveLocation(location, sha string) (string, error) {
	if len(location) == 0 || len(location) > 4096 {
		return "", ErrSource
	}
	u, err := url.Parse(location)
	if err != nil {
		return "", ErrSource
	}
	base, err := url.Parse(c.archive)
	if err != nil {
		return "", ErrSource
	}
	if u.Scheme != base.Scheme || u.Host != base.Host || u.User != nil || u.Fragment != "" || u.RawPath != "" ||
		(u.Path != "/"+c.repository+"/legacy.tar.gz/"+sha && u.Path != "/"+c.repository+"/tar.gz/"+sha) {
		return "", ErrSource
	}
	// GitHub's private archive ticket may appear in this query. Keep it only in
	// memory for this request; neither URL nor response text enters errors.
	return u.String(), nil
}

func (c *Client) Fetch(ctx context.Context, sha string) (workspacebundle.Bundle, error) {
	var err error
	c, err = c.scoped(ctx)
	if err != nil {
		return workspacebundle.Bundle{}, err
	}
	if !workspacebundle.ValidSourceSHA(sha) {
		return workspacebundle.Bundle{}, ErrSource
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	manifest, err := c.manifest(ctx, sha)
	if err != nil {
		return workspacebundle.Bundle{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.api+"/repos/"+c.repository+"/tarball/"+sha, nil)
	if err != nil {
		return workspacebundle.Bundle{}, ErrSource
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2026-03-10")
	if c.token != "" {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return workspacebundle.Bundle{}, ErrSource
	}
	response.Body.Close()
	if response.StatusCode != http.StatusFound {
		return workspacebundle.Bundle{}, httpSourceError(response)
	}
	if len(response.Header.Values("Location")) != 1 {
		return workspacebundle.Bundle{}, ErrSource
	}
	location, err := c.archiveLocation(response.Header.Get("Location"), sha)
	if err != nil {
		return workspacebundle.Bundle{}, ErrSource
	}
	// Construct a fresh request, deliberately without the API Authorization.
	download, err := http.NewRequestWithContext(ctx, http.MethodGet, location, nil)
	if err != nil {
		return workspacebundle.Bundle{}, ErrSource
	}
	response, err = c.http.Do(download)
	if err != nil {
		return workspacebundle.Bundle{}, ErrSource
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return workspacebundle.Bundle{}, httpSourceError(response)
	}
	if response.Header.Get("Content-Encoding") != "" || response.ContentLength > MaxArchiveBytes {
		return workspacebundle.Bundle{}, ErrSource
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, MaxArchiveBytes+1))
	if err != nil || len(raw) > MaxArchiveBytes || ctx.Err() != nil {
		return workspacebundle.Bundle{}, ErrSource
	}
	b, err := DecodeArchive(raw, sha)
	if err != nil || ctx.Err() != nil || manifest.verify(b) != nil {
		return workspacebundle.Bundle{}, ErrSource
	}
	if ctx.Err() != nil {
		return workspacebundle.Bundle{}, ErrSource
	}
	return b, nil
}
