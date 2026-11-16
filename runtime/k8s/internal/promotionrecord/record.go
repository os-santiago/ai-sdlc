// Package promotionrecord describes historical decisions, never effect authority.
package promotionrecord

import (
	"encoding/json"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildrecord"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/provenancerecord"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasing"
	"time"
)

// Field order preserves the historical store.PromotionRecord digest.
type Record struct {
	Command       releasing.Command
	Configuration releasing.Configuration
	Plan          releasing.Plan
	RecordedAt    time.Time
}

func (r Record) Digest() string { b, _ := json.Marshal(r); return authority.Digest(b) }
func (r Record) Validate(p provenancerecord.Record, b buildrecord.Record) error {
	if r.RecordedAt.Before(p.RecordedAt) || r.RecordedAt.Before(time.UnixMilli(r.Plan.CompiledAt)) || r.Command.BindRecords(r.Configuration, p, b, r.RecordedAt) != nil || r.Plan.Validate(r.Configuration, r.Command, p, b) != nil {
		return releasing.ErrRelease
	}
	return nil
}
