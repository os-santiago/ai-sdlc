package modelbroker

import (
	"encoding/json"
	"testing"
)

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

func TestFinalReviewTurnPayloadWithholdsOnlyTools(t *testing.T) {
	raw := []byte(`{"model":"m","max_tokens":64,"temperature":0,"stream":false,"tool_choice":{"type":"function","function":{"name":"read_file"}},"tools":[{"type":"function","function":{"name":"read_file"}}],"messages":[{"role":"user","content":"a <b> & c"},{"role":"assistant","content":null,"tool_calls":[{"id":"x","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a\"}"}}]},{"role":"tool","tool_call_id":"x","content":"1"}]}`)
	sent, err := finalReviewTurnPayload(raw)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(sent, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["tools"]; ok {
		t.Fatal("tools still offered")
	}
	if _, ok := fields["tool_choice"]; ok {
		t.Fatal("tool_choice still sent")
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
	for _, bad := range []string{`{"model":"m","model":"n","messages":[{"role":"user"}]}`, `{"model":"m","messages":[]}`, `{"model":"m"}`, `[]`} {
		if _, err = finalReviewTurnPayload([]byte(bad)); err == nil {
			t.Fatal("accepted", bad)
		}
	}
}
