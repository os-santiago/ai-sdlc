package podtransport

import (
	"context"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/reviewresult"
)

// Review is untrusted advisory output from the original, terminated reviewer.
// Its validity grants no workflow authority and proves no model provenance.
type Review struct {
	raw    []byte
	result reviewresult.Result
}

func (r Review) Bytes() []byte               { return append([]byte(nil), r.raw...) }
func (r Review) Result() reviewresult.Result { return r.result }

// ExportReview checks immutable Pod/container identity and completed read-only reviewer
// termination on both sides of a fixed bounded helper invocation. Source/tree
// equality and trusted model-response matching remain controller obligations.
func (s *Session) ExportReview(ctx context.Context) (Review, error) {
	if s == nil || s.reviewProfile == nil {
		return Review{}, ErrTransport
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	before, err := s.inspect(ctx, true)
	if err != nil || before.workerExitCode == nil || *before.workerExitCode != 10 {
		return Review{}, ErrTransport
	}
	raw, code, err := s.exchangeFixed(ctx, helperOperation{
		command:     []string{"/workspace-transfer", "review", "--pod-uid", s.identity.PodUID, "--source-sha", s.sourceSHA},
		outputLimit: reviewresult.MaxBytes, frames: 1024, timeout: 5 * time.Second,
	}, nil)
	if err != nil || code != 0 {
		return Review{}, ErrTransport
	}
	result, err := reviewresult.Parse(raw, s.reviewProfile.Model, s.reviewProfile.MaxSteps)
	if err != nil {
		return Review{}, ErrTransport
	}
	after, err := s.inspect(ctx, true)
	if err != nil || after.workerExitCode == nil || *after.workerExitCode != 10 {
		return Review{}, ErrTransport
	}
	return Review{append([]byte(nil), raw...), result}, nil
}
