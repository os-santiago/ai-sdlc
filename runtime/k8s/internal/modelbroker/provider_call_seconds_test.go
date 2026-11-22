package modelbroker

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestProviderSendsTheCallersRemainingWait(t *testing.T) {
	seen := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		seen <- strings.Join(r.Header.Values(CallSecondsHeader), ",")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer server.Close()
	provider, err := NewProvider(ProviderConfig{Endpoint: server.URL + "/v1/chat/completions", BearerToken: strings.Repeat("x", 32), AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := provider.invoke(ctx, []byte(`{}`), 1024); err != nil {
		t.Fatal(err)
	}
	got, _ := strconv.Atoi(<-seen)
	if got < 57 || got > 59 {
		t.Fatalf("remaining wait %d, want just under 60", got)
	}
	if _, err := provider.invoke(context.Background(), []byte(`{}`), 1024); err != nil {
		t.Fatal(err)
	}
	if header := <-seen; header != "" {
		t.Fatalf("header %q sent without a caller deadline", header)
	}
}
