package execution

import (
	"bytes"
	"context"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/candidate"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
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
	// Repairs export a cumulative delta against the original repository tree,
	// although the agent's workspace started from the rejected candidate.
	source, err := workspacebundle.Decode(l.prepared.originalSource)
	if err != nil {
		return record, ErrAuthority
	}
	result, err := workspacebundle.Decode(raw)
	if err != nil {
		return record, ErrAuthority
	}
	assignment, err := executor.ParseAssignment(l.prepared.assignment, workspacebundle.Digest(l.prepared.assignment), time.Now())
	if err != nil {
		return record, ErrAuthority
	}
	if assignment.Repair != nil {
		tree, e := result.TreeDigest()
		if e != nil || tree == assignment.Repair.Reference.ParentTreeSHA256 {
			return record, ErrAuthority
		}
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

// Unchanged reports whether an exported workspace carries no change to keep:
// its tree equals the original source tree or, for a repair, the rejected
// candidate's tree it started from. It reads only the bounded export and the
// prepared inputs; an export it cannot decode is not unchanged, so capture
// refuses it with the same checks as any other.
func (l *Lifecycle) Unchanged(exported []byte) bool {
	if l == nil || len(exported) < 1 || len(exported) > workspacebundle.MaxWireBytes {
		return false
	}
	result, err := workspacebundle.Decode(exported)
	if err != nil {
		return false
	}
	tree, err := result.TreeDigest()
	if err != nil {
		return false
	}
	source, err := workspacebundle.Decode(l.prepared.originalSource)
	if err != nil {
		return false
	}
	if original, e := source.TreeDigest(); e == nil && original == tree {
		return true
	}
	assignment, err := executor.ParseAssignment(l.prepared.assignment, workspacebundle.Digest(l.prepared.assignment), time.Now())
	return err == nil && assignment.Repair != nil && assignment.Repair.Reference.ParentTreeSHA256 == tree
}
