package buildflow

import (
	"context"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/podtransport"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

type Runtime struct {
	DB     *store.DB
	Load   PolicyLoader
	Client func() (*podtransport.Client, error)
	// AfterRecovery may dispatch authenticated completed history only after all
	// archived containment is retired, under this controller's exclusive lease.
	AfterRecovery func(context.Context) (bool, error)
}

// PollOnce has no issue/source/stage argument. Recovery always precedes new
// work, including under freeze or missing current policy. Unknown creation or
// custody blocks this controller; it never frees or reuses the physical slot.
func (r Runtime) PollOnce(ctx context.Context) (string, error) {
	if r.DB == nil || r.Load == nil || r.Client == nil {
		return "", ErrBuildFlow
	}
	state := "build-idle"
	acquired, e := r.DB.WithBuildController(ctx, func(ctx context.Context) error {
		journal := store.NewBuildJournal(r.DB, func() (building.Configuration, error) {
			i, e := r.Load()
			return i.Configuration(), e
		})
		client, e := r.Client()
		if e != nil {
			return e
		}
		claim, e := journal.NextBuildRecovery(ctx)
		if e != nil {
			return e
		}
		if claim.ID != "" {
			state = "build-containment"
			return podtransport.CleanupQueuedBuild(ctx, client, journal, claim)
		}
		i, e := r.Load()
		if e != nil || i.Validate() != nil {
			return ErrBuildFlow
		}
		if i.Inner.Freeze || i.Outer.Freeze {
			state = "build-frozen"
			return nil
		}
		if r.AfterRecovery != nil {
			sent, err := r.AfterRecovery(ctx)
			if err != nil {
				return err
			}
			if sent {
				state = "build-verification-handoff"
				return nil
			}
		}
		id, e := journal.NextBuild(ctx)
		if e != nil || id == "" {
			return e
		}
		state = "build-execution"
		_, e = podtransport.ExecuteQueuedBuild(ctx, client, journal, id, i.Configuration(), i.Profile)
		return e
	})
	if !acquired && e == nil {
		return "build-other-replica", nil
	}
	return state, e
}
