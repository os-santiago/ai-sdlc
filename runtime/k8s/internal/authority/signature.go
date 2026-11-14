package authority

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

// Identity must be private to its owning component, never an executor mount.
type Identity struct {
	KeyID      string `json:"keyId"`
	Role       string `json:"role"`
	PrivateKey string `json:"privateKey"`
}

func NewIdentity(role, keyID string) (Identity, PublicKey, error) {
	if !roles[role] || !keyIDPattern.MatchString(keyID) {
		return Identity{}, PublicKey{}, errors.New("invalid identity")
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Identity{}, PublicKey{}, err
	}
	return Identity{keyID, role, base64.StdEncoding.EncodeToString(private)}, PublicKey{role, base64.StdEncoding.EncodeToString(public)}, nil
}

func Digest(payload []byte) string {
	hash := sha256.Sum256(payload)
	return hex.EncodeToString(hash[:])
}

// Receipt identifies a durable fact. Its identity never expires because the
// transport was unavailable; current policy and key revocation still apply.
// Store Payload as bytea, not JSONB, alongside this envelope.
type Receipt struct {
	Version       string `json:"version"`
	PolicyVersion string `json:"policyVersion"`
	ID            string `json:"id"`
	KeyID         string `json:"keyId"`
	Producer      string `json:"producer"`
	Target        string `json:"target"`
	Operation     string `json:"operation"`
	Repository    string `json:"repository"`
	Environment   string `json:"environment"`
	PayloadDigest string `json:"payloadDigest"`
	Signature     string `json:"signature"`
}

func receiptBytes(receipt Receipt) []byte {
	receipt.Signature = ""
	raw, _ := json.Marshal(receipt)
	return append([]byte("ai-sdlc/event/v1\x00"), raw...)
}

func (i Identity) sign(raw []byte) (string, error) {
	key, err := base64.StdEncoding.Strict().DecodeString(i.PrivateKey)
	if err != nil || len(key) != ed25519.PrivateKeySize || i.KeyID == "" || !roles[i.Role] {
		return "", errors.New("invalid signing identity")
	}
	return base64.StdEncoding.EncodeToString(ed25519.Sign(ed25519.PrivateKey(key), raw)), nil
}

func verify(key ed25519.PublicKey, raw []byte, signature string) error {
	sig, err := base64.StdEncoding.Strict().DecodeString(signature)
	if err != nil || !ed25519.Verify(key, raw, sig) {
		return errors.New("signature verification failed")
	}
	return nil
}

func (i Identity) Seal(policy Policy, id, operation string, payload []byte) (Receipt, error) {
	_, target, err := OperationOwner(operation)
	if err != nil {
		return Receipt{}, err
	}
	if id == "" || len(payload) == 0 || len(payload) > 1024*1024 {
		return Receipt{}, errors.New("invalid event identity or payload size")
	}
	if err := policy.Allows(i.Role, target, operation, policy.Repository, policy.Environment); err != nil {
		return Receipt{}, err
	}
	key, err := policy.verificationKey(i.KeyID, i.Role)
	if err != nil {
		return Receipt{}, err
	}
	receipt := Receipt{Version: "1", PolicyVersion: policy.Version, ID: id, KeyID: i.KeyID, Producer: i.Role, Target: target, Operation: operation, Repository: policy.Repository, Environment: policy.Environment, PayloadDigest: Digest(payload)}
	receipt.Signature, err = i.sign(receiptBytes(receipt))
	if err == nil {
		err = verify(key, receiptBytes(receipt), receipt.Signature)
	}
	return receipt, err
}

func (p Policy) VerifyReceipt(receipt Receipt, target, operation string, payload []byte) error {
	if len(payload) == 0 || len(payload) > 1024*1024 {
		return errors.New("invalid payload size")
	}
	if receipt.Version != "1" || receipt.PolicyVersion != p.Version || receipt.ID == "" || receipt.Target != target || receipt.Operation != operation || receipt.PayloadDigest != Digest(payload) {
		return errors.New("event identity, audience, operation or payload mismatch")
	}
	if err := p.Allows(receipt.Producer, target, operation, receipt.Repository, receipt.Environment); err != nil {
		return err
	}
	key, err := p.verificationKey(receipt.KeyID, receipt.Producer)
	if err != nil {
		return err
	}
	return verify(key, receiptBytes(receipt), receipt.Signature)
}

// Transport is a short-lived authorization to carry one specific signed fact.
// The relay owns its transport key but cannot create a producer's receipt.
type Transport struct {
	Version        string `json:"version"`
	PolicyRevision string `json:"policyRevision"`
	KeyID          string `json:"keyId"`
	Caller         string `json:"caller"`
	Target         string `json:"target"`
	Method         string `json:"method"`
	Path           string `json:"path"`
	Repository     string `json:"repository"`
	Environment    string `json:"environment"`
	ReceiptDigest  string `json:"receiptDigest"`
	IssuedAt       int64  `json:"issuedAt"`
	Nonce          string `json:"nonce"`
	Signature      string `json:"signature"`
}

func transportBytes(transport Transport) []byte {
	transport.Signature = ""
	raw, _ := json.Marshal(transport)
	return append([]byte("ai-sdlc/transport/v1\x00"), raw...)
}

func (i Identity) Carry(p Policy, receipt Receipt, method, path string, now time.Time) (Transport, error) {
	if i.Role != "relay" && i.Role != receipt.Producer {
		return Transport{}, errors.New("caller cannot transport this event")
	}
	if _, err := p.verificationKey(i.KeyID, i.Role); err != nil {
		return Transport{}, err
	}
	nonce := make([]byte, 24)
	if _, err := rand.Read(nonce); err != nil {
		return Transport{}, err
	}
	raw, _ := json.Marshal(receipt)
	transport := Transport{Version: "1", PolicyRevision: p.Revision(), KeyID: i.KeyID, Caller: i.Role, Target: receipt.Target, Method: method, Path: path, Repository: receipt.Repository, Environment: receipt.Environment, ReceiptDigest: Digest(raw), IssuedAt: now.Unix(), Nonce: hex.EncodeToString(nonce)}
	var err error
	transport.Signature, err = i.sign(transportBytes(transport))
	return transport, err
}

// VerifyTransport does not consume the nonce. The receiver MUST atomically
// claim it in a durable inbox before any side effect; memory-only replay
// caches are insufficient. The operation's durable receipt ID also dedupes
// legitimate retries using a freshly signed transport authorization.
func (p Policy) VerifyTransport(t Transport, receipt Receipt, method, path string, now time.Time) error {
	raw, _ := json.Marshal(receipt)
	if t.Version != "1" || t.PolicyRevision != p.Revision() || t.Target != receipt.Target || t.Method != method || t.Path != path || t.Repository != p.Repository || t.Environment != p.Environment || t.ReceiptDigest != Digest(raw) {
		return errors.New("transport context mismatch")
	}
	if t.Caller != "relay" && t.Caller != receipt.Producer {
		return errors.New("transport caller denied")
	}
	if t.IssuedAt < now.Unix()-300 || t.IssuedAt > now.Unix()+30 {
		return errors.New("transport authorization expired or issued in future")
	}
	nonce, err := hex.DecodeString(t.Nonce)
	if err != nil || len(nonce) != 24 {
		return errors.New("invalid transport nonce")
	}
	key, err := p.verificationKey(t.KeyID, t.Caller)
	if err != nil {
		return err
	}
	return verify(key, transportBytes(t), t.Signature)
}

// VerifyDelivery verifies both authority layers. Replay consumption and a
// transactionally deduplicated effect are mandatory receiver responsibilities.
func (p Policy) VerifyDelivery(t Transport, receipt Receipt, target, operation, method, path string, payload []byte, now time.Time) error {
	if err := p.VerifyReceipt(receipt, target, operation, payload); err != nil {
		return err
	}
	return p.VerifyTransport(t, receipt, method, path, now)
}
