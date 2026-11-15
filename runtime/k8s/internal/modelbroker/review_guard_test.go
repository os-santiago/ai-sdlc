package modelbroker

import (
	"encoding/json"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/reviewresult"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"strings"
	"testing"
)

func reviewResponse(name, args, message string) []byte {
	m := map[string]any{"role": "assistant", "content": message}
	finish := "stop"
	if name != "" {
		m["content"] = nil
		m["tool_calls"] = []any{map[string]any{"id": "call-1", "type": "function", "function": map[string]string{"name": name, "arguments": args}}}
		finish = "tool_calls"
	}
	raw, _ := json.Marshal(map[string]any{"model": "review-model", "choices": []any{map[string]any{"index": 0, "finish_reason": finish, "message": m}}})
	return raw
}
func TestReviewResponseGuard(t *testing.T) {
	for _, tc := range []struct {
		name, args string
		valid      bool
	}{
		{"read_file", `{"path":"src/utils.js"}`, true},
		{"read_file", `{"path":"src/utils.js","offset":2,"limit":40}`, true},
		{"list_dir", `{}`, true}, {"list_dir", `{"path":"."}`, true},
		{"search_text", `{"pattern":"Date","glob":"src/**/*.js","regex":false}`, true},
		{"code_query", `{"path":"src/utils.js"}`, true},
		{"run_shell", `{"command":"true"}`, false}, {"write_file", `{"path":"a","content":"changed"}`, false},
		{"read_file", `{"path":"/etc/passwd"}`, false}, {"read_file", `{"path":"../auth.json"}`, false},
		{"read_file", `{"path":"src/../auth.json"}`, false}, {"read_file", `{"path":".git/config"}`, false},
		{"read_file", `{"path":"a","path":"b"}`, false}, {"read_file", `{"Path":"a"}`, false},
		{"read_file", `{"path":"a","command":"true"}`, false}, {"read_file", `{"path":"a","limit":-1}`, false},
		{"read_file", `{"path":"a","offset":null}`, false}, {"read_file", `{"path":"a","offset":1.5}`, false},
		{"search_text", `{"pattern":"(a+)+","path":"src/*.js","regex":true}`, false},
		{"search_text", `{"pattern":"x","glob":"/etc/*"}`, false},
		{"search_text", `{"pattern":"x","glob":"{src,../etc}/*"}`, false},
		{"search_text", `{"pattern":"x","path":"a","glob":"b"}`, false},
	} {
		t.Run(tc.name+tc.args, func(t *testing.T) {
			o, e := ObserveReviewResponse(reviewResponse(tc.name, tc.args, ""), "review-model")
			if (e == nil) != tc.valid {
				t.Fatal("guard result", o, e)
			}
		})
	}
	for _, verdict := range []string{"approve", "request_changes"} {
		message := "Inspected implementation.\nVERDICT: " + verdict
		o, e := ObserveReviewResponse(reviewResponse("", "", message), "review-model")
		if e != nil || o.Verdict != verdict || o.FinalSHA256 != authority.Digest([]byte(message)) {
			t.Fatal("explicit verdict", e)
		}
	}
	valid := string(reviewResponse("", "", "VERDICT: approve"))
	for _, raw := range []string{
		strings.Replace(valid, `"stop"`, `"length"`, 1), strings.Replace(valid, `"index":0`, `"index":1`, 1),
		strings.Replace(valid, `"role":"assistant"`, `"role":"system"`, 1),
		strings.Replace(valid, `"role":"assistant"`, `"role":"assistant","function_call":{}`, 1),
		strings.Replace(valid, `"role":"assistant"`, `"role":"assistant","Role":"assistant"`, 1),
		strings.Replace(valid, `"model":"review-model"`, `"model":"other"`, 1),
		string(reviewResponse("", "", "Looks good")), string(reviewResponse("", "", "VERDICT: approve\nVERDICT: approve")),
	} {
		if _, e := ObserveReviewResponse([]byte(raw), "review-model"); e == nil {
			t.Fatal("unsafe response accepted", raw)
		}
	}
}
func TestReviewSummaryMustMatchIndependentEvidence(t *testing.T) {
	message := "Inspected source.\nVERDICT: approve"
	responses := [][]byte{reviewResponse("read_file", `{"path":"src/a.js"}`, ""), reviewResponse("", "", message)}
	evidence := []store.ReviewModelEvidence{}
	for i, raw := range responses {
		o, e := ObserveReviewResponse(raw, "review-model")
		if e != nil {
			t.Fatal(e)
		}
		obs, _ := json.Marshal(o)
		evidence = append(evidence, store.ReviewModelEvidence{Sequence: i + 1, RequestDigest: authority.Digest([]byte("request")), ResponseDigest: authority.Digest(raw), InputBytes: 20, OutputBytes: len(raw), Observation: obs})
	}
	summary := reviewresult.Result{Verdict: "approve", FinalMessage: message, Iterations: 2, ReadCalls: 1}
	if MatchReviewSummary(summary, evidence) != nil {
		t.Fatal("valid evidence")
	}
	for _, mutate := range []func(*reviewresult.Result){func(s *reviewresult.Result) { s.Verdict = "request_changes" }, func(s *reviewresult.Result) { s.FinalMessage = "Different inspection.\nVERDICT: approve" }, func(s *reviewresult.Result) { s.ReadCalls = 2 }, func(s *reviewresult.Result) { s.Iterations = 3 }} {
		s := summary
		mutate(&s)
		if MatchReviewSummary(s, evidence) == nil {
			t.Fatal("forged summary matched")
		}
	}
	evidence[0].Observation = evidence[1].Observation
	if MatchReviewSummary(summary, evidence) == nil {
		t.Fatal("premature verdict matched")
	}
}

func TestReviewReadToolsPermitBoundedNonterminalCommentary(t *testing.T) {
	raw := string(reviewResponse("read_file", `{"path":"src/a.js"}`, ""))
	for _, tc := range []struct {
		content string
		valid   bool
	}{{"I will inspect the implementation and tests.", true}, {"VERDICT: approve", false}, {strings.Repeat("a", 4000), false}, {"bad\rtext", false}} {
		content, _ := json.Marshal(tc.content)
		response := strings.Replace(raw, `"content":null`, `"content":`+string(content), 1)
		if _, e := ObserveReviewResponse([]byte(response), "review-model"); (e == nil) != tc.valid {
			t.Fatal("commentary policy", tc.valid, e)
		}
	}
}
