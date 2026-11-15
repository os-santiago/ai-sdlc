package sourcefetch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

func cachedFixture(sha string) workspacebundle.Bundle {
	return workspacebundle.Bundle{Version: workspacebundle.Version, SourceSHA: sha, Files: []workspacebundle.File{{Path: "README.md", Mode: 0644, Data: []byte("verified")}}}
}
func TestCacheSerializesImmutableReadsAndCopiesResults(t *testing.T) {
	c := NewCache()
	var calls atomic.Int32
	c.fetch = func(_ context.Context, _, sha string) (workspacebundle.Bundle, error) {
		calls.Add(1)
		return cachedFixture(sha), nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b, e := c.Fetch(context.Background(), "fixture/product", testSHA)
			if e != nil {
				t.Error(e)
				return
			}
			b.Files[0].Data[0] = 'X'
		}()
	}
	wg.Wait()
	b, e := c.Fetch(context.Background(), "fixture/product", testSHA)
	if e != nil || calls.Load() != 1 || string(b.Files[0].Data) != "verified" {
		t.Fatal("cache replayed a fetch or exposed mutable storage")
	}
	if _, e = c.Fetch(context.Background(), "fixture/other", testSHA); e != nil || calls.Load() != 2 {
		t.Fatal("repository was not part of cache identity")
	}
	if _, e = c.Fetch(context.Background(), "fixture/other", strings.Repeat("b", 40)); e != nil || calls.Load() != 3 {
		t.Fatal("SHA was not part of cache identity")
	}
}
func TestHTTPRateLimitDefersAcrossRepositoriesWithoutLeakingBody(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Error("anonymous source sent credentials")
		}
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(10*time.Minute).Unix(), 10))
		w.WriteHeader(403)
		w.Write([]byte("provider-sensitive-body"))
	}))
	defer server.Close()
	client, _ := New("fixture/product", "")
	client.api = server.URL
	client.http = server.Client()
	c := NewCache()
	c.fetch = func(ctx context.Context, _, sha string) (workspacebundle.Bundle, error) {
		return client.Fetch(ctx, sha)
	}
	for _, repo := range []string{"fixture/product", "fixture/other"} {
		_, e := c.Fetch(context.Background(), repo, testSHA)
		if !RateLimited(e) || !errors.Is(e, ErrSource) || strings.Contains(e.Error(), "sensitive") {
			t.Fatal("rate error lost bounded classification")
		}
	}
	if calls.Load() != 1 || time.Until(c.retryAt) < 9*time.Minute {
		t.Fatal("reset did not suppress subsequent HTTP requests")
	}
	c.retryAt = time.Now().Add(-time.Second)
	_, _ = c.Fetch(context.Background(), "fixture/product", testSHA)
	if calls.Load() != 2 {
		t.Fatal("cache never retried after backoff")
	}
}
func TestCacheKeepsPreviouslyVerifiedSourceDuringOtherSourceQuota(t *testing.T) {
	c := NewCache()
	calls := 0
	c.fetch = func(_ context.Context, _, sha string) (workspacebundle.Bundle, error) {
		calls++
		if sha != testSHA {
			return workspacebundle.Bundle{}, AvailabilityError{time.Now().Add(time.Minute), true}
		}
		return cachedFixture(sha), nil
	}
	_, _ = c.Fetch(context.Background(), "fixture/product", testSHA)
	_, _ = c.Fetch(context.Background(), "fixture/product", strings.Repeat("b", 40))
	if _, e := c.Fetch(context.Background(), "fixture/product", testSHA); e != nil || calls != 2 {
		t.Fatal("verified immutable source lost during quota backoff")
	}
}
func TestCacheCancellationAndUnverifiedSourceCannotPopulate(t *testing.T) {
	c := NewCache()
	calls := 0
	c.fetch = func(_ context.Context, _, sha string) (workspacebundle.Bundle, error) {
		calls++
		b := cachedFixture(sha)
		b.Files[0].Path = "../escape"
		return b, nil
	}
	if _, e := c.Fetch(context.Background(), "fixture/product", testSHA); e == nil || c.raw != nil {
		t.Fatal("invalid bundle cached")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := c.Fetch(ctx, "fixture/product", testSHA); e == nil || calls != 1 {
		t.Fatal("canceled read reached provider")
	}
	<-c.gate
	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, e := c.Fetch(ctx, "fixture/product", testSHA); e == nil {
		t.Fatal("waiting reader ignored cancellation")
	}
	c.gate <- struct{}{}
}
func TestHTTPBackoffHeadersStayBounded(t *testing.T) {
	for _, retry := range []string{"-1", "999999999999", "invalid", "5"} {
		r := &http.Response{StatusCode: 429, Header: make(http.Header)}
		r.Header.Set("Retry-After", retry)
		e := httpSourceError(r).(AvailabilityError)
		remaining := time.Until(e.retryAt)
		if !e.rateLimited || remaining < 4*time.Second || remaining > time.Hour {
			t.Fatal("unbounded provider backoff")
		}
	}
	r := &http.Response{StatusCode: 503, Header: make(http.Header)}
	if RateLimited(httpSourceError(r)) {
		t.Fatal("ordinary outage classified as quota")
	}
}
