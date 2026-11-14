package authority

import (
	"encoding/json"
	"errors"
)

// Acknowledgement is emitted only after the receiver commits its inbox/effect.
// A plain HTTP 200, proxy response or replayed acknowledgement is insufficient
// for the relay to mark a fact delivered.
type Acknowledgement struct {
	Version        string `json:"version"`
	KeyID          string `json:"keyId"`
	Receiver       string `json:"receiver"`
	PolicyRevision string `json:"policyRevision"`
	ReceiptDigest  string `json:"receiptDigest"`
	TransportNonce string `json:"transportNonce"`
	ResultDigest   string `json:"resultDigest"`
	Signature      string `json:"signature"`
}

func acknowledgementBytes(ack Acknowledgement) []byte {
	ack.Signature = ""
	raw, _ := json.Marshal(ack)
	return append([]byte("ai-sdlc/ack/v1\x00"), raw...)
}

func (i Identity) Confirm(p Policy, receipt Receipt, transport Transport, result []byte) (Acknowledgement, error) {
	if i.Role != receipt.Target || len(result) == 0 || len(result) > 1024*1024 {
		return Acknowledgement{}, errors.New("acknowledgement identity or result denied")
	}
	if err := p.Allows(receipt.Producer, receipt.Target, receipt.Operation, receipt.Repository, receipt.Environment); err != nil {
		return Acknowledgement{}, err
	}
	raw, _ := json.Marshal(receipt)
	ack := Acknowledgement{Version: "1", KeyID: i.KeyID, Receiver: i.Role, PolicyRevision: p.Revision(), ReceiptDigest: Digest(raw), TransportNonce: transport.Nonce, ResultDigest: Digest(result)}
	var err error
	ack.Signature, err = i.sign(acknowledgementBytes(ack))
	if err == nil {
		err = p.VerifyAcknowledgement(ack, receipt, transport, result)
	}
	return ack, err
}

func (p Policy) VerifyAcknowledgement(ack Acknowledgement, receipt Receipt, transport Transport, result []byte) error {
	raw, _ := json.Marshal(receipt)
	if len(result) == 0 || len(result) > 1024*1024 || ack.Version != "1" || ack.Receiver != receipt.Target || ack.PolicyRevision != p.Revision() || ack.ReceiptDigest != Digest(raw) || ack.TransportNonce != transport.Nonce || ack.ResultDigest != Digest(result) {
		return errors.New("acknowledgement context mismatch")
	}
	if err := p.Allows(receipt.Producer, receipt.Target, receipt.Operation, receipt.Repository, receipt.Environment); err != nil {
		return err
	}
	key, err := p.verificationKey(ack.KeyID, ack.Receiver)
	if err != nil {
		return err
	}
	return verify(key, acknowledgementBytes(ack), ack.Signature)
}
