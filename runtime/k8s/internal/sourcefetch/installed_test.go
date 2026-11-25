package sourcefetch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

// A fake value with the shape of a fine-grained token; never a real credential.
var fakeReadToken = "github_pat_" + strings.Repeat("F", 22) + "_" + strings.Repeat("f", 59)

func writeCredential(t *testing.T, document any) string {
	t.Helper()
	raw, ok := document.([]byte)
	if !ok {
		raw, _ = json.Marshal(document)
	}
	path := filepath.Join(t.TempDir(), "credential.json")
	if err := os.WriteFile(path, raw, 0o400); err != nil {
		t.Fatal(err)
	}
	return path
}

func validCredential() map[string]any {
	return map[string]any{"version": 1, "host": "github.com", "repository": "fixture/product", "access": "read", "token": fakeReadToken}
}

// fakeForge is a TLS API and archive host pair for fixture/product@testSHA.
// apiStatus, when set, answers every API request with that status and headers.
type fakeForge struct {
	api, archive *httptest.Server
	mu           sync.Mutex
	apiAuth      []string
	archiveAuth  []string
	apiStatus    int
	apiHeaders   map[string]string
}

func newFakeForge(t *testing.T) *fakeForge {
	t.Helper()
	f := &fakeForge{}
	raw := archiveFixture(t, nil)
	f.archive = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.archiveAuth = append(f.archiveAuth, r.Header.Get("Authorization"))
		f.mu.Unlock()
		w.Write(raw)
	}))
	f.api = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.apiAuth = append(f.apiAuth, r.Header.Get("Authorization"))
		status, headers := f.apiStatus, f.apiHeaders
		f.mu.Unlock()
		if status != 0 {
			for key, value := range headers {
				w.Header().Set(key, value)
			}
			w.WriteHeader(status)
			// A provider body must never reach an error.
			if headers["X-Fixture-Body"] == "secondary" {
				fmt.Fprint(w, `{"message":"You have exceeded a Secondary Rate Limit. Please wait. `+r.Header.Get("Authorization")+`"}`)
				return
			}
			fmt.Fprint(w, `{"message":"Bad credentials `+r.Header.Get("Authorization")+`"}`)
			return
		}
		switch r.URL.Path {
		case "/repos/fixture/product/git/commits/" + testSHA:
			json.NewEncoder(w).Encode(map[string]any{"sha": testSHA, "tree": map[string]string{"sha": "4b825dc642cb6eb9a060e54bf8d69288fbee4904"}})
		case "/repos/fixture/product/git/trees/4b825dc642cb6eb9a060e54bf8d69288fbee4904":
			json.NewEncoder(w).Encode(map[string]any{"sha": "4b825dc642cb6eb9a060e54bf8d69288fbee4904", "tree": []any{}, "truncated": false})
		case "/repos/fixture/product/tarball/" + testSHA:
			w.Header().Set("Location", f.archive.URL+"/fixture/product/legacy.tar.gz/"+testSHA)
			w.WriteHeader(http.StatusFound)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(func() { f.api.Close(); f.archive.Close() })
	return f
}

// point redirects a client built by the Reader to the fake forge.
func (f *fakeForge) point(c *Client) {
	c.api, c.archive = f.api.URL, f.archive.URL
	c.http.Transport.(*http.Transport).TLSClientConfig.RootCAs = f.api.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
}

func (f *fakeForge) calls() (api, archive []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.apiAuth...), append([]string(nil), f.archiveAuth...)
}

func TestInstalledReaderAuthenticatesOnlyAPIReadsOfItsRepository(t *testing.T) {
	forge := newFakeForge(t)
	reader, err := InstalledReader(writeCredential(t, validCredential()))
	if err != nil || !reader.Authenticated() || reader.Check("fixture/product") != nil {
		t.Fatal("valid installed credential refused")
	}
	client, err := reader.client("fixture/product")
	if err != nil {
		t.Fatal(err)
	}
	forge.point(client)
	if _, err = client.Fetch(context.Background(), testSHA); err != nil {
		t.Fatal(err)
	}
	api, archive := forge.calls()
	// Commit, tree and the archive redirect: every API read authenticated.
	if len(api) != 3 || len(archive) != 1 {
		t.Fatalf("unexpected requests: api=%d archive=%d", len(api), len(archive))
	}
	for _, header := range api {
		if header != "Bearer "+fakeReadToken {
			t.Fatal("unauthenticated API read with an installed credential")
		}
	}
	if archive[0] != "" {
		t.Fatal("the read credential reached the archive host")
	}
	// The credential is bound to its repository: another repository is a
	// named stop, never an anonymous read of that repository.
	if _, err = reader.Fetch(context.Background(), "other/product", testSHA); !CredentialInvalid(err) {
		t.Fatal("credential used outside its repository")
	}
}

func TestAnonymousReaderIsUnchanged(t *testing.T) {
	forge := newFakeForge(t)
	reader, err := InstalledReader("")
	if err != nil || reader.Authenticated() || reader.Check("fixture/product") != nil {
		t.Fatal("anonymous reader refused")
	}
	client, err := reader.client("fixture/product")
	if err != nil || client.token != "" {
		t.Fatal("anonymous reader carries a credential")
	}
	forge.point(client)
	if _, err = client.Fetch(context.Background(), testSHA); err != nil {
		t.Fatal(err)
	}
	api, archive := forge.calls()
	// Commit and tree only; the archive comes from the fixed host directly.
	if len(api) != 2 || len(archive) != 1 || api[0] != "" || api[1] != "" || archive[0] != "" {
		t.Fatal("anonymous read changed")
	}
}

func TestRejectedCredentialIsANamedStopWithoutAnonymousFallback(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		headers  map[string]string
		rejected bool
		limited  bool
	}{
		{"unauthorized", 401, nil, true, false},
		{"forbidden", 403, nil, true, false},
		{"primary-rate-limit", 403, map[string]string{"X-RateLimit-Remaining": "0"}, false, true},
		{"secondary-rate-limit", 403, map[string]string{"Retry-After": "30"}, false, true},
		{"too-many-requests", 429, nil, false, true},
		{"not-found", 404, nil, false, false},
		// GitHub may name a secondary limit only in the message, with quota left.
		{"secondary-rate-limit-message", 403, map[string]string{"X-RateLimit-Remaining": "4999", "X-Fixture-Body": "secondary"}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			forge := newFakeForge(t)
			forge.apiStatus, forge.apiHeaders = tc.status, tc.headers
			reader, _ := InstalledReader(writeCredential(t, validCredential()))
			client, err := reader.client("fixture/product")
			if err != nil {
				t.Fatal(err)
			}
			forge.point(client)
			_, err = client.Fetch(context.Background(), testSHA)
			if err == nil || CredentialRejected(err) != tc.rejected || RateLimited(err) != tc.limited {
				t.Fatalf("misclassified: %v", err)
			}
			api, archive := forge.calls()
			// One request, authenticated: neither retried nor repeated anonymously.
			if len(api) != 1 || api[0] != "Bearer "+fakeReadToken || len(archive) != 0 {
				t.Fatalf("fallback or retry after refusal: api=%q archive=%d", api, len(archive))
			}
			if strings.Contains(err.Error(), fakeReadToken) || strings.Contains(err.Error(), "Bad credentials") || strings.Contains(strings.ToLower(err.Error()), "secondary") {
				t.Fatal("credential or provider body in error")
			}
		})
	}
	// Without a credential the same statuses stay availability failures, and
	// a secondary limit named only in the message is still a rate limit.
	for _, tc := range []struct {
		status  int
		headers map[string]string
		limited bool
	}{
		{401, nil, false},
		{403, nil, false},
		{403, map[string]string{"X-RateLimit-Remaining": "59", "X-Fixture-Body": "secondary"}, true},
	} {
		forge := newFakeForge(t)
		forge.apiStatus, forge.apiHeaders = tc.status, tc.headers
		client, _ := New("fixture/product", "")
		forge.point(client)
		_, err := client.Fetch(context.Background(), testSHA)
		if err == nil || CredentialRejected(err) || RateLimited(err) != tc.limited {
			t.Fatalf("anonymous %d misclassified: %v", tc.status, err)
		}
		if api, _ := forge.calls(); len(api) != 1 || api[0] != "" {
			t.Fatal("anonymous refusal retried or authenticated")
		}
	}
}

func TestInvalidInstalledCredentialStopsBeforeAnyRead(t *testing.T) {
	duplicate := []byte(`{"version":1,"host":"github.com","repository":"fixture/product","access":"read","token":"` + fakeReadToken + `","token":"` + strings.Repeat("x", 32) + `"}`)
	mutate := func(key string, value any) map[string]any {
		document := validCredential()
		if value == nil {
			delete(document, key)
		} else {
			document[key] = value
		}
		return document
	}
	// The intake/publication client document: write-capable, other fields.
	writeClient := map[string]any{"version": 1, "mode": "gh-cli", "host": "github.com", "repository": "fixture/product", "actorID": 1, "actorLogin": "fixture", "token": fakeReadToken}
	documents := map[string]any{
		"other-repository": mutate("repository", "other/product"),
		"write-access":     mutate("access", "write"),
		"missing-access":   mutate("access", nil),
		"version":          mutate("version", 2),
		"host":             mutate("host", "github.example"),
		"token-shape":      mutate("token", fakeReadToken+"\r\nInjected: yes"),
		"short-token":      mutate("token", "short"),
		"empty-token":      mutate("token", ""),
		"write-client":     writeClient,
		"duplicate-key":    duplicate,
		"oversized":        []byte(strings.Repeat(" ", maxCredentialBytes+1)),
		"empty":            []byte{},
		"not-json":         []byte(fakeReadToken),
		// Go would match these keys case-insensitively; the installer would not.
		"case-variant-key": []byte(`{"version":1,"host":"github.com","repository":"fixture/product","access":"read","Token":"` + fakeReadToken + `"}`),
		"extra-case-key":   []byte(`{"version":1,"host":"github.com","repository":"fixture/product","access":"read","token":"` + fakeReadToken + `","TOKEN":"` + fakeReadToken + `"}`),
	}
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	for name, document := range documents {
		t.Run(name, func(t *testing.T) {
			reader, err := InstalledReader(writeCredential(t, document))
			if err != nil {
				t.Fatal(err)
			}
			// The fixed API host is never contacted: the load fails first.
			_, err = reader.Fetch(context.Background(), "fixture/product", testSHA)
			if !CredentialInvalid(err) || reader.Check("fixture/product") == nil {
				t.Fatalf("invalid credential accepted: %v", err)
			}
			if strings.Contains(err.Error(), fakeReadToken) {
				t.Fatal("credential in error")
			}
		})
	}
	missing, _ := InstalledReader(filepath.Join(t.TempDir(), "absent.json"))
	if _, err := missing.Fetch(context.Background(), "fixture/product", testSHA); !CredentialInvalid(err) {
		t.Fatal("a missing installed credential must not fall back to anonymous reads")
	}
	for _, path := range []string{"relative/credential.json", "/var/run/../credential.json", "/var/run/ai-sdlc/source/"} {
		if _, err := InstalledReader(path); !CredentialInvalid(err) {
			t.Fatal("unclean credential path accepted: " + path)
		}
	}
	// The Reader holds a path, never the token, so printing it reveals nothing.
	reader, _ := InstalledReader(writeCredential(t, validCredential()))
	if strings.Contains(fmt.Sprintf("%v %+v %#v", reader, reader, reader), fakeReadToken) || strings.Contains(logs.String(), fakeReadToken) {
		t.Fatal("credential printed or logged")
	}
}

// A Cached reader, shared by verification and review, reads one pinned SHA
// once and suppresses repeated reads while the forge is rate limited
// (evaluation battery 28: an uncached review re-downloaded the source on each
// reconcile tick and exhausted the anonymous quota). Copies share the cache;
// the uncached reader is unchanged.
func TestCachedReaderReusesTheBundleAndBacksOffOnRateLimit(t *testing.T) {
	base, err := InstalledReader("")
	if err != nil {
		t.Fatal(err)
	}
	reader := base.Cached()
	copied := reader
	var reads int
	limited := false
	reader.cache.fetch = func(_ context.Context, _, sha string) (workspacebundle.Bundle, error) {
		reads++
		if limited {
			return workspacebundle.Bundle{}, AvailabilityError{retryAt: time.Now().Add(time.Minute), rateLimited: true}
		}
		return cachedFixture(sha), nil
	}
	for i := 0; i < 3; i++ {
		for _, r := range []Reader{reader, copied} {
			if b, err := r.Fetch(context.Background(), "fixture/product", testSHA); err != nil || b.SourceSHA != testSHA {
				t.Fatal("cached read failed", err)
			}
		}
	}
	if reads != 1 {
		t.Fatalf("pinned SHA read %d times, want 1", reads)
	}
	limited = true
	other := strings.Repeat("b", 40)
	for i := 0; i < 3; i++ {
		if _, err := copied.Fetch(context.Background(), "fixture/product", other); !RateLimited(err) {
			t.Fatal("rate limit not reported", err)
		}
	}
	if reads != 2 {
		t.Fatalf("rate-limited reads repeated: %d reads, want 2", reads)
	}
	if base.cache != nil || reader.Authenticated() != base.Authenticated() {
		t.Fatal("Cached changed the original reader")
	}
}

// The #796 guarantees hold through the real Cached wiring: a valid
// credential authenticates the cached read, a rejected one is a named stop
// with no anonymous request, and an invalid file stops before any request
// and is not cached (a fixed file takes effect at once).
func TestCachedReaderKeepsCredentialGuarantees(t *testing.T) {
	t.Run("authenticated", func(t *testing.T) {
		forge := newFakeForge(t)
		base, _ := InstalledReader(writeCredential(t, validCredential()))
		base.adjust = forge.point
		reader := base.Cached()
		if _, err := reader.Fetch(context.Background(), "fixture/product", testSHA); err != nil {
			t.Fatal(err)
		}
		api, archive := forge.calls()
		for _, auth := range api {
			if auth != "Bearer "+fakeReadToken {
				t.Fatal("cached read not authenticated")
			}
		}
		if len(api) == 0 {
			t.Fatal("no forge read")
		}
		if _, err := reader.Fetch(context.Background(), "fixture/product", testSHA); err != nil {
			t.Fatal(err)
		}
		if again, archiveAgain := forge.calls(); len(again) != len(api) || len(archiveAgain) != len(archive) {
			t.Fatal("cached SHA read from the forge again")
		}
	})
	t.Run("rejected", func(t *testing.T) {
		forge := newFakeForge(t)
		forge.apiStatus = 401
		base, _ := InstalledReader(writeCredential(t, validCredential()))
		base.adjust = forge.point
		reader := base.Cached()
		if _, err := reader.Fetch(context.Background(), "fixture/product", testSHA); !CredentialRejected(err) {
			t.Fatal("rejection not named", err)
		}
		if api, archive := forge.calls(); len(api) != 1 || api[0] != "Bearer "+fakeReadToken || len(archive) != 0 {
			t.Fatal("anonymous fallback or retry through the cache")
		}
	})
	t.Run("invalid-not-cached", func(t *testing.T) {
		forge := newFakeForge(t)
		path := writeCredential(t, validCredential())
		if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		base, _ := InstalledReader(path)
		base.adjust = forge.point
		reader := base.Cached()
		if _, err := reader.Fetch(context.Background(), "fixture/product", testSHA); !CredentialInvalid(err) {
			t.Fatal("invalid credential not named", err)
		}
		if api, archive := forge.calls(); len(api) != 0 || len(archive) != 0 {
			t.Fatal("request made with an invalid credential")
		}
		raw, _ := json.Marshal(validCredential())
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := reader.Fetch(context.Background(), "fixture/product", testSHA); err != nil {
			t.Fatal("fixed credential did not take effect at once", err)
		}
	})
}
