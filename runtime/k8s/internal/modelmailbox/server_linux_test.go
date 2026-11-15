package modelmailbox

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func setupServer(t *testing.T, wait time.Duration) (*Store, string, *httptest.Server) {
	t.Helper()
	s, dir := newTestStore(t)
	handler, err := NewServer(s, testIdentity, wait)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)
	return s, dir, httpServer
}

type httpResult struct {
	status int
	body   []byte
	err    error
}

func send(httpServer *httptest.Server, body []byte) <-chan httpResult {
	done := make(chan httpResult, 1)
	go func() {
		client := &http.Client{Timeout: 3 * time.Second}
		r, err := client.Post(httpServer.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
		if err != nil {
			done <- httpResult{err: err}
			return
		}
		defer r.Body.Close()
		raw, err := io.ReadAll(r.Body)
		done <- httpResult{status: r.StatusCode, body: raw, err: err}
	}()
	return done
}
func awaitRequest(t *testing.T, s *Store) Envelope {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		e, err := s.ReadRequest(testIdentity)
		if err == nil {
			return e
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("request not published")
	return Envelope{}
}
func result(t *testing.T, done <-chan httpResult, status int) httpResult {
	t.Helper()
	select {
	case r := <-done:
		if r.err != nil || r.status != status {
			t.Fatalf("unexpected HTTP result %d: %v %s", r.status, r.err, r.body)
		}
		return r
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP request hung")
	}
	return httpResult{}
}

func TestHTTPExchangeAndLocalQuota(t *testing.T) {
	s, _, server := setupServer(t, time.Second)
	for i := uint64(1); i <= MaxRequests; i++ {
		done := send(server, testBody)
		request := awaitRequest(t, s)
		if request.Sequence != i || !bytes.Equal(request.Payload, testBody) {
			t.Fatal("request was changed")
		}
		response := testResponse(request)
		if err := s.WriteResponse(testIdentity, response); err != nil {
			t.Fatal(err)
		}
		r := result(t, done, 200)
		if !bytes.Equal(r.body, response.Payload) {
			t.Fatal("response changed")
		}
	}
	result(t, send(server, testBody), 429)
}
func TestConcurrentAndExpiredRequests(t *testing.T) {
	s, _, server := setupServer(t, 100*time.Millisecond)
	first := send(server, testBody)
	request := awaitRequest(t, s)
	result(t, send(server, testBody), 429)
	result(t, first, 504)
	if s.WriteResponse(testIdentity, testResponse(request)) == nil {
		t.Fatal("accepted expired response")
	}
}
func TestHostileResponseDoesNotReachClient(t *testing.T) {
	for _, kind := range []string{"uid", "digest", "sequence", "expired", "malformed", "oversize"} {
		t.Run(kind, func(t *testing.T) {
			s, dir, server := setupServer(t, time.Second)
			done := send(server, testBody)
			request := awaitRequest(t, s)
			response := testResponse(request)
			switch kind {
			case "uid":
				response.PodUID = "00000000-0000-0000-0000-000000000000"
			case "digest":
				response.RequestSHA256 = strings.Repeat("0", 64)
			case "sequence":
				response.Sequence++
			case "expired":
				response.ExpiresAt = time.Now().Add(-time.Second).UnixMilli()
			}
			raw, _ := json.Marshal(response)
			if kind == "malformed" {
				raw = []byte(`{"kind":"response"}`)
			}
			if kind == "oversize" {
				raw = bytes.Repeat([]byte("x"), MaxEnvelopeBytes+1)
			}
			if err := os.WriteFile(filepath.Join(dir, responseFile), raw, 0600); err != nil {
				t.Fatal(err)
			}
			result(t, done, 502)
		})
	}
}
func TestHTTPInputLimits(t *testing.T) {
	_, _, server := setupServer(t, time.Second)
	result(t, send(server, bytes.Repeat([]byte("x"), MaxRequestBytes+1)), 413)
	result(t, send(server, []byte(`{"model":"m","messages":[{}],"stream":true}`)), 400)
	response, err := http.Get(server.URL + "/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 404 {
		t.Fatal("accepted wrong method")
	}
}

func TestLinkedResponseWaitsOnlyUntilOriginalDeadline(t *testing.T) {
	for _, transient := range []bool{false, true} {
		t.Run(map[bool]string{false: "persistent", true: "publication-window"}[transient], func(t *testing.T) {
			s, dir, server := setupServer(t, 200*time.Millisecond)
			done := send(server, testBody)
			request := awaitRequest(t, s)
			response := testResponse(request)
			raw, _ := json.Marshal(response)
			staging := filepath.Join(dir, "staging-fixture")
			if err := os.WriteFile(staging, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(staging, filepath.Join(dir, responseFile)); err != nil {
				t.Fatal(err)
			}
			if _, err := s.read(responseFile); err != ErrPending {
				t.Fatal("linked bytes were available", err)
			}
			if transient {
				timer := time.AfterFunc(50*time.Millisecond, func() { os.Remove(staging) })
				defer timer.Stop()
				got := result(t, done, 200)
				if !bytes.Equal(got.body, response.Payload) {
					t.Fatal("response changed")
				}
			} else {
				result(t, done, 504)
			}
		})
	}
}
