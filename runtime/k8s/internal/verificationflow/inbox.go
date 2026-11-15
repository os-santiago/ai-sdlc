// Package verificationflow composes independently verified candidate evidence
// with durable verifier claims. Only a signed component inbox can enqueue work.
package verificationflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/delivery"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/verification"
)

func Effect(load func() (authority.Policy, error)) delivery.Effect {
	return func(ctx context.Context, tx *sql.Tx, receipt authority.Receipt, raw []byte) ([]byte, error) {
		p, err := load()
		if err != nil {
			return nil, verification.ErrVerification
		}
		c, err := verification.Verify(p, receipt, raw, time.Now())
		if err != nil {
			return nil, err
		}
		sealed, _ := json.Marshal(receipt)
		if err = store.InsertVerification(ctx, tx, store.VerificationRequest{ID: c.ID(), CandidateID: c.CandidateID, PayloadDigest: authority.Digest(raw), Payload: raw, Receipt: sealed, ExpiresAt: time.UnixMilli(c.ExpiresAt)}); err != nil {
			return nil, err
		}
		return json.Marshal(map[string]string{"verificationId": c.ID(), "state": "queued"})
	}
}
