// Package releaseflow connects authenticated components without exposing
// stage advancement, artifact selection or model-authored execution inputs.
package releaseflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/delivery"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasing"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"time"
)

var ErrFlow = errors.New("releasing handoff denied")

type DispatchLoader func() (releasing.Configuration, authority.Identity, error)

func DispatchVerified(ctx context.Context, db *store.DB, load DispatchLoader) (bool, error) {
	if db == nil || load == nil {
		return false, ErrFlow
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cfg, key, e := load()
	if e != nil || cfg.Validate(time.Now()) != nil || key.Role != "verifier" || key.Validate(cfg.Provenance.Build.Outer) != nil {
		return false, ErrFlow
	}
	id, e := db.NextVerifiedForPromotion(ctx, cfg)
	if e != nil || id == "" {
		return false, e
	}
	r, e := db.ReadProvenanceRecord(ctx, id)
	if e != nil {
		return false, e
	}
	b, e := db.ReadBuiltRecord(ctx, r.Command.BuildID)
	if e != nil {
		return false, e
	}
	c, e := releasing.NewCommand(cfg, r, b, time.Now())
	if e != nil {
		return false, e
	}
	payload, _ := json.Marshal(c)
	cfg, key, e = load()
	if e != nil || c.BindRecords(cfg, r, b, time.Now()) != nil || key.Role != "verifier" || key.Validate(cfg.Provenance.Build.Outer) != nil {
		return false, ErrFlow
	}
	signed, e := key.Seal(cfg.Provenance.Build.Outer, c.ID(), "promote-artifact", payload)
	if e != nil {
		return false, ErrFlow
	}
	e = db.WithTx(ctx, func(tx *sql.Tx) error {
		current := func() (releasing.Configuration, error) { p, _, e := load(); return p, e }
		if e := store.RecordPromotionHandoff(ctx, tx, current, signed, payload); e != nil {
			return e
		}
		// The outbox write uses the same signed policy revision as the handoff.
		p, k, e := load()
		if e != nil || c.BindRecords(p, r, b, time.Now()) != nil || k.KeyID != key.KeyID || k.Role != key.Role || k.Validate(p.Provenance.Build.Outer) != nil {
			return ErrFlow
		}
		if e = delivery.Append(ctx, tx, p.Provenance.Build.Outer, k, c.ID(), "promote-artifact", payload); e != nil {
			return e
		}
		p, _, e = load()
		if e != nil || ctx.Err() != nil || c.BindRecords(p, r, b, time.Now()) != nil {
			return ErrFlow
		}
		return store.CheckPromotionHandoff(ctx, tx, current, signed, payload)
	})
	return e == nil, e
}
