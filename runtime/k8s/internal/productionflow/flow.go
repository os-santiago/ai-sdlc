package productionflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/delivery"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/productioncompletion"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"time"
)

var ErrFlow = errors.New("production fact handoff denied")

type Loader func() (authority.Policy, authority.Identity, error)

func DispatchCompleted(ctx context.Context, db *store.DB, load Loader) (bool, error) {
	if db == nil || load == nil {
		return false, ErrFlow
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	p, key, e := load()
	if e != nil || key.Role != "release-manager" || key.Validate(p) != nil {
		return false, ErrFlow
	}
	id, e := db.NextTerminalForProduction(ctx)
	if e != nil || id == "" {
		return false, e
	}
	r, e := db.ReadTerminalForProduction(ctx, id)
	if e != nil {
		return false, e
	}
	c, e := productioncompletion.New(p, store.ProductionFact(r))
	if e != nil {
		return false, e
	}
	raw, _ := json.Marshal(c)
	signed, e := key.Seal(p, c.ID(), "record-production", raw)
	if e != nil {
		return false, ErrFlow
	}
	e = db.WithTx(ctx, func(tx *sql.Tx) error {
		current := func() (authority.Policy, error) { p, _, e := load(); return p, e }
		if store.RecordProductionHandoff(ctx, tx, current, signed, raw) != nil {
			return ErrFlow
		}
		p, k, e := load()
		if e != nil || k.Role != key.Role || k.KeyID != key.KeyID || c.Validate(p, time.Now()) != nil || k.Validate(p) != nil {
			return ErrFlow
		}
		if e = delivery.Append(ctx, tx, p, k, c.ID(), "record-production", raw); e != nil {
			return e
		}
		return store.CheckProductionHandoff(ctx, tx, current, signed, raw)
	})
	return e == nil, e
}
func InboxEffect(load func() (authority.Policy, error)) delivery.Effect {
	return func(ctx context.Context, tx *sql.Tx, signed authority.Receipt, raw []byte) ([]byte, error) {
		if signed.Operation != "record-production" || signed.Target != "orchestrator" || store.RecordProductionTx(ctx, tx, load, signed, raw) != nil {
			return nil, ErrFlow
		}
		return json.Marshal(map[string]string{"status": "completed", "productionID": signed.ID})
	}
}
