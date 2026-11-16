package store

import (
	"encoding/json"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/mergegate"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/merging"
)

// MergeJournal keeps policy verification inside every authority-bearing journal
// boundary. The loader reads reviewed installation configuration, never an issue
// or caller-provided policy. No installed controller constructs this yet.
type MergeJournal struct {
	*DB
	load func() (authority.Policy, error)
}

func NewMergeJournal(db *DB, load func() (authority.Policy, error)) *MergeJournal {
	return &MergeJournal{db, load}
}
func (j *MergeJournal) currentAuthority(payload, receipt, gate []byte, now time.Time) (authority.Policy, error) {
	if j == nil || j.DB == nil || j.load == nil {
		return authority.Policy{}, ErrExecution
	}
	p, e := j.load()
	if e != nil || verifyMergeAuthority(p, payload, receipt, gate, now) != nil {
		return authority.Policy{}, ErrExecution
	}
	return p, nil
}
func verifyMergeAuthority(p authority.Policy, payload, receipt, gate []byte, now time.Time) error {
	if len(receipt) == 0 || len(receipt) > 4096 || len(gate) == 0 || len(gate) > 16384 {
		return ErrExecution
	}
	var signed authority.Receipt
	if authority.DecodeStrict(receipt, &signed) != nil {
		return ErrExecution
	}
	c, e := merging.Verify(p, signed, payload, now)
	if e != nil || authority.Digest(gate) != c.GateSHA256 {
		return ErrExecution
	}
	var observed mergegate.Observation
	if authority.DecodeStrict(gate, &observed) != nil || !observed.Ready || observed.MergeAuthorized || observed.Reasons == nil || len(observed.Reasons) != 0 || observed.Expected != (mergegate.Expected{PullRequest: c.PullRequest, HeadSHA: c.HeadSHA, BaseSHA: c.BaseSHA, TreeSHA: c.TreeSHA, Branch: c.Branch}) {
		return ErrExecution
	}
	for _, d := range []string{observed.PolicySHA256, observed.ChecksSHA256, observed.ReviewsSHA256} {
		if !modelDigest.MatchString(d) {
			return ErrExecution
		}
	}
	issued := time.UnixMilli(c.IssuedAt)
	if observed.ObservedAt.IsZero() || observed.ObservedAt.After(issued.Add(5*time.Second)) || observed.ObservedAt.Before(issued.Add(-30*time.Second)) || observed.ObservedAt.After(now.Add(5*time.Second)) || !now.Before(observed.ObservedAt.Add(30*time.Second)) || time.UnixMilli(c.ExpiresAt).After(observed.ObservedAt.Add(30*time.Second)) {
		return ErrExecution
	}
	// Public policy only: bound storage prevents arbitrary configuration payloads.
	raw, e := json.Marshal(p)
	if e != nil || len(raw) > 32768 {
		return ErrExecution
	}
	return nil
}
