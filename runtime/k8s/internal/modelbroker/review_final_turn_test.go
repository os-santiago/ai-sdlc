package modelbroker

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// The gateway adapter tests replay this exact final turn.
func TestFinalReviewTurnNoteMatchesTheAdapterTests(t *testing.T) {
	raw, err := os.ReadFile("../../tools/test_model_gateway.py")
	if err != nil || !strings.Contains(string(raw), `FINAL_TURN_NOTE = "`+ReviewFinalTurnNote+`"`) {
		t.Fatal("adapter tests do not replay ReviewFinalTurnNote", err)
	}
}

func TestFinalReviewTurnIsTheLastPermittedStep(t *testing.T) {
	for _, tc := range []struct {
		sequence uint64
		steps    int
		want     bool
	}{{8, 8, true}, {2, 2, true}, {7, 8, false}, {1, 8, false}, {9, 8, false}, {1, 1, false}} {
		if finalReviewTurn(tc.sequence, tc.steps) != tc.want {
			t.Fatal(tc)
		}
	}
}

func TestFinalReviewTurnPayloadKeepsToolsAndRefusesThem(t *testing.T) {
	raw := []byte(`{"model":"m","max_tokens":64,"temperature":0,"stream":false,"tool_choice":{"type":"function","function":{"name":"read_file"}},"tools":[{"type":"function","function":{"name":"read_file"}}],"messages":[{"role":"user","content":"a <b> & c"},{"role":"assistant","content":null,"tool_calls":[{"id":"x","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a\"}"}}]},{"role":"tool","tool_call_id":"x","content":"1"}]}`)
	sent, err := finalReviewTurnPayload(raw)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(sent, &fields); err != nil {
		t.Fatal(err)
	}
	// Tool definitions stay for the tool history; the model may not call one.
	if string(fields["tools"]) != `[{"type":"function","function":{"name":"read_file"}}]` || string(fields["tool_choice"]) != `"none"` {
		t.Fatalf("final turn tools: %s", sent)
	}
	if string(fields["model"]) != `"m"` || string(fields["max_tokens"]) != "64" || string(fields["temperature"]) != "0" || string(fields["stream"]) != "false" {
		t.Fatalf("other fields changed: %s", sent)
	}
	var messages []json.RawMessage
	if err = json.Unmarshal(fields["messages"], &messages); err != nil || len(messages) != 4 {
		t.Fatal("messages", err)
	}
	if string(messages[0]) != `{"role":"user","content":"a <b> & c"}` || string(messages[2]) != `{"role":"tool","tool_call_id":"x","content":"1"}` {
		t.Fatalf("earlier messages changed: %s", sent)
	}
	var note struct{ Role, Content string }
	if json.Unmarshal(messages[3], &note) != nil || note.Role != "user" || note.Content != ReviewFinalTurnNote {
		t.Fatalf("final note missing: %s", messages[3])
	}
	// Without tools there is no tool_choice (some providers refuse one).
	plain, err := finalReviewTurnPayload([]byte(`{"model":"m","tool_choice":"auto","messages":[{"role":"user","content":"x"}]}`))
	if err != nil || string(plain) != `{"messages":[{"role":"user","content":"x"},{"role":"user","content":"`+ReviewFinalTurnNote+`"}],"model":"m"}` {
		t.Fatalf("plain final turn: %s %v", plain, err)
	}
	for _, bad := range []string{`{"model":"m","model":"n","messages":[{"role":"user"}]}`, `{"model":"m","messages":[]}`, `{"model":"m"}`, `[]`, `{"model":"m","tools":{},"messages":[{"role":"user"}]}`} {
		if _, err = finalReviewTurnPayload([]byte(bad)); err == nil {
			t.Fatal("accepted", bad)
		}
	}
}
