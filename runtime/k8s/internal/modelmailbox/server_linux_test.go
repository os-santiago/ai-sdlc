package modelmailbox

import (
	"bytes"
	"encoding/json"
	"errors"
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

func TestRestrictedMailboxRejectsOverriddenModelAndTokenLimits(t *testing.T) {
	store, dir, _ := setupServer(t, time.Second)
	server, err := NewRestrictedServer(store, testIdentity, time.Second, RequestLimits{Model: "m", MaxTokens: 2048})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()
	for _, body := range []string{
		`{"model":"m","messages":[{}],"max_tokens":1000000,"stream":false}`,
		`{"model":"other","messages":[{}],"max_tokens":2048,"stream":false}`,
		`{"model":"m","messages":[{}],"stream":false}`,
		`{"model":"m","messages":[{}],"max_tokens":null,"stream":false}`,
		`{"model":"m","messages":[{}],"max_tokens":1.5,"stream":false}`,
		`{"model":"m","messages":[{}],"max_tokens":2048,"stream":true}`,
	} {
		result(t, send(httpServer, []byte(body)), 400)
		if _, err := os.Stat(filepath.Join(dir, "request.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("rejected request was published")
		}
	}
	if err := (RequestLimits{Model: "m", MaxTokens: 2048}).Validate([]byte(`{"model":"m","messages":[{}],"max_tokens":2048,"stream":false}`)); err != nil {
		t.Fatal(err)
	}
}

// The largest installation-selected answer bound (8192) is admitted exactly.
func TestRequestLimitsAdmitTheLargestAnswerBound(t *testing.T) {
	limits := RequestLimits{Model: "m", MaxTokens: 8192}
	if err := limits.Validate([]byte(`{"model":"m","messages":[{}],"max_tokens":8192,"stream":false}`)); err != nil {
		t.Fatal(err)
	}
	if limits.Validate([]byte(`{"model":"m","messages":[{}],"max_tokens":8193,"stream":false}`)) == nil {
		t.Fatal("request above the answer bound admitted")
	}
	store, _, _ := setupServer(t, time.Second)
	if _, err := NewRestrictedServer(store, testIdentity, time.Second, limits); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRestrictedServer(store, testIdentity, time.Second, RequestLimits{Model: "m", MaxTokens: 8193}); err == nil {
		t.Fatal("answer bound above the runtime cap admitted")
	}
}

// setupKeepaliveServer shortens the keepalive commit point and interval so
// the long-call path runs in milliseconds.
func setupKeepaliveServer(t *testing.T, wait, commitAfter, every time.Duration) (*Store, string, *httptest.Server) {
	t.Helper()
	s, dir := newTestStore(t)
	handler, err := NewServer(s, testIdentity, wait)
	if err != nil {
		t.Fatal(err)
	}
	handler.commitAfter, handler.keepaliveEvery = commitAfter, every
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)
	return s, dir, httpServer
}

// post returns once response headers arrive, like fetch.
func post(t *testing.T, httpServer *httptest.Server) (*http.Response, time.Duration) {
	t.Helper()
	started := time.Now()
	r, err := (&http.Client{Timeout: 3 * time.Second}).Post(httpServer.URL+"/v1/chat/completions", "application/json", bytes.NewReader(testBody))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Body.Close() })
	return r, time.Since(started)
}

func TestDefaultKeepaliveStaysUnderUndiciTimeouts(t *testing.T) {
	s, _ := newTestStore(t)
	server, err := NewServer(s, testIdentity, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	const undici = 300 * time.Second // Node fetch headersTimeout and bodyTimeout
	if server.commitAfter != keepaliveCommit || server.keepaliveEvery != keepaliveInterval || keepaliveCommit > undici-time.Minute || keepaliveInterval > undici/5 {
		t.Fatal("keepalive does not leave a safe margin under undici's 300 s defaults")
	}
}

func TestLongCallCommitsHeadersAndPadsWithWhitespace(t *testing.T) {
	s, dir, server := setupKeepaliveServer(t, 2*time.Second, 50*time.Millisecond, 20*time.Millisecond)
	r, elapsed := post(t, server)
	request := awaitRequest(t, s)
	// Headers arrive at the commit point, before any response exists.
	if r.StatusCode != 200 || r.Header.Get("Content-Type") != "application/json" || elapsed > time.Second {
		t.Fatalf("headers not committed: %d %q after %s", r.StatusCode, r.Header.Get("Content-Type"), elapsed)
	}
	time.Sleep(200 * time.Millisecond) // several keepalive intervals
	response := testResponse(request)
	if err := s.WriteResponse(testIdentity, response); err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	padding := bytes.TrimSuffix(body, response.Payload)
	if len(padding) < 3 || len(padding) == len(body) || strings.Trim(string(padding), " ") != "" {
		t.Fatalf("unexpected keepalive body %q", body)
	}
	var parsed map[string]any
	if json.Unmarshal(body, &parsed) != nil || parsed["choices"] == nil {
		t.Fatal("padded body is not the JSON response")
	}
	for _, name := range []string{requestFile, responseFile} {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("exchange files not removed", name)
		}
	}
}

func TestEarlyResponseKeepsStatusAndExactBytes(t *testing.T) {
	for _, status := range []int{200, 503} {
		s, _, server := setupKeepaliveServer(t, 2*time.Second, time.Second, 20*time.Millisecond)
		done := send(server, testBody)
		response := testResponse(awaitRequest(t, s))
		response.Status = status
		if err := s.WriteResponse(testIdentity, response); err != nil {
			t.Fatal(err)
		}
		if got := result(t, done, status); !bytes.Equal(got.body, response.Payload) {
			t.Fatalf("early response changed: %q", got.body)
		}
	}
}

// After the commit a failure cannot change the 200, so the body is cut off
// without its final chunk and the client's read fails instead of parsing.
func TestFailureAfterCommitAbortsTheBody(t *testing.T) {
	for _, kind := range []string{"deadline", "mismatch", "status"} {
		t.Run(kind, func(t *testing.T) {
			s, dir, server := setupKeepaliveServer(t, 300*time.Millisecond, 50*time.Millisecond, 20*time.Millisecond)
			r, _ := post(t, server)
			request := awaitRequest(t, s)
			if r.StatusCode != 200 {
				t.Fatal("headers not committed", r.StatusCode)
			}
			time.Sleep(100 * time.Millisecond)
			response := testResponse(request)
			switch kind {
			case "mismatch":
				response.Sequence++
				raw, _ := json.Marshal(response)
				if err := os.WriteFile(filepath.Join(dir, responseFile), raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "status":
				response.Status = 502
				response.Payload = []byte(`{"error":{"message":"upstream"}}`)
				if err := s.WriteResponse(testIdentity, response); err != nil {
					t.Fatal(err)
				}
			}
			body, err := io.ReadAll(r.Body)
			if !errors.Is(err, io.ErrUnexpectedEOF) || strings.Trim(string(body), " ") != "" {
				t.Fatalf("post-commit failure was readable: %q %v", body, err)
			}
			for _, name := range []string{requestFile, responseFile} {
				if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("exchange files not removed", name)
				}
			}
			// The single in-flight slot was released.
			next := send(server, testBody)
			if err := s.WriteResponse(testIdentity, testResponse(awaitRequest(t, s))); err != nil {
				t.Fatal(err)
			}
			result(t, next, 200)
		})
	}
}
