package terminalflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/delivery"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

func InboxEffect(j *store.TerminalJournal) delivery.Effect {
	return func(ctx context.Context, tx *sql.Tx, signed authority.Receipt, raw []byte) ([]byte, error) {
		if j == nil || signed.Operation != "record-deployment" || signed.Target != "release-manager" {
			return nil, ErrTerminal
		}
		receipt, e := json.Marshal(signed)
		if e != nil || j.QueueTerminalTx(ctx, tx, raw, receipt) != nil {
			return nil, ErrTerminal
		}
		return json.Marshal(map[string]string{"status": "queued", "completionID": signed.ID})
	}
}
