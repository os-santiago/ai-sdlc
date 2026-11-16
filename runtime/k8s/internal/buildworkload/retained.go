package buildworkload

import (
	"bytes"
	"encoding/json"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

const RetainedVersion = "ai-sdlc.retained-build-context/v1"
const CompilerVersion = "ai-sdlc.saferus-build-compiler/v1"
const MaxRetainedBytes = 131072

// RetainedContext contains public historical compilation facts, not execution
// authority. Requests use base64 JSON encoding to preserve exact original bytes.
type RetainedContext struct {
	Version            string                  `json:"version"`
	Compiler           string                  `json:"compiler"`
	Recipe             string                  `json:"recipe"`
	Profile            Profile                 `json:"profile"`
	Configuration      building.ProfileBinding `json:"configuration"`
	IntakeConfigSHA256 string                  `json:"intakeConfigSHA256"`
	Slot               building.Slot           `json:"slot"`
	Build              []byte                  `json:"build"`
	Publish            []byte                  `json:"publish"`
}

func (r RetainedContext) Encode() ([]byte, error) {
	if len(r.Build) == 0 || len(r.Build) > 32768 || len(r.Publish) == 0 || len(r.Publish) > 32768 {
		return nil, ErrWorkload
	}
	raw, e := json.Marshal(r)
	if e != nil || len(raw) > MaxRetainedBytes {
		return nil, ErrWorkload
	}
	return raw, nil
}
func DecodeRetained(raw []byte) (RetainedContext, error) {
	var r RetainedContext
	if len(raw) == 0 || len(raw) > MaxRetainedBytes || authority.DecodeStrict(raw, &r) != nil {
		return RetainedContext{}, ErrWorkload
	}
	canonical, e := r.Encode()
	// Only canonical stored envelopes are accepted; aliases and omitted fields
	// cannot bypass the versioned schema. Inner request bytes remain untouched.
	if e != nil || !bytes.Equal(raw, canonical) {
		return RetainedContext{}, ErrWorkload
	}
	return r, nil
}

// ValidateHistorical requires an already authenticated historical command/plan.
// It has no current clock, transport, claim or permission-returning operation.
func (r RetainedContext) ValidateHistorical(c building.Command, plan building.Plan) error {
	if r.Version != RetainedVersion || r.Compiler != CompilerVersion || r.Recipe != building.Recipe || c.Recipe != r.Recipe || c.Version != building.Version ||
		r.Profile.Validate() != nil || r.Configuration.Validate() != nil || r.Slot.Validate() != nil ||
		r.Configuration.RecipeProfile != r.Profile.Digest() || r.Configuration.Digest() != c.ProfileSHA256 ||
		r.IntakeConfigSHA256 != c.IntakeConfigSHA256 || !workspacebundle.ValidDigest(r.IntakeConfigSHA256) ||
		r.Slot.ClusterUID != r.Profile.ClusterUID || r.Slot.NamespaceUID != r.Profile.NamespaceUID ||
		!workspacebundle.ValidSourceSHA(c.SourceSHA) || !workspacebundle.ValidSourceSHA(c.TreeSHA) ||
		c.IssuedAt <= 0 || c.ExpiresAt <= c.IssuedAt || c.ExpiresAt-c.IssuedAt > building.MaxAge.Milliseconds() ||
		c.ExpiresAt-c.IssuedAt < (BuildBudget+PublishBudget+ObservationMargin-5*time.Second).Milliseconds() {
		return ErrWorkload
	}
	found := false
	for _, s := range r.Configuration.Slots {
		if s == r.Slot {
			found = true
		}
	}
	if !found {
		return ErrWorkload
	}
	if _, e := r.Encode(); e != nil {
		return e
	}
	expected, e := compileDefinition(c, r.Profile, r.Slot)
	if e != nil || expected.Plan != plan || !bytes.Equal(expected.Build, r.Build) || !bytes.Equal(expected.Publish, r.Publish) {
		return ErrWorkload
	}
	return nil
}
