package main

import (
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildworkload"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/podtransport"
)

// Input is private stdin; output contains public custody/configuration only.
func observeBuildInstallation(operation string, input io.Reader, output io.Writer) error {
	raw, e := io.ReadAll(io.LimitReader(input, 256*1024+1))
	if e != nil || len(raw) > 256*1024 {
		return podtransport.ErrBuildTransport
	}
	var request struct {
		Client         podtransport.Config                  `json:"client"`
		Slots          []building.Slot                      `json:"slots"`
		PublisherImage string                               `json:"publisherImage,omitempty"`
		Admission      buildworkload.AdmissionConfiguration `json:"admission,omitempty"`
		ApprovedTrust  podtransport.BuildTrust              `json:"approvedTrust,omitempty"`
	}
	if authority.DecodeStrict(raw, &request) != nil {
		return podtransport.ErrBuildTransport
	}
	client, e := podtransport.NewClient(request.Client)
	if e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var result any
	switch operation {
	case "observe-build-admission":
		result, e = podtransport.ObserveBuildAdmission(ctx, client, request.Slots, request.PublisherImage)
	case "observe-build-trust":
		result, e = podtransport.ObserveBuildTrust(ctx, client, request.Slots)
	case "observe-build-profile":
		result, e = podtransport.ObserveBuildProfile(ctx, client, request.Admission, request.Slots, request.ApprovedTrust)
	default:
		return podtransport.ErrBuildTransport
	}
	if e != nil {
		return e
	}
	return json.NewEncoder(output).Encode(result)
}
