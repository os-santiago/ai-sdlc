package podtransport

import (
	"context"
	"errors"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildworkload"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

// ExecuteQueuedBuild consumes only a persisted, authenticated request. There is
// no HTTP endpoint for this method. The component reconciler is its sole caller.
// A restarted or failed claim is never resumed: original resources are retained
// for UID-bound cleanup and independent provenance; physical slots are not reused.
func ExecuteQueuedBuild(ctx context.Context, client *Client, journal *store.BuildJournal, id string, cfg building.Configuration, profile buildworkload.Profile) (result building.Result, err error) {
	if journal == nil || journal.DB == nil || client == nil || cfg.Validate() != nil || profile.Validate() != nil || cfg.ProfileSHA256 != profile.Digest() {
		return result, ErrBuildTransport
	}
	request, e := journal.ReadQueuedBuild(ctx, id)
	if e != nil {
		return result, e
	}
	var receipt authority.Receipt
	if authority.DecodeStrict(request.Receipt, &receipt) != nil {
		return result, ErrBuildTransport
	}
	slot, e := journal.UnusedBuildSlot(ctx)
	if e != nil {
		return result, e
	}
	resources, e := PlanBuild(client, cfg, profile, receipt, request.Payload, slot)
	if e != nil {
		return result, e
	}
	claim, e := journal.ClaimBuild(ctx, id, slot)
	if e != nil {
		return result, e
	}
	ctx, cancel := context.WithDeadline(ctx, claim.Until)
	defer cancel()
	defer func() {
		if err != nil {
			stopCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
			stopErr := journal.StopBuild(stopCtx, claim, "uncertain")
			stop()
			if stopErr != nil {
				err = errors.Join(err, stopErr)
			}
			// A concurrent expiry or a lost stop ACK may already have made the
			// claim terminal. Cleanup rechecks archived state independently.
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 245*time.Second)
			defer cleanupCancel()
			if cleanupErr := CleanupQueuedBuild(cleanupCtx, client, journal, claim); cleanupErr != nil {
				err = errors.Join(err, cleanupErr)
			}
		}
	}()
	fence := func() error { return journal.CheckBuild(ctx, claim) }
	plan := resources.Plan()
	observations := map[string]BuildObservation{}
	for _, phase := range []string{"build", "publish"} {
		if e = fence(); e != nil {
			return result, e
		}
		if e = resources.inspectInstalledProfile(ctx); e != nil {
			return result, e
		}
		if e = resources.inspectBuildSlot(ctx); e != nil {
			return result, e
		}
		if e = fence(); e != nil {
			return result, e
		}
		if phase == "publish" {
			original := observations["build"]
			fresh, e := resources.observePhase(ctx, "build", original.Termination.PipelineRunUID)
			if e != nil || !fresh.Complete {
				return result, ErrBuildTransport
			}
		}
		var effect store.BuildEffect
		if phase == "build" {
			effect, e = journal.BeginBuildEffect(ctx, claim, plan)
		} else {
			effect, e = journal.BeginPublishEffect(ctx, claim)
		}
		if e != nil {
			return result, e
		}
		if effect.Name != resources.runName(phase) || effect.RequestDigest != map[string]string{"build": plan.BuildRequestSHA256, "publish": plan.PublishRequestSHA256}[phase] {
			return result, ErrBuildTransport
		}
		// Reload signed authority and issue freshness after preflight and durable
		// intent, immediately before the only POST. Unknown replies are not retried.
		if e = fence(); e != nil {
			return result, e
		}
		uid, e := resources.postRun(ctx, phase)
		if e != nil {
			return result, e
		}
		if e = journal.RememberBuildCreated(context.WithoutCancel(ctx), effect, uid); e != nil {
			return result, e
		}
		for {
			if e = fence(); e != nil {
				return result, e
			}
			observed, e := resources.observePhase(ctx, phase, uid)
			if e != nil {
				return result, e
			}
			if e = fence(); e != nil {
				return result, e
			}
			if observed.Complete {
				if e = journal.RecordBuildTermination(ctx, claim, observed.Termination); e != nil {
					return result, e
				}
				observations[phase] = observed
				break
			}
			timer := time.NewTimer(time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return result, ctx.Err()
			case <-timer.C:
			}
		}
	}
	b, p := observations["build"], observations["publish"]
	c := request.Command
	result = building.Result{Version: "ai-sdlc.build-result/v1", BuildID: id, PlanSHA256: plan.Digest(), ImageURL: p.Results["IMAGE_URL"], ImageDigest: p.Results["IMAGE_DIGEST"], SourceSHA: c.SourceSHA, TreeSHA: c.TreeSHA, DefinitionSHA256: plan.DefinitionSHA256, PublisherImage: plan.PublisherImage, BuildRunUID: b.Termination.PipelineRunUID, PublishRunUID: p.Termination.PipelineRunUID, BuildObservationSHA256: b.Termination.Digest(), PublishObservationSHA256: p.Termination.Digest(), ReleaseAuthorized: false}
	if result.Validate(c, plan, b.Termination, p.Termination) != nil {
		return building.Result{}, ErrBuildTransport
	}
	if e = journal.FinishBuild(ctx, claim, result); e != nil {
		return building.Result{}, e
	}
	return result, nil
}
