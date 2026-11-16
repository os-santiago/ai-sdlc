// Package buildflow composes a signed merged-source request, durable execution
// fences and the closed Kubernetes transport. It exposes no operator advance API.
package buildflow

import (
	"encoding/json"
	"errors"
	"io"
	"os"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildworkload"
)

var ErrBuildFlow = errors.New("build controller denied")

type Installed struct {
	Version            string                `json:"version"`
	Inner              authority.Policy      `json:"inner"`
	Outer              authority.Policy      `json:"outer"`
	Profile            buildworkload.Profile `json:"profile"`
	Slots              []building.Slot       `json:"slots"`
	IntakeConfigSHA256 string                `json:"intakeConfigSHA256"`
	IssueMaxAgeSeconds int                   `json:"issueMaxAgeSeconds"`
}
type PolicyLoader func() (Installed, error)

func (i Installed) Configuration() building.Configuration {
	return building.Configuration{Inner: i.Inner, Outer: i.Outer, ProfileSHA256: i.Profile.Digest(), Slots: i.Slots, IntakeConfigSHA256: i.IntakeConfigSHA256, IssueMaxAgeSeconds: i.IssueMaxAgeSeconds}
}
func (i Installed) Validate() error {
	if i.Version != "ai-sdlc.build-installation/v1" || i.Profile.Validate() != nil || i.Configuration().Validate() != nil {
		return ErrBuildFlow
	}
	for _, s := range i.Slots {
		if s.ClusterUID != i.Profile.ClusterUID || s.NamespaceUID != i.Profile.NamespaceUID {
			return ErrBuildFlow
		}
	}
	return nil
}
func (i Installed) Digest() string { raw, _ := json.Marshal(i); return authority.Digest(raw) }
func LoadInstalled(path string) (Installed, error) {
	var out Installed
	if path == "" {
		return out, ErrBuildFlow
	}
	file, e := os.Open(path)
	if e != nil {
		return out, ErrBuildFlow
	}
	defer file.Close()
	raw, e := io.ReadAll(io.LimitReader(file, 128*1024+1))
	if e != nil || len(raw) > 128*1024 || authority.DecodeStrict(raw, &out) != nil || out.Validate() != nil {
		return Installed{}, ErrBuildFlow
	}
	var fields map[string]json.RawMessage
	if authority.DecodeStrict(raw, &fields) != nil {
		return Installed{}, ErrBuildFlow
	}
	if _, e = authority.ParsePolicy(fields["inner"]); e != nil {
		return Installed{}, ErrBuildFlow
	}
	if _, e = authority.ParsePolicy(fields["outer"]); e != nil {
		return Installed{}, ErrBuildFlow
	}
	return out, nil
}
