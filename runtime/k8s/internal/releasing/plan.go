package releasing

import (
	"encoding/json"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildrecord"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/provenancerecord"
	"time"
)

const PlanVersion = "ai-sdlc.saferus-release-plan/v1"
const PlanMaxAge = 15 * time.Minute

// Reserve advance and containment (rollout and health for each), plus bounded
// observation/patch/journal overhead. Expiry is never bypassed for rollback.
const MinimumEffectBudget = 14 * time.Minute

// Plan is a deterministic decision, not permission to patch a workload. The
// future deployer must receive separate signed authority, record an effect
// intent and compare live target identity/content before its image-only CAS.
// This first-install plan permits only the independently approved baseline.
type Plan struct {
	Version                string `json:"version"`
	PromotionID            string `json:"promotionID"`
	ProvenanceRecordSHA256 string `json:"provenanceRecordSHA256"`
	TargetSHA256           string `json:"targetSHA256"`
	SourceSHA              string `json:"sourceSHA"`
	Image                  string `json:"image"`
	PreviousImage          string `json:"previousImage"`
	CompiledAt             int64  `json:"compiledAt"`
	ExpiresAt              int64  `json:"expiresAt"`
}

func (p Plan) Digest() string { b, _ := json.Marshal(p); return authority.Digest(b) }
func CompilePlan(cfg Configuration, c Command, r provenancerecord.Record, b buildrecord.Record, now time.Time) (Plan, error) {
	if c.BindRecords(cfg, r, b, now) != nil {
		return Plan{}, ErrRelease
	}
	expires := now.Add(PlanMaxAge).UnixMilli()
	for _, limit := range []int64{cfg.Target.ExpiresAt, cfg.Provenance.Trust.ExpiresAt} {
		if expires > limit {
			expires = limit
		}
	}
	// Retain enough time for fresh deployment admission, rollout and smoke.
	if expires-now.UnixMilli() < MinimumEffectBudget.Milliseconds() {
		return Plan{}, ErrRelease
	}
	image := building.Registry + "@" + c.ImageDigest
	if image == cfg.Target.BaselineImage {
		return Plan{}, ErrRelease
	}
	return Plan{PlanVersion, c.ID(), r.Digest(), cfg.Target.Digest(), c.SourceSHA, image, cfg.Target.BaselineImage, now.UnixMilli(), expires}, nil
}

// Validate authenticates no history. It checks deterministic compilation under
// the recorded configuration and time, even after the promotion command expires.
func (p Plan) Validate(cfg Configuration, c Command, r provenancerecord.Record, b buildrecord.Record) error {
	expected, e := CompilePlan(cfg, c, r, b, time.UnixMilli(p.CompiledAt))
	if e != nil || expected != p {
		return ErrRelease
	}
	return nil
}
