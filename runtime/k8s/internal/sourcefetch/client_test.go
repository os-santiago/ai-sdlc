package sourcefetch

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestActualTLSArchiveBoundarySeparatesCredentials(t *testing.T) {
	raw := archiveFixture(t, nil)
	token := "ghs_15368_" + strings.Repeat("a", 200) + "." + strings.Repeat("b", 240) + "." + strings.Repeat("c", 86)
	var apiCalls, archiveCalls atomic.Int32
	var mode atomic.Int32
	archive := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		archiveCalls.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Error("API credential reached archive")
		}
		if r.URL.Path != "/fixture/product/legacy.tar.gz/"+testSHA {
			t.Error("wrong archive path")
		}
		if mode.Load() == 2 {
			w.Header().Set("Location", "https://other.invalid/")
			w.WriteHeader(302)
			return
		}
		if mode.Load() == 3 {
			w.Header().Set("Content-Length", "33554433")
			w.WriteHeader(200)
			return
		}
		if mode.Load() == 4 {
			w.Header().Set("Content-Encoding", "gzip")
			w.WriteHeader(200)
			return
		}
		w.Write(raw)
	}))
	defer archive.Close()
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiCalls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("missing API authentication")
		}
		if r.URL.Path == "/repos/fixture/product/git/commits/"+testSHA {
			json.NewEncoder(w).Encode(map[string]any{"sha": testSHA, "tree": map[string]string{"sha": "4b825dc642cb6eb9a060e54bf8d69288fbee4904"}})
			return
		}
		if r.URL.Path == "/repos/fixture/product/git/trees/4b825dc642cb6eb9a060e54bf8d69288fbee4904" {
			if r.URL.RawQuery != "recursive=1" {
				t.Error("recursive tree required")
			}
			json.NewEncoder(w).Encode(map[string]any{"sha": "4b825dc642cb6eb9a060e54bf8d69288fbee4904", "tree": []any{}, "truncated": false})
			return
		}
		if r.URL.Path != "/repos/fixture/product/tarball/"+testSHA {
			t.Error("wrong pinned API path")
		}
		if mode.Load() == 1 {
			w.Header().Set("Location", "https://other.invalid/")
		} else {
			w.Header().Set("Location", archive.URL+"/fixture/product/legacy.tar.gz/"+testSHA)
		}
		w.WriteHeader(302)
	}))
	defer api.Close()
	c, err := New("fixture/product", token)
	if err != nil {
		t.Fatal(err)
	}
	c.api = api.URL
	c.archive = archive.URL
	c.http.Transport.(*http.Transport).TLSClientConfig.RootCAs = api.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	if _, err = c.Fetch(context.Background(), testSHA); err != nil {
		t.Fatal(err)
	}
	if apiCalls.Load() != 3 || archiveCalls.Load() != 1 {
		t.Fatal("unexpected HTTP attempts")
	}
	// Context credentials authenticate only API reads and never mutate a reused
	// anonymous client or reach the independently constructed archive request.
	anonymous := *c
	anonymous.token = ""
	scoped, err := WithReadCredential(context.Background(), "fixture/product", token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = anonymous.Fetch(scoped, testSHA); err != nil {
		t.Fatal(err)
	}
	if anonymous.token != "" || apiCalls.Load() != 6 || archiveCalls.Load() != 2 {
		t.Fatal("scoped credential isolation")
	}
	beforeScope := apiCalls.Load()
	wrong, _ := WithReadCredential(context.Background(), "other/product", token)
	conflict, _ := WithReadCredential(context.Background(), "fixture/product", strings.Repeat("y", 32))
	for _, ctx := range []context.Context{wrong, conflict} {
		if _, err = c.Fetch(ctx, testSHA); err == nil {
			t.Fatal("conflicting credential accepted")
		}
	}
	canceled, cancel := context.WithCancel(scoped)
	cancel()
	if _, err = anonymous.Fetch(canceled, testSHA); err == nil || apiCalls.Load() != beforeScope {
		t.Fatal("invalid context reached API")
	}

	for _, variant := range []int32{1, 2, 3, 4} {
		mode.Store(variant)
		before := archiveCalls.Load()
		if _, err = c.Fetch(context.Background(), testSHA); err == nil {
			t.Fatalf("unsafe mode %d accepted", variant)
		}
		if variant == 1 && archiveCalls.Load() != before {
			t.Fatal("untrusted redirect was followed")
		}
	}
	before := apiCalls.Load()
	if _, err = c.Fetch(context.Background(), "main"); err == nil || apiCalls.Load() != before {
		t.Fatal("mutable branch reached API")
	}
}

func TestArchiveLocationRejectsScopeAndURLConfusion(t *testing.T) {
	c, _ := New("fixture/product", "")
	good := "https://codeload.github.com/fixture/product/legacy.tar.gz/" + testSHA
	if _, err := c.archiveLocation(good+"?ticket=private-fixture", testSHA); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{
		strings.Replace(good, "https:", "http:", 1), strings.Replace(good, "codeload.github.com", "codeload.github.com.evil.invalid", 1),
		strings.Replace(good, "codeload.github.com", "user@codeload.github.com", 1), good + "#fragment",
		strings.Replace(good, "fixture/product", "other/product", 1), strings.Replace(good, testSHA, "main", 1),
		strings.Replace(good, "/fixture/", "/%66ixture/", 1), good + strings.Repeat("x", 4096),
	} {
		if _, err := c.archiveLocation(value, testSHA); err == nil {
			t.Fatal("unsafe redirect accepted")
		}
	}
}

func TestArchiveHTTPDeadlineAndTLSVerification(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	c, _ := New("fixture/product", "")
	c.api = server.URL
	if _, err := c.Fetch(context.Background(), testSHA); err == nil {
		t.Fatal("untrusted TLS certificate accepted")
	}
	c.http.Transport.(*http.Transport).TLSClientConfig.RootCAs = server.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := c.Fetch(ctx, testSHA); err == nil {
		t.Fatal("deadline ignored")
	}
	if time.Since(started) > time.Second {
		t.Fatal("deadline was not bounded")
	}
}

func TestActualPublicGitHubPinnedSource(t *testing.T) {
	if os.Getenv("AI_SDLC_TEST_GITHUB_SOURCE") != "1" {
		t.Skip("explicit public GitHub read validation required")
	}
	c, err := New("os-santiago/homedir", "")
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	b, err := c.Fetch(context.Background(), testSHA)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := b.TreeDigest()
	if err != nil || digest != "a392d4d3be50cc89cb1cc324fb736952fe9e86b47e5d131912163d363c147277" || len(b.Files) != 1158 {
		t.Fatal("actual GitHub archive differs from independent pinned source")
	}
	t.Logf("actual public source: files=%d tree=%s seconds=%.3f", len(b.Files), digest, time.Since(started).Seconds())
}

func TestManifestHTTPFailureIsBoundedAndOnlyTransientFailuresRetry(t *testing.T) {
	retryDelay = func(int) time.Duration { return time.Millisecond }
	t.Cleanup(func() { retryDelay = func(attempt int) time.Duration { return time.Duration(attempt) * time.Second } })
	for _, mode := range []string{"server-error", "rate-limited", "ambiguous-json", "oversized-header"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if mode == "server-error" {
					w.WriteHeader(503)
					w.Write([]byte("private-fixture-response"))
					return
				}
				if mode == "rate-limited" {
					w.WriteHeader(429)
					return
				}
				if mode == "oversized-header" {
					w.Header().Set("X-Large", strings.Repeat("x", 16*1024))
				}
				w.Write([]byte(`{"sha":"` + testSHA + `","SHA":"` + testSHA + `","tree":{"sha":"4b825dc642cb6eb9a060e54bf8d69288fbee4904"}}`))
			}))
			defer server.Close()
			c, _ := New("fixture/product", "")
			c.api = server.URL
			c.http.Transport.(*http.Transport).TLSClientConfig.RootCAs = server.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
			_, err := c.Fetch(context.Background(), testSHA)
			want := int32(1)
			if mode == "server-error" {
				want = fetchAttempts // a transient 5xx is retried, boundedly
			}
			if !errors.Is(err, ErrSource) || strings.Contains(err.Error(), "private-fixture-response") || calls.Load() != want {
				t.Fatalf("failure=%v calls=%d", err, calls.Load())
			}
		})
	}
}

func TestATransientServerErrorIsRetriedAndAContractFailureIsNot(t *testing.T) {
	retryDelay = func(int) time.Duration { return time.Millisecond }
	t.Cleanup(func() { retryDelay = func(attempt int) time.Duration { return time.Duration(attempt) * time.Second } })
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(502)
			return
		}
		w.Write([]byte(`{"sha":"` + strings.Repeat("0", 40) + `"}`)) // a contract violation ends the attempt
	}))
	defer server.Close()
	c, _ := New("fixture/product", "")
	c.api = server.URL
	c.http.Transport.(*http.Transport).TLSClientConfig.RootCAs = server.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	if _, err := c.Fetch(context.Background(), testSHA); err == nil || calls.Load() != 2 {
		t.Fatalf("the 502 must be retried once and the contract violation must not: calls=%d", calls.Load())
	}
}

func TestAClosedConnectionIsRetriedAndACertificateFailureIsNot(t *testing.T) {
	retryDelay = func(int) time.Duration { return time.Millisecond }
	t.Cleanup(func() { retryDelay = func(attempt int) time.Duration { return time.Duration(attempt) * time.Second } })
	var calls, connections atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close() // close before any response
		}
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.StartTLS()
	defer server.Close()
	c, _ := New("fixture/product", "")
	c.api = server.URL
	c.http.Transport.(*http.Transport).TLSClientConfig.RootCAs = server.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	if _, err := c.Fetch(context.Background(), testSHA); !errors.Is(err, ErrSource) || calls.Load() != fetchAttempts {
		t.Fatalf("a closed connection must be retried boundedly: calls=%d err=%v", calls.Load(), err)
	}
	connections.Store(0)
	untrusted, _ := New("fixture/product", "")
	untrusted.api = server.URL // no fixture CA: the certificate is rejected
	if _, err := untrusted.Fetch(context.Background(), testSHA); !errors.Is(err, ErrSource) || connections.Load() != 1 {
		t.Fatalf("a certificate failure must not be retried: connections=%d", connections.Load())
	}
}

func TestCancellationDuringBackoffStopsRetrying(t *testing.T) {
	retryDelay = func(int) time.Duration { return time.Minute }
	t.Cleanup(func() { retryDelay = func(attempt int) time.Duration { return time.Duration(attempt) * time.Second } })
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(503)
	}))
	defer server.Close()
	c, _ := New("fixture/product", "")
	c.api = server.URL
	c.http.Transport.(*http.Transport).TLSClientConfig.RootCAs = server.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := c.Fetch(ctx, testSHA); !errors.Is(err, ErrSource) || calls.Load() != 1 || time.Since(started) > 5*time.Second {
		t.Fatalf("cancellation must end the backoff: calls=%d", calls.Load())
	}
}

func TestAnonymousReadSkipsTheAPIRedirect(t *testing.T) {
	raw := archiveFixture(t, nil)
	var apiCalls, archiveCalls atomic.Int32
	var archiveStatus atomic.Int32
	archive := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		archiveCalls.Add(1)
		if r.Header.Get("Authorization") != "" || r.URL.Path != "/fixture/product/legacy.tar.gz/"+testSHA || r.URL.RawQuery != "" {
			t.Error("unexpected anonymous archive request")
		}
		if status := archiveStatus.Load(); status != 0 {
			w.WriteHeader(int(status))
			return
		}
		w.Write(raw)
	}))
	defer archive.Close()
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiCalls.Add(1)
		switch r.URL.Path {
		case "/repos/fixture/product/git/commits/" + testSHA:
			json.NewEncoder(w).Encode(map[string]any{"sha": testSHA, "tree": map[string]string{"sha": "4b825dc642cb6eb9a060e54bf8d69288fbee4904"}})
		case "/repos/fixture/product/git/trees/4b825dc642cb6eb9a060e54bf8d69288fbee4904":
			json.NewEncoder(w).Encode(map[string]any{"sha": "4b825dc642cb6eb9a060e54bf8d69288fbee4904", "tree": []any{}, "truncated": false})
		default:
			t.Error("anonymous read reached the API tarball redirect")
			w.WriteHeader(404)
		}
	}))
	defer api.Close()
	c, err := New("fixture/product", "")
	if err != nil {
		t.Fatal(err)
	}
	c.api = api.URL
	c.archive = archive.URL
	c.http.Transport.(*http.Transport).TLSClientConfig.RootCAs = api.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	if _, err = c.Fetch(context.Background(), testSHA); err != nil {
		t.Fatal(err)
	}
	if apiCalls.Load() != 2 || archiveCalls.Load() != 1 {
		t.Fatalf("anonymous read: api=%d archive=%d", apiCalls.Load(), archiveCalls.Load())
	}
	// The archive is still verified against the manifest, and an archive host
	// failure keeps the existing classification.
	archiveStatus.Store(429)
	if _, err = c.Fetch(context.Background(), testSHA); !RateLimited(err) {
		t.Fatal("archive rate limit not classified", err)
	}
	archiveStatus.Store(0)
	raw = archiveFixture(t, []*tar.Header{{Typeflag: tar.TypeReg, Name: testRoot + "README.md", Mode: 0644, Size: 3}})
	if _, err = c.Fetch(context.Background(), testSHA); err == nil {
		t.Fatal("an archive outside the manifest was accepted")
	}
}
