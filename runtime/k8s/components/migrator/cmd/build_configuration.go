package main

import (
	"encoding/json"
	"io"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildflow"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildworkload"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/intake"
)

// Compile public installation configuration without generating signing keys,
// executing software, opening a database or changing a workflow stage.
func compileBuildInstallation(input io.Reader, output io.Writer) error {
	raw, e := io.ReadAll(io.LimitReader(input, 256*1024+1))
	if e != nil || len(raw) > 256*1024 {
		return buildflow.ErrBuildFlow
	}
	var request struct {
		Inner              json.RawMessage       `json:"inner"`
		Outer              json.RawMessage       `json:"outer"`
		Profile            buildworkload.Profile `json:"profile"`
		Slots              []building.Slot       `json:"slots"`
		Intake             intake.Config         `json:"intake"`
		IssueMaxAgeSeconds int                   `json:"issueMaxAgeSeconds"`
	}
	if authority.DecodeStrict(raw, &request) != nil {
		return buildflow.ErrBuildFlow
	}
	inner, e := authority.ParsePolicy(request.Inner)
	if e != nil {
		return e
	}
	outer, e := authority.ParsePolicy(request.Outer)
	if e != nil {
		return e
	}
	if request.Intake.Validate(inner.Repository) != nil {
		return buildflow.ErrBuildFlow
	}
	installed := buildflow.Installed{Version: "ai-sdlc.build-installation/v1", Inner: inner, Outer: outer, Profile: request.Profile, Slots: request.Slots, IntakeConfigSHA256: request.Intake.Digest(), IssueMaxAgeSeconds: request.IssueMaxAgeSeconds}
	if installed.Validate() != nil {
		return buildflow.ErrBuildFlow
	}
	return json.NewEncoder(output).Encode(installed)
}
