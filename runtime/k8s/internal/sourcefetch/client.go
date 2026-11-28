package sourcefetch

import (
	"context"
	"crypto/tls"
	"errors"
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

// Tokens are opaque. GitHub installation tokens now contain a JWT and may
// exceed 512 bytes; reject header delimiters while retaining a finite bound.
var tokenPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

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
	if len(repository) > 256 || !repoPattern.MatchString(repository) || token != "" && (len(token) < 20 || len(token) > 4096 || !tokenPattern.MatchString(token)) {
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

// Fetch attempts are bounded: a transient failure (a network error or a 5xx
// from GitHub) is retried at most fetchAttempts times in total, because a
// pinned, digest-checked read is idempotent and one transient error must not
// stop a delivery stage. A contract violation or a rate limit is never retried.
const fetchAttempts = 3

var retryDelay = func(attempt int) time.Duration { return time.Duration(attempt) * time.Second }

func (c *Client) Fetch(ctx context.Context, sha string) (workspacebundle.Bundle, error) {
	var err error
	c, err = c.scoped(ctx)
	if err != nil {
		return workspacebundle.Bundle{}, err
	}
	if !workspacebundle.ValidSourceSHA(sha) {
		return workspacebundle.Bundle{}, ErrSource
	}
	for attempt := 1; ; attempt++ {
		b, err, transient := c.fetchOnce(ctx, sha)
		if err == nil || !transient || attempt == fetchAttempts {
			return b, err
		}
		select {
		case <-ctx.Done():
			return workspacebundle.Bundle{}, ErrSource
		case <-time.After(retryDelay(attempt)):
		}
	}
}

// networkFailure reports a connection-level failure before a response (dial,
// reset, timeout, connection closed before the headers). TLS certificate,
// alert and record failures and protocol violations are not retried; a
// truncated response body is a contract failure, never retried.
func networkFailure(err error) bool {
	var certificate *tls.CertificateVerificationError
	var alert tls.AlertError
	var record tls.RecordHeaderError
	if errors.As(err, &certificate) || errors.As(err, &alert) || errors.As(err, &record) {
		return false
	}
	var op *net.OpError
	var timeout interface{ Timeout() bool }
	return errors.As(err, &op) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) ||
		(errors.As(err, &timeout) && timeout.Timeout())
}

// transientError reports a failure worth one more bounded attempt.
func transientError(err error) bool {
	var unavailable AvailabilityError
	return errors.As(err, &unavailable) && !unavailable.rateLimited && unavailable.serverError
}

func (c *Client) fetchOnce(ctx context.Context, sha string) (workspacebundle.Bundle, error, bool) {
	b, err, network := c.fetchAttempt(ctx, sha)
	return b, err, err != nil && (network || transientError(err))
}

// archiveURL returns the pinned archive location. An anonymous reader goes to
// the fixed archive host directly: the API redirect would spend one more call
// of the small anonymous API quota and confers no trust, because the archive
// is verified against the commit's tree manifest either way. An authenticated
// reader asks the API, whose redirect carries the private-archive ticket.
func (c *Client) archiveURL(ctx context.Context, sha string) (string, error) {
	if c.token == "" {
		return c.archiveLocation(c.archive+"/"+c.repository+"/legacy.tar.gz/"+sha, sha)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.api+"/repos/"+c.repository+"/tarball/"+sha, nil)
	if err != nil {
		return "", ErrSource
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2026-03-10")
	request.Header.Set("Authorization", "Bearer "+c.token)
	response, err := c.http.Do(request)
	if err != nil {
		if ctx.Err() == nil && networkFailure(err) {
			return "", errNetwork
		}
		return "", ErrSource
	}
	if response.StatusCode != http.StatusFound {
		// Classify before closing: a secondary rate limit may be named only
		// in the response body.
		err := c.responseError(response)
		response.Body.Close()
		return "", err
	}
	response.Body.Close()
	if len(response.Header.Values("Location")) != 1 {
		return "", ErrSource
	}
	return c.archiveLocation(response.Header.Get("Location"), sha)
}

func (c *Client) fetchAttempt(ctx context.Context, sha string) (workspacebundle.Bundle, error, bool) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	manifest, err := c.manifest(ctx, sha)
	if err != nil {
		return workspacebundle.Bundle{}, err, errors.Is(err, errNetwork)
	}
	location, err := c.archiveURL(ctx, sha)
	if err != nil {
		return workspacebundle.Bundle{}, err, errors.Is(err, errNetwork)
	}
	// Construct a fresh request, deliberately without the API Authorization.
	download, err := http.NewRequestWithContext(ctx, http.MethodGet, location, nil)
	if err != nil {
		return workspacebundle.Bundle{}, ErrSource, false
	}
	response, err := c.http.Do(download)
	if err != nil {
		return workspacebundle.Bundle{}, ErrSource, ctx.Err() == nil && networkFailure(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return workspacebundle.Bundle{}, httpSourceError(response), false
	}
	if response.Header.Get("Content-Encoding") != "" || response.ContentLength > MaxArchiveBytes {
		return workspacebundle.Bundle{}, ErrSource, false
	}
	// Decoded as it arrives: neither the compressed archive nor its tar
	// stream is held whole, only the bundle being built.
	b, err := DecodeArchiveFrom(response.Body, sha)
	if err != nil || ctx.Err() != nil || manifest.verify(b) != nil {
		return workspacebundle.Bundle{}, ErrSource, false
	}
	if ctx.Err() != nil {
		return workspacebundle.Bundle{}, ErrSource, false
	}
	return b, nil, false
}
