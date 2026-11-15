package modelmailbox

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPublicationReceiptRequiresSuccessfulExclusiveWrite(t *testing.T) {
	s, _ := newTestStore(t)
	request := testRequest()
	reply := request
	reply.Kind = "response"
	reply.Status = 200
	reply.Payload = []byte(`{"choices":[]}`)
	if got, err := s.PublishResponse(testIdentity, reply); err == nil || got.Version != "" {
		t.Fatal("receipt without pending request")
	}
	if err := s.publish(requestFile, request); err != nil {
		t.Fatal(err)
	}
	receipt, err := s.PublishResponse(testIdentity, reply)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(receipt)
	if ValidatePublicationReceipt(raw, reply) != nil {
		t.Fatal("valid publication receipt rejected")
	}
	if got, err := s.PublishResponse(testIdentity, reply); err == nil || got.Version != "" {
		t.Fatal("duplicate publication received new receipt")
	}
	for _, mutated := range []string{
		string(raw) + `{}`, strings.Replace(string(raw), `"bytes":`, `"Bytes":`, 1), strings.Replace(string(raw), `"status":200`, `"status":200,"status":200`, 1), strings.Replace(string(raw), `"status":200`, `"status":null`, 1), strings.Replace(string(raw), receipt.ResponseSHA256, strings.Repeat("0", 64), 1),
	} {
		if ValidatePublicationReceipt([]byte(mutated), reply) == nil {
			t.Fatal("ambiguous or changed receipt accepted")
		}
	}
	for _, change := range []func(*Envelope){func(e *Envelope) { e.PodUID = "22222222-2222-4222-8222-222222222222" }, func(e *Envelope) { e.Attempt = "other" }, func(e *Envelope) { e.Sequence++ }, func(e *Envelope) { e.RequestSHA256 = strings.Repeat("0", 64) }, func(e *Envelope) { e.Status = 503 }, func(e *Envelope) { e.Payload = []byte(`{}`) }} {
		changed := reply
		change(&changed)
		if ValidatePublicationReceipt(raw, changed) == nil {
			t.Fatal("receipt did not bind exact response")
		}
	}
}
