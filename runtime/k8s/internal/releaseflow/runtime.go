package releaseflow

import (
	"context"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasing"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"time"
)

// Runtime compiles durable plans only; it cannot execute Kubernetes or forge
// effects. Installation and a separately authorized deployer remain necessary.
type Runtime struct {
	DB   *store.DB
	Load func() (releasing.Configuration, error)
}

func (r Runtime) PollOnce(ctx context.Context) (string, error) {
	if r.DB == nil || r.Load == nil {
		return "", ErrFlow
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	j := store.NewPromotionJournal(r.DB, r.Load)
	if e := j.ExpirePromotion(ctx); e != nil {
		return "promotion-expiration", e
	}
	id, e := j.NextPromotion(ctx)
	if e != nil {
		return "promotion-authority", e
	}
	if id == "" {
		return "promotion-idle", nil
	}
	claim, e := j.ClaimPromotion(ctx, id)
	if e != nil {
		return "promotion-claim", e
	}
	stop := func(reason string) {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = j.StopPromotion(cleanup, claim, reason)
	}
	cfg, input, e := j.ReadPromotionInput(ctx, claim)
	if e != nil {
		stop("revoked")
		return "promotion-authority", e
	}
	plan, e := releasing.CompilePlan(cfg, input.Command, input.Provenance, input.Build, time.Now())
	if e != nil {
		stop("rejected")
		return "promotion-plan", e
	}
	if e = j.FinishPromotion(ctx, claim, plan); e != nil {
		recovery, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if stored, readErr := j.ReadPromotionRecord(recovery, claim.ID); readErr == nil && stored.Plan == plan {
			return "promotion-recorded", nil
		}
		stop("uncertain")
		return "promotion-completion", ErrFlow
	}
	return "promotion-recorded", nil
}
