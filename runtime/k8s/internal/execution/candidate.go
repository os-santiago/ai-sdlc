package execution

import (
	"bytes"
	"context"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/candidate"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

// CaptureCandidate preserves one bounded unvalidated delta. It does not accept
// generated code, complete an attempt, or grant publication/release authority.
// Zero changes and zero confirmed model calls are not successful captures.
func (l *Lifecycle) CaptureCandidate(ctx context.Context, exported []byte) (record store.CandidateRecord, err error) {
	if l == nil || !l.mu.TryLock() {
		return record, ErrAuthority
	}
	defer l.mu.Unlock()
	if l.stopped || l.captured || l.model == nil {
		return record, ErrAuthority
	}
	ctx, cancel := context.WithDeadline(ctx, l.durable.Claim.Until)
	defer cancel()
	ctx, bound := context.WithTimeout(ctx, 20*time.Second)
	defer bound()
	defer func() {
		if recover() != nil {
			err = ErrAuthority
		}
		if err != nil {
			record = store.CandidateRecord{}
			l.stopped = true
			stopLifecycleClaim(l.db, l.durable.Claim)
		}
	}()
	if len(exported) < 1 || len(exported) > workspacebundle.MaxWireBytes {
		return record, ErrAuthority
	}
	raw := bytes.Clone(exported)
	_, phase, err := l.proofPhase(ctx)
	if err != nil || phase != "export-ready" {
		return record, ErrAuthority
	}
	source, err := workspacebundle.Decode(l.prepared.Source())
	if err != nil {
		return record, ErrAuthority
	}
	result, err := workspacebundle.Decode(raw)
	if err != nil {
		return record, ErrAuthority
	}
	delta, err := candidate.Build(source, result)
	if err != nil {
		return record, ErrAuthority
	}
	if _, err = candidate.Apply(source, delta); err != nil {
		return record, ErrAuthority
	}
	encoded, err := candidate.Encode(delta)
	if err != nil {
		return record, ErrAuthority
	}
	if _, phase, err = l.proofPhase(ctx); err != nil || phase != "export-ready" {
		return record, ErrAuthority
	}
	record, err = l.db.CaptureLifecycleCandidate(ctx, l.durable, l.model.scope.AllocationID, raw, encoded)
	if err != nil {
		return record, ErrAuthority
	}
	// All candidate/model database reads precede the final current preparation
	// check in proofPhase. No blocking artifact read follows that check.
	if _, phase, err = l.proofPhase(ctx); err != nil || phase != "candidate-captured" {
		return record, ErrAuthority
	}
	l.captured = true
	return record, nil
}
