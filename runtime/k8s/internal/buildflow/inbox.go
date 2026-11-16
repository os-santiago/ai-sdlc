package buildflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/delivery"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

// InboxEffect has no Kubernetes side effect. The authenticated inbox and queue
// commit together; replay returns the already committed inbox response.
func InboxEffect(journal *store.BuildJournal) delivery.Effect {
	return func(ctx context.Context, tx *sql.Tx, receipt authority.Receipt, payload []byte) ([]byte, error) {
		if journal == nil || receipt.Operation != "build-merged" || receipt.Target != "build-controller" {
			return nil, ErrBuildFlow
		}
		raw, e := json.Marshal(receipt)
		if e != nil {
			return nil, ErrBuildFlow
		}
		if e = journal.QueueBuildTx(ctx, tx, payload, raw); e != nil {
			return nil, e
		}
		return json.Marshal(map[string]string{"status": "queued", "buildID": receipt.ID})
	}
}
