// Package provenanceflow connects authenticated components without exposing
// stage advancement, artifact selection or model-authored execution inputs.
package provenanceflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/delivery"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/provenance"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"time"
)

var ErrFlow = errors.New("provenance handoff denied")

type DispatchLoader func() (provenance.Configuration, authority.Identity, error)

func DispatchBuilt(ctx context.Context, db *store.DB, load DispatchLoader) (bool, error) {
	if db == nil || load == nil {
		return false, ErrFlow
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cfg, key, e := load()
	if e != nil || cfg.Validate(time.Now()) != nil || key.Role != "build-controller" || key.Validate(cfg.Build.Outer) != nil {
		return false, ErrFlow
	}
	id, e := db.NextBuiltForProvenance(ctx, cfg)
	if e != nil || id == "" {
		return false, e
	}
	r, e := db.ReadCleanBuiltRecord(ctx, id)
	if e != nil {
		return false, e
	}
	c, e := provenance.NewCommand(cfg, r, time.Now())
	if e != nil {
		return false, e
	}
	payload, _ := json.Marshal(c)
	cfg, key, e = load()
	if e != nil || c.BindRecord(cfg, r, time.Now()) != nil || key.Role != "build-controller" || key.Validate(cfg.Build.Outer) != nil {
		return false, ErrFlow
	}
	signed, e := key.Seal(cfg.Build.Outer, c.ID(), "verify-built", payload)
	if e != nil {
		return false, ErrFlow
	}
	e = db.WithTx(ctx, func(tx *sql.Tx) error {
		current := func() (provenance.Configuration, error) { p, _, e := load(); return p, e }
		if e := store.RecordProvenanceHandoff(ctx, tx, current, signed, payload); e != nil {
			return e
		}
		// The outbox write uses the same signed policy revision as the handoff.
		p, k, e := load()
		if e != nil || c.BindRecord(p, r, time.Now()) != nil || k.KeyID != key.KeyID || k.Role != key.Role || k.Validate(p.Build.Outer) != nil {
			return ErrFlow
		}
		if e = delivery.Append(ctx, tx, p.Build.Outer, k, c.ID(), "verify-built", payload); e != nil {
			return e
		}
		p, _, e = load()
		if e != nil || ctx.Err() != nil || c.BindRecord(p, r, time.Now()) != nil {
			return ErrFlow
		}
		return store.CheckProvenanceHandoff(ctx, tx, current, signed, payload)
	})
	return e == nil, e
}
