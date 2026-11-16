package provenanceflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/delivery"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

func InboxEffect(j *store.ProvenanceJournal) delivery.Effect {
	return func(ctx context.Context, tx *sql.Tx, r authority.Receipt, payload []byte) ([]byte, error) {
		if j == nil || r.Operation != "verify-built" || r.Target != "verifier" {
			return nil, ErrFlow
		}
		raw, e := json.Marshal(r)
		if e != nil {
			return nil, ErrFlow
		}
		if e = j.QueueProvenanceTx(ctx, tx, payload, raw); e != nil {
			return nil, e
		}
		return json.Marshal(map[string]string{"status": "queued", "verificationID": r.ID})
	}
}
