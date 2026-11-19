package repairflow

import (
	"context"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/execution"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/intake"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/repair"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/reviewflow"
)

// InstalledLoader reads only reviewed mounts. The orchestrator receives the
// public review projection, never the review model credential or connection.
func InstalledLoader(identity func() (authority.Policy, authority.Identity, error), executionPath, intakePath, reviewPath string) Loader {
	return func(ctx context.Context) (Inputs, error) {
		var in Inputs
		if ctx.Err() != nil || identity == nil {
			return in, repair.ErrRepair
		}
		var e error
		in.Policy, in.Identity, e = identity()
		if e != nil {
			return Inputs{}, e
		}
		in.Runtime, in.Profile, e = execution.LoadRuntime(executionPath)
		if e != nil {
			return Inputs{}, e
		}
		in.Intake, e = intake.LoadConfig(intakePath, in.Policy.Repository)
		if e != nil {
			return Inputs{}, e
		}
		review, e := reviewflow.LoadPublicInstalled(reviewPath)
		if e != nil {
			return Inputs{}, e
		}
		in.ReviewProfile, in.ReviewConfiguration = authority.Digest(review.Profile), review.Digest()
		return in, nil
	}
}
