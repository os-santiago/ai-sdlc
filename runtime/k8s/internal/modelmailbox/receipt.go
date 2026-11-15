package modelmailbox

import (
	"bytes"
	"encoding/json"
	"errors"
)

const ReceiptVersion = "ai-sdlc.mailbox-publication/v1"
const MaxReceiptBytes = 2048

// PublicationReceipt confirms helper publication, not consumption by SCC.
// A transport EOF or a successful socket write is not this evidence.
type PublicationReceipt struct {
	Version        string `json:"version"`
	Attempt        string `json:"attempt"`
	PodUID         string `json:"podUID"`
	Sequence       uint64 `json:"sequence"`
	RequestSHA256  string `json:"requestSHA256"`
	ResponseSHA256 string `json:"responseSHA256"`
	Status         int    `json:"status"`
	Bytes          int    `json:"bytes"`
}

func receiptFor(e Envelope) PublicationReceipt {
	return PublicationReceipt{ReceiptVersion, e.Attempt, e.PodUID, e.Sequence, e.RequestSHA256, Digest(e.Payload), e.Status, len(e.Payload)}
}

func (s *Store) PublishResponse(id Identity, e Envelope) (PublicationReceipt, error) {
	if err := s.WriteResponse(id, e); err != nil {
		return PublicationReceipt{}, err
	}
	return receiptFor(e), nil
}

func ValidatePublicationReceipt(raw []byte, expected Envelope) error {
	reject := errors.New("mailbox publication receipt mismatch")
	if len(raw) == 0 || len(raw) > MaxReceiptBytes || expected.Validate(Identity{expected.Attempt, expected.PodUID}, "response") != nil {
		return reject
	}
	fields, err := objectFields(raw)
	if err != nil || len(fields) != 8 {
		return reject
	}
	for _, key := range []string{"version", "attempt", "podUID", "sequence", "requestSHA256", "responseSHA256", "status", "bytes"} {
		if value, ok := fields[key]; !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return reject
		}
	}
	var got PublicationReceipt
	if json.Unmarshal(raw, &got) != nil || got != receiptFor(expected) {
		return reject
	}
	return nil
}
