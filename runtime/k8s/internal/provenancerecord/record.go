// Package provenancerecord describes historical verification evidence. Values
// and structural validation never establish journal authenticity or authority.
package provenancerecord

import (
	"encoding/json"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildrecord"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/provenance"
	"time"
)

// Field order and names preserve the original store.ProvenanceRecord digest.
type Record struct {
	Command       provenance.Command
	Configuration provenance.Configuration
	Proof         provenance.Proof
	RecordedAt    time.Time
}

func (r Record) Digest() string { raw, _ := json.Marshal(r); return authority.Digest(raw) }
func (r Record) Validate(b buildrecord.Record) error {
	if r.RecordedAt.Before(b.RecordedAt) || r.RecordedAt.Before(time.UnixMilli(r.Command.IssuedAt)) || r.Command.BindProof(r.Configuration, b, r.Proof, r.RecordedAt) != nil {
		return provenance.ErrProvenance
	}
	return nil
}
