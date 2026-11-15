package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelbroker"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelmailbox"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/reviewresult"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

type reviewAuthorityFunc func(context.Context, store.ReviewRun, store.ReviewClaim) (time.Time, error)

func (f reviewAuthorityFunc) AuthorizeReview(ctx context.Context, r store.ReviewRun, c store.ReviewClaim) (time.Time, error) {
	return f(ctx, r, c)
}

type reviewPublisherFunc func(context.Context, modelmailbox.Envelope) error

func (f reviewPublisherFunc) Publish(ctx context.Context, e modelmailbox.Envelope) error {
	return f(ctx, e)
}
func TestReviewDispatcherRealJournal(t *testing.T) {
	for _, scenario := range []string{"approve", "mutation", "implicit", "revoked-before", "revoked-after", "lost-provider", "lost-publish", "mutated-publish", "lost-reservation", "lost-completion", "lost-ack"} {
		t.Run(scenario, func(t *testing.T) {
			profile := []byte(`{"version":"ai-sdlc.reviewer-profile/v1","model":"review-model","maxSteps":4,"maxSeconds":90,"maxTokens":2048,"aDevSHA256":"` + strings.Repeat("a", 64) + `"}`)
			f, db, run := reviewJournalFixture(t, authority.Digest(profile))
			c := claimReview(t, db, run)
			pod := startReviewFixture(t, db, c)
			var calls, publications atomic.Int32
			var revoked atomic.Bool
			if scenario == "revoked-before" {
				revoked.Store(true)
			}
			final := "Inspected source.\nVERDICT: approve"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Drain the bounded request before responding; this exercises real HTTP
				// without inducing a server-side unread-body connection reset.
				raw, e := io.ReadAll(io.LimitReader(r.Body, 131073))
				if e != nil || len(raw) > 131072 {
					w.WriteHeader(400)
					return
				}
				n := calls.Add(1)
				if scenario == "lost-provider" {
					w.WriteHeader(503)
					return
				}
				if scenario == "revoked-after" {
					revoked.Store(true)
				}
				message := map[string]any{"role": "assistant", "content": final}
				finish := "stop"
				if n == 1 {
					name, args := "read_file", `{"path":"src/a.js"}`
					if scenario == "mutation" {
						name, args = "run_shell", `{"command":"true"}`
					}
					message["content"] = nil
					message["tool_calls"] = []any{map[string]any{"id": "call-1", "type": "function", "function": map[string]string{"name": name, "arguments": args}}}
					finish = "tool_calls"
				}
				if n == 2 && scenario == "implicit" {
					message["content"] = "Looks good"
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"model": "review-model", "choices": []any{map[string]any{"index": 0, "finish_reason": finish, "message": message}}})
			}))
			defer server.Close()
			provider, e := modelbroker.NewProvider(modelbroker.ProviderConfig{Endpoint: server.URL + "/v1/chat/completions", BearerToken: strings.Repeat("x", 48), AllowLoopbackHTTP: true})
			if e != nil {
				t.Fatal(e)
			}
			d := modelbroker.ReviewDispatcher{DB: db, Provider: provider, ConfigurationDigest: run.ConfigurationDigest,
				Authority: reviewAuthorityFunc(func(context.Context, store.ReviewRun, store.ReviewClaim) (time.Time, error) {
					if revoked.Load() {
						return time.Time{}, errors.New("revoked")
					}
					return c.Until, nil
				}),
				Mailbox: reviewPublisherFunc(func(_ context.Context, envelope modelmailbox.Envelope) error {
					publications.Add(1)
					if scenario == "lost-publish" {
						return errors.New("lost reply")
					}
					if scenario == "mutated-publish" {
						envelope.Payload[0] = 'x'
					}
					return nil
				}),
			}
			var dropped <-chan struct{}
			// Each authority check commits its claim observation transaction. Fault the
			// real reservation/completion/ACK COMMIT, not a fake DB method.
			switch scenario {
			case "lost-reservation":
				d.DB, dropped = executionCommitFault(t, 2, "verifier")
			case "lost-completion":
				d.DB, dropped = executionCommitFault(t, 5, "verifier")
			case "lost-ack":
				d.DB, dropped = executionCommitFault(t, 8, "verifier")
			}
			request := func(sequence uint64) modelmailbox.Envelope {
				raw := []byte(`{"model":"review-model","max_tokens":2048,"messages":[{"role":"user","content":"Inspect source"}]}`)
				return modelmailbox.Envelope{Version: modelmailbox.Version, Kind: "request", Attempt: fmt.Sprintf("%s-1", run.WorkItem), PodUID: pod, Sequence: sequence, RequestSHA256: modelmailbox.Digest(raw), ExpiresAt: time.Now().Add(time.Minute).UnixMilli(), Payload: raw}
			}
			result, err := d.DeliverReview(context.Background(), c, profile, request(1))
			if scenario == "approve" || scenario == "implicit" {
				if err != nil || !result.HandoffConfirmed {
					t.Fatal("read handoff", result, err)
				}
				result, err = d.DeliverReview(context.Background(), c, profile, request(2))
			}
			if scenario == "approve" {
				evidence, e := db.ReadReviewModelEvidence(context.Background(), c)
				if err != nil || !result.HandoffConfirmed || e != nil || len(evidence) != 2 || calls.Load() != 2 || publications.Load() != 2 {
					t.Fatal("review delivery", result, err, e)
				}
				if modelbroker.MatchReviewSummary(reviewresult.Result{Verdict: "approve", FinalMessage: final, Iterations: 2, ReadCalls: 1}, evidence) != nil {
					t.Fatal("independent evidence mismatch")
				}
				if _, e = d.DeliverReview(context.Background(), c, profile, request(3)); e == nil || calls.Load() != 2 {
					t.Fatal("conversation continued after final verdict")
				}
			} else {
				if err == nil || result.HandoffConfirmed {
					t.Fatal("unsafe delivery accepted", result)
				}
				var state string
				if f.Admin.Pool.QueryRow(`SELECT state FROM review_runs WHERE id=$1`, run.ID).Scan(&state) != nil || state != "failed" {
					t.Fatal("unsafe parent not stopped", state)
				}
				before := calls.Load()
				d.DB = db
				if _, e = d.DeliverReview(context.Background(), c, profile, request(1)); e == nil || calls.Load() != before {
					t.Fatal("uncertain attempt replayed")
				}
				if _, e = db.ReadReviewModelEvidence(context.Background(), c); e == nil {
					t.Fatal("unsafe attempt became evidence")
				}
				if (scenario == "mutation" || scenario == "revoked-after" || scenario == "lost-completion") && publications.Load() != 0 {
					t.Fatal("unsafe provider response reached worker")
				}
				if (scenario == "revoked-before" || scenario == "lost-reservation") && calls.Load() != 0 {
					t.Fatal("unclaimed provider call")
				}
			}
			if dropped != nil {
				select {
				case <-dropped:
				case <-time.After(time.Second):
					t.Fatal("COMMIT reply not dropped")
				}
			}
		})
	}
}
