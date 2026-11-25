package sourcefetch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

// AvailabilityError contains only bounded scheduling metadata, never a URL,
// provider body, repository content or credential. It is not execution authority.
type AvailabilityError struct {
	retryAt     time.Time
	rateLimited bool
	serverError bool
}

func (e AvailabilityError) Error() string { return "public source temporarily unavailable" }
func (e AvailabilityError) Unwrap() error { return ErrSource }
func RateLimited(err error) bool          { var e AvailabilityError; return errors.As(err, &e) && e.rateLimited }

// responseError classifies a failed API response. With a credential, HTTP 401
// and a 403 that is not a rate limit mean the forge refused the credential: a
// named, non-retried stop, never a fallback to an anonymous read.
func (c *Client) responseError(response *http.Response) error {
	err := httpSourceError(response)
	if c.token != "" && !RateLimited(err) && (response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden) {
		return errCredentialRejected
	}
	return err
}

// errNetwork marks a request that failed before any HTTP response.
var errNetwork = fmt.Errorf("source request did not complete: %w", ErrSource)

// maxErrorBodyBytes bounds the error body read to recognize a secondary
// rate limit. The body is only searched for one fixed phrase: it is never
// logged, returned or stored.
const maxErrorBodyBytes = 4096

func secondaryRateLimit(response *http.Response) bool {
	if response.Body == nil {
		return false
	}
	raw, _ := io.ReadAll(io.LimitReader(response.Body, maxErrorBodyBytes))
	return bytes.Contains(bytes.ToLower(raw), []byte("secondary rate limit"))
}

func httpSourceError(response *http.Response) error {
	now := time.Now()
	retry := now.Add(10 * time.Second)
	// GitHub reports both its primary limit (remaining quota 0) and its
	// secondary limit (a Retry-After, or only the phrase in the message) as
	// 403 or 429.
	limited := response.StatusCode == 429 || response.StatusCode == 403 &&
		(response.Header.Get("X-RateLimit-Remaining") == "0" || response.Header.Get("Retry-After") != "" || secondaryRateLimit(response))
	if limited {
		retry = now.Add(time.Minute)
		if seconds, e := strconv.ParseInt(response.Header.Get("Retry-After"), 10, 64); e == nil && seconds >= 5 && seconds <= 3600 {
			retry = now.Add(time.Duration(seconds) * time.Second)
		}
		if reset, e := strconv.ParseInt(response.Header.Get("X-RateLimit-Reset"), 10, 64); e == nil {
			at := time.Unix(reset, 0)
			if at.After(now.Add(5*time.Second)) && at.Before(now.Add(time.Hour)) {
				retry = at.Add(time.Second)
			}
		}
	}
	return AvailabilityError{retryAt: retry, rateLimited: limited, serverError: response.StatusCode >= 500 && response.StatusCode <= 599}
}

// Cache owns at most one verified immutable source bundle (bounded by the bundle
// codec). It stores no credential and confers no claim. Copies prevent a caller
// from modifying a later result. Availability backoff suppresses repeated reads;
// restarting never resumes or replays an execution/model attempt.
type Cache struct {
	gate                        chan struct{}
	repository, sha             string
	raw                         []byte
	failedRepository, failedSHA string
	failure                     error
	retryAt                     time.Time
	fetch                       func(context.Context, string, string) (workspacebundle.Bundle, error)
}

func NewCache() *Cache {
	c := &Cache{gate: make(chan struct{}, 1)}
	c.gate <- struct{}{}
	c.fetch = func(ctx context.Context, repository, sha string) (workspacebundle.Bundle, error) {
		client, e := New(repository, "")
		if e != nil {
			return workspacebundle.Bundle{}, e
		}
		return client.Fetch(ctx, sha)
	}
	return c
}
func (c *Cache) Fetch(ctx context.Context, repository, sha string) (workspacebundle.Bundle, error) {
	if c == nil || c.gate == nil || !workspacebundle.ValidSourceSHA(sha) {
		return workspacebundle.Bundle{}, ErrSource
	}
	if _, e := New(repository, ""); e != nil {
		return workspacebundle.Bundle{}, e
	}
	select {
	case <-ctx.Done():
		return workspacebundle.Bundle{}, ErrSource
	case <-c.gate:
	}
	defer func() { c.gate <- struct{}{} }()
	if ctx.Err() != nil {
		return workspacebundle.Bundle{}, ErrSource
	}
	if c.repository == repository && c.sha == sha && c.raw != nil {
		return workspacebundle.Decode(c.raw)
	}
	if time.Now().Before(c.retryAt) && (RateLimited(c.failure) || c.failedRepository == repository && c.failedSHA == sha) {
		return workspacebundle.Bundle{}, c.failure
	}
	bundle, e := c.fetch(ctx, repository, sha)
	if e != nil {
		c.failedRepository, c.failedSHA, c.failure = repository, sha, e
		c.retryAt = time.Now().Add(10 * time.Second)
		var unavailable AvailabilityError
		if errors.As(e, &unavailable) && unavailable.retryAt.After(time.Now()) && unavailable.retryAt.Before(time.Now().Add(2*time.Hour)) {
			c.retryAt = unavailable.retryAt
		}
		return workspacebundle.Bundle{}, e
	}
	if ctx.Err() != nil || bundle.SourceSHA != sha || bundle.Validate() != nil {
		return workspacebundle.Bundle{}, ErrSource
	}
	raw, e := workspacebundle.Encode(bundle)
	if e != nil {
		return workspacebundle.Bundle{}, ErrSource
	}
	c.repository, c.sha, c.raw = repository, sha, raw
	c.failure = nil
	c.retryAt = time.Time{}
	return workspacebundle.Decode(c.raw)
}
