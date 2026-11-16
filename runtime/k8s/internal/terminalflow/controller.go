// Package terminalflow owns the finite release/issue terminal effects. It never
// adopts matching external objects or renews an expired completion command.
package terminalflow

import (
	"context"
	"errors"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasecompletion"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/terminalwriter"
	"time"
)

var ErrTerminal = errors.New("terminal release denied or uncertain")

type Transport interface {
	Execute(context.Context, releasecompletion.Plan, string, int64, terminalwriter.Fence) (releasecompletion.EffectReceipt, error)
}
type Controller struct {
	Journal *store.TerminalJournal
	Writer  Transport
}

func (c *Controller) RunOne(ctx context.Context) (bool, error) {
	if c == nil || c.Journal == nil || c.Writer == nil {
		return false, ErrTerminal
	}
	worked := false
	_, e := c.Journal.WithTerminalController(ctx, func(ctx context.Context) error {
		if e := c.Journal.ExpireTerminals(ctx); e != nil {
			return e
		}
		claim, e := c.Journal.NextTerminalRecovery(ctx)
		if e != nil {
			return e
		}
		if claim.ID == "" {
			id, e := c.Journal.NextTerminal(ctx)
			if e != nil || id == "" {
				return e
			}
			claim, e = c.Journal.ClaimTerminal(ctx, id)
			if e != nil {
				return e
			}
		}
		worked = true
		return c.run(ctx, claim)
	})
	return worked, e
}
func (c *Controller) run(ctx context.Context, claim store.TerminalClaim) error {
	r, e := c.Journal.ReadTerminal(ctx, claim)
	if e != nil {
		_ = c.Journal.QuarantineTerminal(ctx, claim, "rejected")
		return ErrTerminal
	}
	progress, e := c.Journal.ReadTerminalProgress(ctx, claim)
	if e != nil {
		_ = c.Journal.QuarantineTerminal(ctx, claim, "rejected")
		return ErrTerminal
	}
	// Only authenticated original ACKs permit continuation. A missing send is safe
	// to start under fresh authority; a send with no ACK is irrevocably uncertain.
	if progress.Uncertain {
		_ = c.Journal.QuarantineTerminal(ctx, claim, "uncertain")
		return ErrTerminal
	}
	if progress.NextStep == "" {
		return c.Journal.FinishTerminal(ctx, claim)
	}
	if !claim.Until.After(time.Now()) {
		_ = c.Journal.QuarantineTerminal(ctx, claim, "expired")
		return ErrTerminal
	}
	active, cancel := context.WithDeadline(ctx, claim.Until)
	defer cancel()
	for progress.NextStep != "" {
		step := progress.NextStep
		if _, e = c.Journal.BeginTerminalEffect(active, claim, step); e != nil {
			_ = c.Journal.QuarantineTerminal(ctx, claim, "revoked")
			return ErrTerminal
		}
		receipt, e := c.Writer.Execute(active, r.Plan, step, progress.ReleaseID, func(ctx context.Context, q releasecompletion.EffectRequest) error {
			return c.Journal.SendTerminalEffect(ctx, claim, q)
		})
		if e != nil {
			_ = c.Journal.QuarantineTerminal(ctx, claim, "uncertain")
			return ErrTerminal
		}
		if e = c.Journal.RecordTerminalReceipt(active, claim, receipt); e != nil {
			// A SQL COMMIT response can be lost after its fact became durable. Resolve
			// only that exact original fact. Never repeat the external mutation.
			observed, readErr := c.Journal.ReadTerminalProgress(ctx, claim)
			if readErr != nil || len(observed.Receipts) == 0 || observed.Receipts[len(observed.Receipts)-1] != receipt {
				_ = c.Journal.QuarantineTerminal(ctx, claim, "uncertain")
				return ErrTerminal
			}
		}
		progress, e = c.Journal.ReadTerminalProgress(ctx, claim)
		if e != nil || progress.Uncertain {
			return ErrTerminal
		}
	}
	return c.Journal.FinishTerminal(ctx, claim)
}
