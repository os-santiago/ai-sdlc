// Package modelmailbox transports untrusted model data without granting network
// or credential access to an executor. It does not authorize provider calls.
package modelmailbox

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"time"
)

const (
	Version          = "ai-sdlc.model-mailbox/v1"
	MaxRequestBytes  = 128 * 1024
	MaxResponseBytes = 256 * 1024
	MaxEnvelopeBytes = MaxResponseBytes*4/3 + 4096
	MaxRequests      = 32
	// Generator tool calls (Tekton catalog runs on the candidate tree): the
	// request carries the candidate diff, the response a bounded summary.
	MaxToolRequestBytes  = 800 * 1024
	MaxToolResponseBytes = 16 * 1024
	MaxToolEnvelopeBytes = MaxToolRequestBytes*4/3 + 4096
	MaxToolCalls         = 16
)

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,127}$`)
var uidPattern = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)

type Identity struct{ Attempt, PodUID string }

func (id Identity) Validate() error {
	if !idPattern.MatchString(id.Attempt) || !uidPattern.MatchString(id.PodUID) {
		return errors.New("invalid mailbox identity")
	}
	return nil
}

// Payload is base64 in JSON so its exact bytes survive envelope serialization.
// RequestSHA256 binds the request bytes for both request and response envelopes.
type Envelope struct {
	Version       string `json:"version"`
	Kind          string `json:"kind"`
	Attempt       string `json:"attempt"`
	PodUID        string `json:"podUID"`
	Sequence      uint64 `json:"sequence"`
	RequestSHA256 string `json:"requestSHA256"`
	Status        int    `json:"status"`
	ExpiresAt     int64  `json:"expiresAt"`
	Payload       []byte `json:"payload"`
}

func Digest(body []byte) string { sum := sha256.Sum256(body); return hex.EncodeToString(sum[:]) }

func (e Envelope) Validate(id Identity, kind string) error {
	if id.Validate() != nil || e.Version != Version || e.Kind != kind || e.Attempt != id.Attempt || e.PodUID != id.PodUID || e.Sequence == 0 || e.Sequence > MaxRequests || e.ExpiresAt <= time.Now().UnixMilli() {
		return errors.New("mailbox envelope identity mismatch")
	}
	digest, err := hex.DecodeString(e.RequestSHA256)
	if err != nil || len(digest) != 32 || hex.EncodeToString(digest) != e.RequestSHA256 {
		return errors.New("invalid mailbox request digest")
	}
	if kind == "request" {
		if e.Status != 0 || Digest(e.Payload) != e.RequestSHA256 || ValidateRequest(e.Payload) != nil {
			return errors.New("invalid mailbox request")
		}
	} else if kind == "response" {
		if len(e.Payload) == 0 || len(e.Payload) > MaxResponseBytes || !json.Valid(e.Payload) {
			return errors.New("invalid mailbox response")
		}
		switch e.Status {
		case 200, 400, 401, 403, 429, 500, 502, 503, 504:
		default:
			return errors.New("unsupported mailbox response status")
		}
	} else if kind == "tool-request" {
		if e.Status != 0 || e.Sequence > MaxToolCalls || Digest(e.Payload) != e.RequestSHA256 || ValidateToolRequest(e.Payload) != nil {
			return errors.New("invalid mailbox tool request")
		}
	} else if kind == "tool-response" {
		if e.Sequence > MaxToolCalls || len(e.Payload) == 0 || len(e.Payload) > MaxToolResponseBytes || !json.Valid(e.Payload) {
			return errors.New("invalid mailbox tool response")
		}
		switch e.Status {
		case 200, 400, 403, 429, 500, 504:
		default:
			return errors.New("unsupported mailbox tool response status")
		}
	} else {
		return errors.New("unsupported mailbox envelope kind")
	}
	return nil
}

// ValidateToolRequest checks the shape of a generator tool call: an object
// with only tool, target, packages and diff. The controller authorizes it
// against the manifest; the mailbox only bounds and transports it.
func ValidateToolRequest(raw []byte) error {
	if len(raw) == 0 || len(raw) > MaxToolRequestBytes {
		return errors.New("invalid tool request size")
	}
	fields, err := objectFields(raw)
	if err != nil {
		return errors.New("invalid tool request")
	}
	allowed := map[string]bool{"tool": true, "target": true, "packages": true, "diff": true}
	for key := range fields {
		if !allowed[key] {
			return errors.New("unsupported tool request field")
		}
	}
	var tool, target string
	if json.Unmarshal(fields["tool"], &tool) != nil || len(tool) == 0 || len(tool) > 64 ||
		json.Unmarshal(fields["target"], &target) != nil || len(target) == 0 || len(target) > 256 {
		return errors.New("invalid tool request tool or target")
	}
	return nil
}

// Decode rejects duplicate, case-variant, unknown and missing envelope fields.
func Decode(raw []byte) (Envelope, error) { return DecodeWithin(raw, MaxEnvelopeBytes) }

// DecodeWithin is Decode with an explicit size bound (tool requests are larger).
func DecodeWithin(raw []byte, limit int) (Envelope, error) {
	var e Envelope
	if len(raw) > limit {
		return e, errors.New("mailbox envelope too large")
	}
	fields, err := objectFields(raw)
	if err != nil || len(fields) != 9 {
		return e, errors.New("invalid mailbox envelope fields")
	}
	for _, key := range []string{"version", "kind", "attempt", "podUID", "sequence", "requestSHA256", "status", "payload", "expiresAt"} {
		if value, ok := fields[key]; !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return e, errors.New("invalid mailbox envelope fields")
		}
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		return e, errors.New("invalid mailbox envelope")
	}
	return e, nil
}

func ValidateRequest(raw []byte) error {
	if len(raw) == 0 || len(raw) > MaxRequestBytes {
		return errors.New("invalid mailbox request size")
	}
	fields, err := objectFields(raw)
	if err != nil {
		return errors.New("invalid chat request")
	}
	allowed := map[string]bool{"model": true, "messages": true, "temperature": true, "stream": true, "tools": true, "max_tokens": true, "tool_choice": true}
	for key := range fields {
		if !allowed[key] {
			return errors.New("unsupported chat request field")
		}
	}
	var model string
	var messages []json.RawMessage
	if json.Unmarshal(fields["model"], &model) != nil || len(model) == 0 || len(model) > 128 || json.Unmarshal(fields["messages"], &messages) != nil || len(messages) == 0 || len(messages) > 256 {
		return errors.New("invalid chat request model or messages")
	}
	if value, ok := fields["stream"]; ok && !bytes.Equal(bytes.TrimSpace(value), []byte("false")) {
		return errors.New("mailbox supports non-streaming requests only")
	}
	return nil
}

func objectFields(raw []byte) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("expected JSON object")
	}
	fields := map[string]json.RawMessage{}
	for d.More() {
		token, err = d.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, errors.New("invalid JSON key")
		}
		if _, exists := fields[key]; exists {
			return nil, errors.New("duplicate JSON key")
		}
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return nil, err
		}
		fields[key] = value
	}
	if token, err = d.Token(); err != nil || token != json.Delim('}') {
		return nil, errors.New("invalid JSON object")
	}
	if _, err = d.Token(); err != io.EOF {
		return nil, errors.New("expected one JSON object")
	}
	return fields, nil
}
