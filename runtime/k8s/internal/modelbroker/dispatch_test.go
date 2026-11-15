package modelbroker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelmailbox"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

var completionBody = []byte(`{"id":"fixture","object":"chat.completion","model":"fixture-model","choices":[{"index":0,"message":{"role":"assistant","content":"fixture"},"finish_reason":"stop"}]}`)

type publisherFunc func(context.Context, modelmailbox.Envelope) error

func (f publisherFunc) Publish(ctx context.Context, e modelmailbox.Envelope) error { return f(ctx, e) }

func brokerFixture(t *testing.T, handler http.HandlerFunc, publisher Publisher) (Dispatcher, store.ModelAllocation, []byte, modelmailbox.Envelope) {
	t.Helper()
	dsn := os.Getenv("AI_SDLC_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("real PostgreSQL fixture required")
	}
	ctx := context.Background()
	db, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err = db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	plan, work := store.NewID(), store.NewID()
	if err = store.UpsertPlan(ctx, db.Pool, store.PlanRecord{ID: plan, Repository: "fixture/product", CorrelationID: store.NewID()}); err != nil {
		t.Fatal(err)
	}
	if err = store.UpsertWorkItem(ctx, db.Pool, store.WorkItemRecord{PlanID: plan, ID: "fixture", UUID: work, Status: "running", IssueNumbers: []int{1}, AcceptanceCriteria: []string{"fixture"}}); err != nil {
		t.Fatal(err)
	}
	profile, _ := json.Marshal(executor.Profile{Version: executor.ProfileVersion, Model: "fixture-model", MaxSteps: 4, MaxSeconds: 3, MaxTokens: 64, ADevSHA256: strings.Repeat("a", 64)})
	budget := store.ModelBudget{WorkItem: work, Repository: "fixture/product", SourceSHA: strings.Repeat("b", 40), PolicyDigest: modelmailbox.Digest(profile), Model: "fixture-model", Limits: store.ModelLimits{Calls: 4, InputBytes: 100000, OutputBytes: 100000}, ExpiresAt: time.Now().Add(30 * time.Minute)}
	if err = db.CreateModelBudget(ctx, budget); err != nil {
		t.Fatal(err)
	}
	allocation := store.ModelAllocation{ID: store.NewID(), WorkItem: work, PodUID: store.NewID(), Attempt: 1, Limits: store.ModelLimits{Calls: 4, InputBytes: 10000, OutputBytes: 10000}, ResponseLimit: 1024, ExpiresAt: time.Now().Add(20 * time.Minute)}
	if err = db.CreateModelAllocation(ctx, allocation); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	provider, err := NewProvider(ProviderConfig{Endpoint: server.URL + "/v1/chat/completions", BearerToken: strings.Repeat("x", 32), AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"model":"fixture-model","messages":[{"role":"user","content":"fixture"}],"stream":false,"max_tokens":64}`)
	request := modelmailbox.Envelope{Version: modelmailbox.Version, Kind: "request", Attempt: work + "-1", PodUID: allocation.PodUID, Sequence: 1, RequestSHA256: modelmailbox.Digest(body), Payload: body, ExpiresAt: time.Now().Add(time.Minute).UnixMilli()}
	return Dispatcher{DB: db, Provider: provider, Mailbox: publisher}, allocation, profile, request
}
func ledgerState(t *testing.T, db *store.DB, a store.ModelAllocation) (string, string, bool, int) {
	t.Helper()
	var parent, call string
	var handed bool
	var count int
	err := db.Pool.QueryRow(`SELECT b.state,c.state,c.response_handed_off_at IS NOT NULL,b.reserved_calls FROM model_calls c JOIN model_allocations a ON a.id=c.allocation_id JOIN model_budgets b ON b.work_item_uuid=a.work_item_uuid WHERE a.id=$1`, a.ID).Scan(&parent, &call, &handed, &count)
	if err != nil {
		t.Fatal(err)
	}
	return parent, call, handed, count
}

func TestBrokerConcurrentDeliveryInvokesAndPublishesOnce(t *testing.T) {
	var providerCalls, published atomic.Int32
	var observed []byte
	var mu sync.Mutex
	dispatcher, a, profile, request := brokerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		providerCalls.Add(1)
		if r.Method != "POST" || r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer "+strings.Repeat("x", 32) {
			t.Error("provider request binding changed")
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		mu.Lock()
		observed = raw
		mu.Unlock()
		time.Sleep(50 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.Write(completionBody)
	}, publisherFunc(func(ctx context.Context, e modelmailbox.Envelope) error {
		published.Add(1)
		if e.Kind != "response" || e.Status != 200 || !bytes.Equal(e.Payload, completionBody) {
			return errors.New("response bytes changed")
		}
		return nil
	}))
	var wg sync.WaitGroup
	var attempts atomic.Int32
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := dispatcher.Deliver(context.Background(), a.ID, profile, request)
			if result.DispatchAttempted {
				attempts.Add(1)
			}
			if err != nil && !errors.Is(err, ErrNotClaimed) {
				t.Errorf("delivery: %+v %v", result, err)
			}
		}()
	}
	wg.Wait()
	if providerCalls.Load() != 1 || published.Load() != 1 || attempts.Load() != 1 {
		t.Fatalf("provider=%d publisher=%d attempts=%d", providerCalls.Load(), published.Load(), attempts.Load())
	}
	mu.Lock()
	same := bytes.Equal(observed, request.Payload)
	mu.Unlock()
	if !same {
		t.Fatal("actual provider request bytes changed")
	}
	parent, call, handed, count := ledgerState(t, dispatcher.DB, a)
	if parent != "active" || call != "completed" || !handed || count != 1 {
		t.Fatalf("ledger: %s %s %t %d", parent, call, handed, count)
	}
	result, err := dispatcher.Deliver(context.Background(), a.ID, profile, request)
	if err != nil || result.DispatchAttempted || !result.HandoffConfirmed || providerCalls.Load() != 1 || published.Load() != 1 {
		t.Fatalf("replay: %+v %v", result, err)
	}
}

func TestBrokerProviderFailuresNeverRetryOrPublish(t *testing.T) {
	for _, kind := range []string{"status", "redirect", "oversized", "headers", "broken", "invalid-json", "invalid-utf8", "wrong-role"} {
		t.Run(kind, func(t *testing.T) {
			var calls, published, redirected atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
			defer target.Close()
			dispatcher, a, profile, request := brokerFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				switch kind {
				case "status":
					w.WriteHeader(503)
					w.Write([]byte(`{"error":"fixture"}`))
				case "redirect":
					w.Header().Set("Location", target.URL+"/credential-trap")
					w.WriteHeader(307)
				case "oversized":
					w.Write([]byte(`{"padding":"` + strings.Repeat("x", 2048) + `"}`))
				case "headers":
					w.Header().Set("X-Padding", strings.Repeat("x", 9000))
					w.Write(completionBody)
				case "broken":
					conn, buff, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					defer conn.Close()
					fmt.Fprint(buff, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 1000\r\n\r\n{\"partial\":")
					buff.Flush()
				case "invalid-json":
					w.Write([]byte(`not json`))
				case "invalid-utf8":
					w.Write(bytes.Replace(completionBody, []byte(`"content":"fixture"`), []byte{'"', 'c', 'o', 'n', 't', 'e', 'n', 't', '"', ':', '"', 0xff, '"'}, 1))
				case "wrong-role":
					w.Write([]byte(`{"choices":[{"message":{"role":"system"}}]}`))
				}
			}, publisherFunc(func(context.Context, modelmailbox.Envelope) error { published.Add(1); return nil }))
			result, err := dispatcher.Deliver(context.Background(), a.ID, profile, request)
			if !errors.Is(err, ErrUncertain) || !result.DispatchAttempted || result.HandoffConfirmed {
				t.Fatalf("failure: %+v %v", result, err)
			}
			parent, call, handed, count := ledgerState(t, dispatcher.DB, a)
			if parent != "stopped" || call != "ambiguous" || handed || count != 1 {
				t.Fatalf("failure ledger: %s %s %t %d", parent, call, handed, count)
			}
			request.Sequence = 2
			result, err = dispatcher.Deliver(context.Background(), a.ID, profile, request)
			if err == nil || result.DispatchAttempted || calls.Load() != 1 || published.Load() != 0 || redirected.Load() != 0 {
				t.Fatalf("retry escaped stop: %+v %v", result, err)
			}
		})
	}
}

func TestBrokerPublicationFailurePreservesCompletedResponseEvidence(t *testing.T) {
	for _, mutate := range []bool{false, true} {
		var calls atomic.Int32
		dispatcher, a, profile, request := brokerFixture(t, func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.Write(completionBody)
		}, publisherFunc(func(ctx context.Context, e modelmailbox.Envelope) error {
			if mutate {
				e.Payload[0] = '!'
				return nil
			}
			return errors.New("fixture publication unavailable")
		}))
		result, err := dispatcher.Deliver(context.Background(), a.ID, profile, request)
		if !errors.Is(err, ErrUncertain) || result.HandoffConfirmed || calls.Load() != 1 {
			t.Fatalf("publication: %+v %v", result, err)
		}
		parent, call, handed, count := ledgerState(t, dispatcher.DB, a)
		if parent != "stopped" || call != "ambiguous" || handed || count != 1 {
			t.Fatalf("publication ledger: %s %s %t %d", parent, call, handed, count)
		}
		var hash string
		if err := dispatcher.DB.Pool.QueryRow(`SELECT response_sha256 FROM model_calls WHERE allocation_id=$1`, a.ID).Scan(&hash); err != nil || hash != modelmailbox.Digest(completionBody) {
			t.Fatal("known response evidence lost")
		}
	}
}

func TestBrokerRejectsUnboundRequestsBeforeSpending(t *testing.T) {
	for _, kind := range []string{"profile", "pod", "attempt", "model", "tokens", "digest", "expired", "oversized"} {
		t.Run(kind, func(t *testing.T) {
			var calls atomic.Int32
			dispatcher, a, profile, request := brokerFixture(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }, publisherFunc(func(context.Context, modelmailbox.Envelope) error { return nil }))
			switch kind {
			case "profile":
				profile = append(profile, ' ')
			case "pod":
				request.PodUID = store.NewID()
			case "attempt":
				request.Attempt = "other-1"
			case "model":
				request.Payload = bytes.ReplaceAll(request.Payload, []byte("fixture-model"), []byte("other-model"))
				request.RequestSHA256 = modelmailbox.Digest(request.Payload)
			case "tokens":
				request.Payload = bytes.ReplaceAll(request.Payload, []byte(`"max_tokens":64`), []byte(`"max_tokens":65`))
				request.RequestSHA256 = modelmailbox.Digest(request.Payload)
			case "digest":
				request.RequestSHA256 = strings.Repeat("f", 64)
			case "expired":
				request.ExpiresAt = time.Now().Add(-time.Second).UnixMilli()
			case "oversized":
				request.Payload = bytes.Repeat([]byte("x"), modelmailbox.MaxRequestBytes+1)
			}
			result, err := dispatcher.Deliver(context.Background(), a.ID, profile, request)
			if !errors.Is(err, ErrRejected) || result.DispatchAttempted || calls.Load() != 0 {
				t.Fatalf("unbound call admitted: %+v %v", result, err)
			}
			var count int
			if err := dispatcher.DB.Pool.QueryRow(`SELECT reserved_calls FROM model_budgets WHERE work_item_uuid=$1`, a.WorkItem).Scan(&count); err != nil || count != 0 {
				t.Fatal("rejected request spent budget")
			}
		})
	}
}

func TestBrokerCancellationStopsPendingProviderCall(t *testing.T) {
	var calls, published atomic.Int32
	entered := make(chan struct{})
	dispatcher, a, profile, request := brokerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		close(entered)
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}, publisherFunc(func(context.Context, modelmailbox.Envelope) error { published.Add(1); return nil }))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := dispatcher.Deliver(ctx, a.ID, profile, request); done <- err }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("provider was not reached")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, ErrUncertain) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation did not bound dispatch")
	}
	parent, call, handed, _ := ledgerState(t, dispatcher.DB, a)
	if parent != "stopped" || call != "ambiguous" || handed || calls.Load() != 1 || published.Load() != 0 {
		t.Fatal("cancelled call was not safely stopped")
	}
}
