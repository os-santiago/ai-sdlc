package reviewflow

import (
	"context"
	"errors"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/podtransport"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/verificationflow"
	"time"
)

// Runtime consumes only passed behavior proofs or its already queued reviews.
// Cleanup can continue after policy revocation using original acknowledged UIDs.
type Runtime struct {
	DB     *store.DB
	Policy verificationflow.PolicyLoader
	Load   Loader
	Client *podtransport.Client
}

func (r Runtime) PollOnce(ctx context.Context) (string, error) {
	if r.DB == nil || r.Policy == nil || r.Load == nil || r.Client == nil {
		return "", ControllerError{Stage: "configuration"}
	}
	ctx, cancel := context.WithTimeout(ctx, 6*time.Minute)
	defer cancel()
	if r.DB.ExpireReviews(ctx) != nil {
		return "", ControllerError{Stage: "expiry"}
	}
	cleanup, e := r.DB.NextReviewCleanup(ctx)
	if e != nil {
		return "", ControllerError{Stage: "cleanup"}
	}
	if cleanup.Claim.ID != "" {
		effects, e := r.DB.ReadReviewEffects(ctx, cleanup.Claim)
		removed := e == nil
		podName, configName, podUID, configUID := "", "", "", ""
		for _, effect := range effects {
			if effect.Kind != "config" && effect.Kind != "pod" {
				continue
			}
			if effect.NotSent {
				continue
			}
			if effect.UID == "" {
				removed = false
				continue
			}
			if effect.Kind == "config" {
				configName, configUID = effect.Name, effect.UID
			} else {
				podName, podUID = effect.Name, effect.UID
			}
		}
		if (podUID != "" || configUID != "") && r.Client.CleanupAcknowledged(ctx, "ai-sdlc-reviewers", podName, configName, podUID, configUID) != nil {
			removed = false
		}
		if r.DB.FinishReviewCleanup(ctx, cleanup, removed) != nil || !removed {
			return "cleanup-pending", ControllerError{Stage: "cleanup", CleanupPending: true}
		}
	}
	blocked, e := r.DB.ReviewCleanupBlocked(ctx)
	if e != nil || blocked {
		return "cleanup-uncertain", ControllerError{Stage: "cleanup", CleanupPending: true}
	}
	reviewID, e := r.DB.NextReview(ctx)
	if e != nil {
		return "", ControllerError{Stage: "queue"}
	}
	id := ""
	if reviewID != "" {
		run, e := r.DB.ReadReview(ctx, reviewID)
		if e != nil {
			return "", ControllerError{Stage: "observation"}
		}
		config, e := loadConfiguration(ctx, r.Load)
		if errors.Is(e, store.ErrObservationUnavailable) {
			return "observation-unavailable", ControllerError{Stage: "observation"}
		}
		if e != nil || config.Digest() != run.ConfigurationDigest {
			if r.DB.RejectQueuedReview(ctx, reviewID) != nil {
				return "", ControllerError{Stage: "rejection"}
			}
			return "rejected", nil
		}
		id = run.VerificationID
	} else {
		id, e = r.DB.NextVerifiedForReview(ctx)
		if e != nil {
			return "", ControllerError{Stage: "queue"}
		}
	}
	if id == "" {
		return "", nil
	}
	if _, _, _, e = verificationflow.CurrentProof(ctx, r.DB, r.Policy, id); e != nil {
		if errors.Is(e, store.ErrObservationUnavailable) {
			return "observation-unavailable", ControllerError{Stage: "observation"}
		}
		if reviewID != "" && r.DB.RejectQueuedReview(ctx, reviewID) != nil {
			return "", ControllerError{Stage: "rejection"}
		}
		return "rejected", nil
	}
	if _, e = Run(ctx, r.DB, r.Policy, r.Load, id, r.Client); e != nil {
		return "stopped", e
	}
	return "review-passed", nil
}
