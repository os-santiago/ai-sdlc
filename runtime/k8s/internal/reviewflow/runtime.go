package reviewflow

import (
	"context"
	"errors"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/podtransport"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/sourcefetch"
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
	// Source reads the pinned source: the zero Reader is anonymous; an
	// installed reader authenticates with the repository-scoped read
	// credential mounted only into the verifier container, never a reviewer pod.
	Source sourcefetch.Reader
}

// VerificationMayProceed keeps independent queues moving only when review has
// completed its work or performed no new work. Cleanup uncertainty and
// post-claim failures stop both; review-passed already confirms cleanup. An
// unavailable gateway or sandbox RuntimeClass stops review before any claim.
func VerificationMayProceed(state string, err error) bool {
	if err != nil {
		var diagnostic ControllerError
		return errors.As(err, &diagnostic) && !diagnostic.CleanupPending && (diagnostic.Stage == "gateway-unavailable" || diagnostic.Stage == "gateway-profile-unfit" || diagnostic.Stage == "sandbox-runtime-class")
	}
	return state == "" || state == "rejected" || state == "review-passed"
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
		if (podUID != "" || configUID != "") && r.Client.CleanupReviewAcknowledged(ctx, podName, configName, podUID, configUID) != nil {
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
	// Cleanup and rejection above always run; claiming the review and creating
	// its pod require the sandbox class, observed now with this poll's client.
	config, e := loadConfiguration(ctx, r.Load)
	if e != nil {
		return "", ControllerError{Stage: "configuration"}
	}
	sandbox := Sandbox{RuntimeClass: config.SandboxRuntimeClass()}
	// An unknown class (a transient API failure) refuses the claim too.
	if sandbox.Admission, e = r.Client.CheckRuntimeClass(ctx, sandbox.RuntimeClass); e != nil {
		state := "sandbox-runtime-class-unavailable"
		if podtransport.RuntimeClassUnknown(e) {
			state = "sandbox-runtime-class-unknown"
		}
		return state, podtransport.WithRuntimeClassReason(ControllerError{Stage: "sandbox-runtime-class"}, e)
	}
	if _, e = run(ctx, r.DB, r.Policy, r.Load, id, r.Client, sandbox, r.Source.Fetch); e != nil {
		return "stopped", e
	}
	return "review-passed", nil
}
