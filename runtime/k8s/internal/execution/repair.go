package execution

import (
	"context"
	"encoding/json"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/repair"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

func repairPreparation(ctx context.Context, db *store.DB, p authority.Policy, c Command) (repair.Assignment, store.VerificationCandidate, error) {
	deny := func() (repair.Assignment, store.VerificationCandidate, error) {
		return repair.Assignment{}, store.VerificationCandidate{}, ErrAuthority
	}
	if c.Repair == nil || c.Attempt != 2 {
		return deny()
	}
	raw, _ := json.Marshal(c)
	a, parent, e := db.ReadRepairPreparation(ctx, c.ID(), raw)
	if e != nil || a.Reference != *c.Repair {
		return deny()
	}
	var receipt authority.Receipt
	if authority.DecodeStrict(parent.Receipt, &receipt) != nil {
		return deny()
	}
	original, e := Verify(p, receipt, parent.Payload, time.Now())
	if e != nil || original.Attempt != 1 || original.Repair != nil || original.ID() != c.Repair.ParentCandidateID || original.ExpiresAt < c.ExpiresAt {
		return deny()
	}
	expected := original
	expected.Attempt = 2
	expected.Repair = c.Repair
	expected.ExpiresAt = c.ExpiresAt
	if expected.ID() != c.ID() {
		return deny()
	}
	return a, parent, nil
}

func repairInputs(ctx context.Context, db *store.DB, in PreparationInputs, c Command) error {
	profile, e := executor.ParseProfile(in.Profile, c.ProfileSHA256)
	if e != nil || profile.MaxRepairs != 1 {
		return ErrAuthority
	}
	_, _, e = repairPreparation(ctx, db, in.Policy, c)
	return e
}
