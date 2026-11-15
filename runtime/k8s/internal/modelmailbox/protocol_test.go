package modelmailbox

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var testIdentity = Identity{Attempt: "fixture-461", PodUID: "11111111-2222-3333-4444-555555555555"}
var testBody = []byte(`{"model":"fixture-model","stream":false,"messages":[{"role":"user","content":"hello\\nworld"}]}`)

func testRequest() Envelope {
	return Envelope{Version: Version, Kind: "request", Attempt: testIdentity.Attempt, PodUID: testIdentity.PodUID, Sequence: 1, RequestSHA256: Digest(testBody), Payload: append([]byte(nil), testBody...), ExpiresAt: time.Now().Add(time.Minute).UnixMilli()}
}
func testResponse(request Envelope) Envelope {
	request.Kind = "response"
	request.Status = 200
	request.Payload = []byte(`{"choices":[{"message":{"role":"assistant","content":"fixture"}}]}`)
	return request
}

func TestEnvelopeRoundTripPreservesExactBytes(t *testing.T) {
	e := testRequest()
	e.Payload = append([]byte(" \n"), e.Payload...)
	e.RequestSHA256 = Digest(e.Payload)
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(raw)
	if err != nil || decoded.Validate(testIdentity, "request") != nil || !bytes.Equal(decoded.Payload, e.Payload) {
		t.Fatal("envelope changed payload or identity", err)
	}
}

func TestEnvelopeRejectsAmbiguity(t *testing.T) {
	raw, _ := json.Marshal(testRequest())
	for name, value := range map[string]string{
		"duplicate":    strings.Replace(string(raw), `"kind":"request"`, `"kind":"response","kind":"request"`, 1),
		"case-variant": strings.Replace(string(raw), `"kind":`, `"Kind":`, 1),
		"unknown":      strings.Replace(string(raw), `"kind":`, `"extra":0,"kind":`, 1),
		"missing":      strings.Replace(string(raw), `"status":0,`, ``, 1),
		"null":         strings.Replace(string(raw), `"status":0`, `"status":null`, 1),
		"two-values":   string(raw) + "{}", "oversize": strings.Repeat(" ", MaxEnvelopeBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode([]byte(value)); err == nil {
				t.Fatal("accepted ambiguous envelope")
			}
		})
	}
}

func TestEnvelopeRejectsForgedOrExpiredIdentity(t *testing.T) {
	for name, change := range map[string]func(*Envelope){
		"attempt": func(e *Envelope) { e.Attempt = "other" }, "uid": func(e *Envelope) { e.PodUID = "22222222-2222-3333-4444-555555555555" },
		"digest": func(e *Envelope) { e.RequestSHA256 = strings.Repeat("0", 64) }, "payload": func(e *Envelope) { e.Payload = []byte(`{}`) },
		"version": func(e *Envelope) { e.Version = "v0" }, "sequence": func(e *Envelope) { e.Sequence = 0 },
		"quota": func(e *Envelope) { e.Sequence = MaxRequests + 1 }, "expired": func(e *Envelope) { e.ExpiresAt = time.Now().Add(-time.Second).UnixMilli() },
	} {
		t.Run(name, func(t *testing.T) {
			e := testRequest()
			change(&e)
			if e.Validate(testIdentity, "request") == nil {
				t.Fatal("accepted forged envelope")
			}
		})
	}
}

func TestRequestContract(t *testing.T) {
	for name, body := range map[string]string{
		"streaming":       `{"model":"m","messages":[{}],"stream":true}`,
		"url":             `{"model":"m","messages":[{}],"url":"https://example.invalid"}`,
		"duplicate-model": `{"model":"m","model":"n","messages":[{}]}`,
		"case-variant":    `{"Model":"m","messages":[{}]}`,
		"empty-messages":  `{"model":"m","messages":[]}`,
		"null-stream":     `{"model":"m","messages":[{}],"stream":null}`,
		"trailing":        string(testBody) + "{}", "array": `[]`, "oversize": strings.Repeat("a", MaxRequestBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			if ValidateRequest([]byte(body)) == nil {
				t.Fatal("accepted invalid request")
			}
		})
	}
}
