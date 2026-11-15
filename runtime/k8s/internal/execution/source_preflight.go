package execution

import (
	"context"
	"errors"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/sourcefetch"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

type revokedSourceAuthority struct{}

func (revokedSourceAuthority) Error() string { return "queued source authority revoked" }
func (revokedSourceAuthority) Unwrap() error { return ControllerError{"source-preflight-authority"} }

// Retire only conclusively invalid authority. Observation failures and source
// outages retain the original queue lifetime; no failed claim is reopened.
func acquireQueuedSource(ctx context.Context, db *store.DB, load PreparationLoader, id string, fetch sourceLoader) (acquiredSource, error) {
	source, e := preflightSource(ctx, db, load, id, fetch)
	var revoked revokedSourceAuthority
	if errors.As(e, &revoked) && ctx.Err() == nil {
		if db.RejectQueuedExecution(ctx, id) != nil {
			return acquiredSource{}, ControllerError{"source-preflight-authority"}
		}
	}
	return source, e
}
func preflightReadinessError(ctx context.Context, err error) error {
	if ctx.Err() != nil || errors.Is(err, store.ErrObservationUnavailable) {
		return ControllerError{"source-preflight-authority"}
	}
	return revokedSourceAuthority{}
}

type acquiredSource struct {
	repository string
	bundle     workspacebundle.Bundle
}

// Read-only acquisition precedes Claim. A quota/transport failure leaves the
// existing request queued, with its original expiry; it does not reopen a claim,
// create Kubernetes resources or reserve model work. Claim and preparation both
// independently revalidate current authority after this read.
func preflightSource(ctx context.Context, db *store.DB, load PreparationLoader, id string, fetch sourceLoader) (acquiredSource, error) {
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	if db == nil || load == nil || fetch == nil {
		return acquiredSource{}, ControllerError{"configuration"}
	}
	r, e := db.ReadExecution(ctx, id)
	if e != nil {
		return acquiredSource{}, ControllerError{"source-preflight-authority"}
	}
	input, e := load(ctx)
	if e != nil {
		return acquiredSource{}, ControllerError{"source-preflight-authority"}
	}
	input = copyInputs(input)
	command, e := verifyRecord(input.Policy, r)
	if e != nil || checkInputs(input, command) != nil {
		return acquiredSource{}, preflightReadinessError(ctx, ErrAuthority)
	}
	if e = readiness(ctx, db, input.Policy, input.Intake, command); e != nil {
		return acquiredSource{}, preflightReadinessError(ctx, e)
	}
	bundle, e := fetch(ctx, command.Repository, command.SourceSHA)
	if e != nil {
		stage := "source-unavailable"
		if sourcefetch.RateLimited(e) {
			stage = "source-rate-limited"
		}
		return acquiredSource{}, ControllerError{stage}
	}
	if ctx.Err() != nil || bundle.SourceSHA != command.SourceSHA || bundle.Validate() != nil {
		return acquiredSource{}, ControllerError{"source-unavailable"}
	}
	latest, e := load(ctx)
	if e != nil || ctx.Err() != nil {
		return acquiredSource{}, ControllerError{"source-preflight-authority"}
	}
	if latest.Policy.Revision() != input.Policy.Revision() || latest.Intake.Digest() != input.Intake.Digest() || workspacebundle.Digest(latest.Profile) != workspacebundle.Digest(input.Profile) || workspacebundle.Digest(latest.ADev) != workspacebundle.Digest(input.ADev) {
		return acquiredSource{}, revokedSourceAuthority{}
	}
	if _, e = verifyRecord(latest.Policy, r); e != nil {
		return acquiredSource{}, preflightReadinessError(ctx, e)
	}
	if e = readiness(ctx, db, latest.Policy, latest.Intake, command); e != nil {
		return acquiredSource{}, preflightReadinessError(ctx, e)
	}
	return acquiredSource{command.Repository, bundle}, nil
}
