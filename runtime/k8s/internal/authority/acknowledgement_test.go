package authority

import (
	"testing"
	"time"
)

func TestAcknowledgementBindsCommittedResultAndTransport(t *testing.T) {
	p, ids := fixture(t)
	payload, result := []byte(`{"issue":1}`), []byte(`{"accepted":true}`)
	r, err := ids["intake"].Seal(p, "event-1", "admit-issue", payload)
	if err != nil {
		t.Fatal(err)
	}
	transport, err := ids["relay"].Carry(p, r, "POST", "/admit", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ack, err := ids["admission-controller"].Confirm(p, r, transport, result)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.VerifyAcknowledgement(ack, r, transport, result); err != nil {
		t.Fatal(err)
	}
	if _, err := ids["relay"].Confirm(p, r, transport, result); err == nil {
		t.Fatal("relay signed receiver acknowledgement")
	}
	for _, scenario := range []string{"nonce", "receipt", "result", "key", "receiver", "signature", "freeze"} {
		a, receipt, carriage, response, policy := ack, r, transport, result, p
		switch scenario {
		case "nonce":
			carriage.Nonce = "different-attempt"
		case "receipt":
			receipt.ID = "different-event"
		case "result":
			response = []byte(`{"accepted":false}`)
		case "key":
			a.KeyID = "relay-1"
		case "receiver":
			a.Receiver = "relay"
		case "signature":
			a.Signature = "invalid"
		case "freeze":
			policy.Freeze = true
		}
		if err := policy.VerifyAcknowledgement(a, receipt, carriage, response); err == nil {
			t.Fatalf("%s acknowledgement accepted", scenario)
		}
	}
}
