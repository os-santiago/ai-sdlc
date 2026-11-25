package execution

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelbroker"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelmailbox"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

// The last turn the signed review profile permits is sent to the provider
// with tool_choice "none" and the broker's fixed note, and a tool proposal
// there is refused; earlier turns are sent byte for byte. The journal records what was sent, the mailbox reply still
// binds the worker's own request, and a turn beyond the budget is named
// request-turn-budget, apart from binding failures.
func TestReviewFinalTurnRefusesToolsAndNamesTheBudget(t *testing.T) {
	for _, scenario := range []string{"final-verdict", "final-tools", "binding-model", "binding-digest"} {
		t.Run(scenario, func(t *testing.T) {
			profile := []byte(`{"version":"ai-sdlc.reviewer-profile/v1","model":"review-model","maxSteps":2,"maxSeconds":90,"maxTokens":2048,"aDevSHA256":"` + strings.Repeat("a", 64) + `"}`)
			f, db, run := reviewJournalFixture(t, authority.Digest(profile))
			c := claimReview(t, db, run)
			pod := startReviewFixture(t, db, c)
			var mu sync.Mutex
			var bodies [][]byte
			var replies []modelmailbox.Envelope
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, e := io.ReadAll(io.LimitReader(r.Body, 131073))
				if e != nil || len(raw) > 131072 {
					w.WriteHeader(400)
					return
				}
				mu.Lock()
				bodies = append(bodies, raw)
				n := len(bodies)
				mu.Unlock()
				message := map[string]any{"role": "assistant", "content": "Inspected source.\nVERDICT: approve"}
				finish := "stop"
				if n == 1 || scenario == "final-tools" {
					message["content"] = nil
					message["tool_calls"] = []any{map[string]any{"id": fmt.Sprintf("call-%d", n), "type": "function", "function": map[string]string{"name": "read_file", "arguments": `{"path":"src/a.js"}`}}}
					finish = "tool_calls"
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
				Authority: reviewAuthorityFunc(func(context.Context, store.ReviewRun, store.ReviewClaim) (time.Time, error) { return c.Until, nil }),
				Mailbox: reviewPublisherFunc(func(_ context.Context, envelope modelmailbox.Envelope) error {
					mu.Lock()
					replies = append(replies, envelope)
					mu.Unlock()
					return nil
				}),
			}
			tools := `"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object"}}}],"tool_choice":"auto"`
			request := func(sequence uint64, model string, podUID string) modelmailbox.Envelope {
				raw := []byte(`{"model":"` + model + `","max_tokens":2048,` + tools + `,"messages":[{"role":"system","content":"Review <candidate> & report"},{"role":"user","content":"Inspect source"}]}`)
				return modelmailbox.Envelope{Version: modelmailbox.Version, Kind: "request", Attempt: fmt.Sprintf("%s-1", run.WorkItem), PodUID: podUID, Sequence: sequence, RequestSHA256: modelmailbox.Digest(raw), ExpiresAt: time.Now().Add(time.Minute).UnixMilli(), Payload: raw}
			}
			stopped := func() {
				t.Helper()
				var state string
				if f.Admin.Pool.QueryRow(`SELECT state FROM review_runs WHERE id=$1`, run.ID).Scan(&state) != nil || state != "failed" {
					t.Fatal("refused review not stopped", state)
				}
			}
			switch scenario {
			case "binding-model", "binding-digest":
				first := request(1, "review-model", pod)
				first.RequestSHA256 = modelmailbox.Digest([]byte("{}"))
				if scenario == "binding-model" {
					first = request(1, "other-model", pod)
				}
				result, err := d.DeliverReview(context.Background(), c, profile, first)
				if err == nil || result.FailureStage != "request-binding" || result.DispatchAttempted || len(bodies) != 0 {
					t.Fatal("binding failure not named request-binding", result, err)
				}
				stopped()
				return
			}
			first, final := request(1, "review-model", pod), request(2, "review-model", pod)
			if result, err := d.DeliverReview(context.Background(), c, profile, first); err != nil || !result.HandoffConfirmed {
				t.Fatal("first review turn", result, err)
			}
			result, err := d.DeliverReview(context.Background(), c, profile, final)
			if len(bodies) != 2 || string(bodies[0]) != string(first.Payload) {
				t.Fatal("an earlier turn was changed")
			}
			var sent struct {
				Model     string            `json:"model"`
				MaxTokens int               `json:"max_tokens"`
				Tools     json.RawMessage   `json:"tools"`
				Choice    json.RawMessage   `json:"tool_choice"`
				Messages  []json.RawMessage `json:"messages"`
			}
			if err := json.Unmarshal(bodies[1], &sent); err != nil {
				t.Fatal(err)
			}
			note, _ := json.Marshal(struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			}{"user", modelbroker.ReviewFinalTurnNote})
			if string(sent.Tools) != `[{"type":"function","function":{"name":"read_file","parameters":{"type":"object"}}}]` || string(sent.Choice) != `"none"` || sent.Model != "review-model" || sent.MaxTokens != 2048 || len(sent.Messages) != 3 || string(sent.Messages[2]) != string(note) {
				t.Fatalf("final turn allowed tools or lost its note: %s", bodies[1])
			}
			if string(sent.Messages[0]) != `{"role":"system","content":"Review <candidate> & report"}` {
				t.Fatalf("final turn rewrote an earlier message: %s", sent.Messages[0])
			}
			if scenario == "final-tools" {
				// A tool proposal on the last turn is refused by the response
				// policy and never reaches the worker.
				if err == nil || result.FailureStage != "response-policy" || result.HandoffConfirmed || len(replies) != 1 {
					t.Fatal("final-turn tool proposal not refused", result, err)
				}
				stopped()
				return
			}
			if err != nil || !result.HandoffConfirmed {
				t.Fatal("final review turn", result, err)
			}
			// The journal records what the provider received; the worker's reply
			// still names the request the worker sent.
			evidence, err := db.ReadReviewModelEvidence(context.Background(), c)
			if err != nil || len(evidence) != 2 || evidence[0].RequestDigest != modelmailbox.Digest(first.Payload) || evidence[1].RequestDigest != modelmailbox.Digest(bodies[1]) || evidence[1].InputBytes != len(bodies[1]) {
				t.Fatal("journal does not record the sent final turn", evidence, err)
			}
			if len(replies) != 2 || replies[1].RequestSHA256 != final.RequestSHA256 || replies[1].Sequence != 2 {
				t.Fatal("reply no longer binds the worker's request")
			}
			var o modelbroker.ReviewObservation
			if err = json.Unmarshal(evidence[1].Observation, &o); err != nil || o.Verdict != "approve" {
				t.Fatal("final verdict not observed", o, err)
			}
			// A request beyond the budget is refused before any reservation.
			result, err = d.DeliverReview(context.Background(), c, profile, request(3, "review-model", pod))
			if err == nil || result.FailureStage != "request-turn-budget" || result.DispatchAttempted || len(bodies) != 2 {
				t.Fatal("turn beyond the budget not named request-turn-budget", result, err)
			}
			var rows, reserved int
			if f.Admin.Pool.QueryRow(`SELECT count(*) FROM review_model_calls WHERE run_id=$1 AND sequence=3`, run.ID).Scan(&rows) != nil || rows != 0 {
				t.Fatal("refused turn was reserved", rows)
			}
			if f.Admin.Pool.QueryRow(`SELECT reserved_calls FROM review_runs WHERE id=$1`, run.ID).Scan(&reserved) != nil || reserved != 2 {
				t.Fatal("refused turn changed the call budget", reserved)
			}
			stopped()
		})
	}
}
