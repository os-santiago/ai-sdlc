package modelbroker

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestProviderRejectsUntrustedConfiguration(t *testing.T) {
	for _, endpoint := range []string{
		"http://provider.example/v1/chat/completions", "http://localhost/v1/chat/completions",
		"https://user:password@provider.example/v1/chat/completions",
		"https://provider.example/v1/chat/completions?redirect=other",
		"https://provider.example/v1/chat/completions#fragment",
		"https://provider.example/other", "file:///v1/chat/completions",
	} {
		if _, err := NewProvider(ProviderConfig{Endpoint: endpoint, BearerToken: strings.Repeat("x", 32), AllowLoopbackHTTP: true}); err == nil {
			t.Fatalf("unsafe endpoint accepted: %q", endpoint)
		}
	}
	if _, err := NewProvider(ProviderConfig{Endpoint: "http://127.0.0.1:8787/v1/chat/completions", BearerToken: strings.Repeat("x", 32)}); err == nil {
		t.Fatal("local HTTP enabled implicitly")
	}
	if _, err := NewProvider(ProviderConfig{Endpoint: "https://provider.example/v1/chat/completions", BearerToken: strings.Repeat("x", 32) + "\r\nInjected: yes"}); err == nil {
		t.Fatal("header injection accepted")
	}
}
func TestProviderDoesNotUseAmbientProxy(t *testing.T) {
	var calls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(502) }))
	defer proxy.Close()
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("ALL_PROXY", proxy.URL)
	provider, err := NewProvider(ProviderConfig{Endpoint: "https://ai-sdlc-provider.invalid/v1/chat/completions", BearerToken: strings.Repeat("x", 32)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err = provider.invoke(ctx, []byte(`{}`), 1024); !errors.Is(err, ErrProvider) {
		t.Fatal("invalid provider unexpectedly connected")
	}
	if calls.Load() != 0 {
		t.Fatal("provider request reached ambient proxy")
	}
}
func TestProviderRequiresVerifiedTLS(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer server.Close()
	provider, err := NewProvider(ProviderConfig{Endpoint: server.URL + "/v1/chat/completions", BearerToken: strings.Repeat("x", 32)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = provider.invoke(context.Background(), []byte(`{}`), 1024); !errors.Is(err, ErrProvider) {
		t.Fatal("untrusted TLS certificate accepted")
	}
	if calls.Load() != 0 {
		t.Fatal("credential reached unverified TLS endpoint")
	}
}
func TestProviderSlowResponseBodyIsBounded(t *testing.T) {
	entered := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		close(entered)
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer server.Close()
	provider, err := NewProvider(ProviderConfig{Endpoint: server.URL + "/v1/chat/completions", BearerToken: strings.Repeat("x", 32), AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := provider.invoke(ctx, []byte(`{}`), 1024); done <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("response headers not observed")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, ErrProvider) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("slow body escaped cancellation")
	}
}

func TestProviderAcceptsBufferedCompletionWithinAdapterDeadline(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-time.After(16 * time.Second):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"bounded completion"}}]}`)
	}))
	defer server.Close()
	provider, err := NewProvider(ProviderConfig{Endpoint: server.URL + "/v1/chat/completions", BearerToken: strings.Repeat("x", 32), AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 28*time.Second)
	defer cancel()
	if _, err = provider.invoke(ctx, []byte(`{}`), 1024); err != nil {
		t.Fatal("valid buffered completion rejected")
	}
	if calls.Load() != 1 {
		t.Fatal("provider call repeated")
	}
}

func TestProviderHeaderWaitHonorsEarlierCancellation(t *testing.T) {
	entered := make(chan struct{})
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		close(entered)
		<-r.Context().Done()
	}))
	defer server.Close()
	provider, err := NewProvider(ProviderConfig{Endpoint: server.URL + "/v1/chat/completions", BearerToken: strings.Repeat("x", 32), AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := provider.invoke(ctx, []byte(`{}`), 1024); done <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("provider not reached")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, ErrProvider) {
			t.Fatal("cancellation accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("header cancellation did not terminate")
	}
	if calls.Load() != 1 {
		t.Fatal("provider call repeated")
	}
}
