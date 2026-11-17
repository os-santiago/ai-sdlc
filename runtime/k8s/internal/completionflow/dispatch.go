// Package completionflow connects authenticated components without exposing
// stage advancement, artifact selection or model-authored execution inputs.
package completionflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/delivery"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasecompletion"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasing"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"time"
)

var ErrFlow = errors.New("completion handoff denied")

type DispatchLoader func() (releasing.Configuration, authority.Identity, error)

func DispatchDeployed(ctx context.Context, db *store.DB, load DispatchLoader) (bool, error) {
	if db == nil || load == nil {
		return false, ErrFlow
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cfg, key, e := load()
	if e != nil || cfg.Validate(time.Now()) != nil || key.Role != "deployer" || key.Validate(cfg.Provenance.Build.Outer) != nil {
		return false, ErrFlow
	}
	id, e := db.NextDeployedForCompletion(ctx, cfg)
	if e != nil || id == "" {
		return false, e
	}
	r, e := db.ReadDeployedForCompletion(ctx, id)
	if e != nil {
		return false, e
	}
	c, e := releasecompletion.NewCommand(cfg, r.Deployment, r.Upstream.Promotion, r.Upstream.Provenance, r.Upstream.Build, time.Now())
	if e != nil {
		return false, e
	}
	payload, _ := json.Marshal(c)
	cfg, key, e = load()
	if e != nil || c.BindRecords(cfg, r.Deployment, r.Upstream.Promotion, r.Upstream.Provenance, r.Upstream.Build, time.Now()) != nil || key.Role != "deployer" || key.Validate(cfg.Provenance.Build.Outer) != nil {
		return false, ErrFlow
	}
	signed, e := key.Seal(cfg.Provenance.Build.Outer, c.ID(), "record-deployment", payload)
	if e != nil {
		return false, ErrFlow
	}
	e = db.WithTx(ctx, func(tx *sql.Tx) error {
		current := func() (releasing.Configuration, error) { p, _, e := load(); return p, e }
		if e := store.RecordCompletionHandoff(ctx, tx, current, signed, payload); e != nil {
			return e
		}
		// The outbox write uses the same signed policy revision as the handoff.
		p, k, e := load()
		if e != nil || c.BindRecords(p, r.Deployment, r.Upstream.Promotion, r.Upstream.Provenance, r.Upstream.Build, time.Now()) != nil || k.KeyID != key.KeyID || k.Role != key.Role || k.Validate(p.Provenance.Build.Outer) != nil {
			return ErrFlow
		}
		if e = delivery.Append(ctx, tx, p.Provenance.Build.Outer, k, c.ID(), "record-deployment", payload); e != nil {
			return e
		}
		p, _, e = load()
		if e != nil || ctx.Err() != nil || c.BindRecords(p, r.Deployment, r.Upstream.Promotion, r.Upstream.Provenance, r.Upstream.Build, time.Now()) != nil {
			return ErrFlow
		}
		return store.CheckCompletionHandoff(ctx, tx, current, signed, payload)
	})
	return e == nil, e
}
