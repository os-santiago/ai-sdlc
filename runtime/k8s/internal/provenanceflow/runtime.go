package provenanceflow

import (
	"context"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildrecord"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/provenance"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
	"time"
)

type EvidenceVerifier interface {
	Verify(context.Context, buildrecord.Record) (provenance.Proof, error)
}

// Runtime coordinates the dedicated provenance process. Its production factory
// constructs the closed registry verifier with separately mounted custody.
type Runtime struct {
	DB       *store.DB
	Load     func() (provenance.Configuration, error)
	Verifier func(provenance.Configuration) (EvidenceVerifier, error)
}

func (r Runtime) PollOnce(ctx context.Context) (string, error) {
	if r.DB == nil || r.Load == nil || r.Verifier == nil {
		return "", ErrFlow
	}
	j := store.NewProvenanceJournal(r.DB, r.Load)
	// Retire expired attempts even if current policy/trust cannot be loaded.
	if e := j.ExpireProvenance(ctx); e != nil {
		return "provenance-expiration", e
	}
	id, e := j.NextProvenance(ctx)
	if e != nil {
		return "provenance-authority", e
	}
	if id == "" {
		return "provenance-idle", nil
	}
	claim, e := j.ClaimProvenance(ctx, id)
	if e != nil {
		return "provenance-claim", e
	}
	stopped := func(reason string) {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = j.StopProvenance(cleanup, claim, reason)
	}
	cfg, record, e := j.ReadProvenanceInput(ctx, claim)
	if e != nil {
		stopped("revoked")
		return "provenance-authority", e
	}
	verifier, e := r.Verifier(cfg)
	if e != nil || verifier == nil {
		stopped("rejected")
		return "provenance-configuration", ErrFlow
	}
	// Credential loading/factory creation is another possible blocking boundary.
	// Reobserve authority before exposing history to the network verifier.
	cfg2, again, e := j.ReadProvenanceInput(ctx, claim)
	if e != nil || cfg2.Trust.Digest() != cfg.Trust.Digest() || again.Digest() != record.Digest() {
		stopped("revoked")
		return "provenance-authority", ErrFlow
	}
	attempt, cancel := context.WithDeadline(ctx, claim.Until)
	defer cancel()
	proof, e := verifier.Verify(attempt, record)
	if e != nil {
		stopped("rejected")
		return "provenance-evidence", ErrFlow
	}
	if e = j.FinishProvenance(ctx, claim, proof); e != nil {
		// A lost COMMIT response is resolved as history, never a second network call.
		recovery, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		if stored, readErr := j.ReadProvenanceRecord(recovery, claim.ID); readErr == nil && stored.Proof == proof {
			return "provenance-recorded", nil
		}
		stopped("uncertain")
		return "provenance-completion", ErrFlow
	}
	return "provenance-recorded", nil
}
