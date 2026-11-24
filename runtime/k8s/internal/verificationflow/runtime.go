package verificationflow

import (
	"context"
	"errors"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/podtransport"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/verification"
)

// Runtime has no externally callable claim or advance operation. Its queue is
// populated only by the authenticated component inbox. Cleanup remains possible
// after authority revocation, but never adopts resources from names alone.
type Runtime struct {
	DB     *store.DB
	Load   PolicyLoader
	Client *podtransport.Client
	Image  string
	// MailboxImage carries the repository-tests runner and transfer helper.
	MailboxImage string
	// RuntimeClass is the installed sandbox RuntimeClass for verification pods.
	RuntimeClass string
}

func (r Runtime) PollOnce(ctx context.Context) (string, error) {
	if r.DB == nil || r.Load == nil || r.Client == nil {
		return "", ControllerError{"configuration"}
	}
	// Repository-tests verifications renew their claim while pods run; the
	// bound covers the longest command lifetime plus cleanup.
	ctx, cancel := context.WithTimeout(ctx, verification.MaxCommandAge+5*time.Minute)
	defer cancel()
	if r.DB.ExpireVerifications(ctx) != nil {
		return "", ControllerError{"expiry"}
	}
	cleanup, err := r.DB.NextVerificationCleanup(ctx)
	if err != nil {
		return "", ControllerError{"cleanup"}
	}
	if cleanup.Claim.ID != "" {
		effects, e := r.DB.ReadVerificationEffects(ctx, cleanup.Claim)
		removed := e == nil
		for _, kind := range []string{"baseline", "candidate", "reference", "tests", "config"} {
			for _, effect := range effects {
				if effect.Kind == kind {
					if effect.NotSent {
						continue
					}
					if effect.UID == "" {
						removed = false
						continue
					}
					if r.Client.CleanupVerification(ctx, effect.Kind, effect.Name, effect.UID) != nil {
						removed = false
					}
				}
			}
		}
		if r.DB.FinishVerificationCleanup(ctx, cleanup, removed) != nil || !removed {
			return "cleanup-pending", ControllerError{"cleanup"}
		}
	}
	blocked, err := r.DB.VerificationCleanupBlocked(ctx)
	if err != nil || blocked {
		return "cleanup-uncertain", ControllerError{"cleanup"}
	}
	id, err := r.DB.NextVerification(ctx)
	if err != nil {
		return "", ControllerError{"queue"}
	}
	if id == "" {
		return "", nil
	}
	if _, _, _, err = CurrentProof(ctx, r.DB, r.Load, id); err != nil {
		if errors.Is(err, store.ErrObservationUnavailable) {
			return "observation-unavailable", ControllerError{"authority"}
		}
		if r.DB.RejectQueuedVerification(ctx, id) != nil {
			return "", ControllerError{"rejection"}
		}
		return "rejected", nil
	}
	if _, err = RunWithMailbox(ctx, r.DB, r.Load, id, r.Client, r.Image, r.MailboxImage, r.RuntimeClass); err != nil {
		return "stopped", err
	}
	return "behavior-verified", nil
}
