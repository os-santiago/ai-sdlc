package deploymentflow

import (
	"context"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"time"
)

// A restarted controller contains an acknowledged candidate. It never resumes
// advance, re-sends an uncertain request or extends the original deadline.
func (r Runtime) recover(ctx context.Context, j *store.DeploymentJournal, c store.DeploymentClaim) (string, error) {
	stop := func(reason string) {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = j.QuarantineDeployment(cleanup, c, reason)
	}
	original, e := j.ReadDeploymentRecovery(ctx, c)
	if e != nil {
		stop("uncertain")
		return "deployment-recovery-quarantined", ErrFlow
	}
	attempt, cancel := context.WithDeadline(ctx, c.Until)
	defer cancel()
	if attempt.Err() != nil {
		stop("expired")
		return "deployment-recovery-expired", ErrFlow
	}
	transport, e := r.Transport(original.Configuration.Target)
	if e != nil || transport == nil {
		stop("rejected")
		return "deployment-recovery-transport", ErrFlow
	}
	if original.RolledBack {
		return r.finishContainment(attempt, j, c, transport, original.Rollback, stop)
	}
	return r.contain(attempt, j, c, transport, original.Advance, stop)
}
