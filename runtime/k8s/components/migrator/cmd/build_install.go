package main

import (
	"encoding/json"
	"errors"
	"io"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildstorage"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildworkload"
)

func buildInstallation(operation string, input io.Reader, output io.Writer) error {
	raw, e := io.ReadAll(io.LimitReader(input, 128*1024+1))
	if e != nil || len(raw) > 128*1024 {
		return errors.New("invalid build installation input")
	}
	switch operation {
	case "build-admission":
		var config struct {
			Admission buildworkload.AdmissionConfiguration `json:"admission"`
			Slots     []building.Slot                      `json:"slots"`
		}
		if authority.DecodeStrict(raw, &config) != nil {
			return errors.New("invalid build admission input")
		}
		policy, binding, e := buildworkload.AdmissionForInstallation(config.Admission, config.Slots)
		if e != nil {
			return e
		}
		mutation, mutationBinding := buildworkload.BoundsMutation()
		return json.NewEncoder(output).Encode(map[string]any{"apiVersion": "v1", "kind": "List", "items": []json.RawMessage{policy, binding, mutation, mutationBinding}})
	case "prepare-build-slot":
		var config struct {
			StorageIdentitySHA256 string `json:"storageIdentitySHA256"`
			NodeBootID            string `json:"nodeBootID"`
		}
		if authority.DecodeStrict(raw, &config) != nil {
			return errors.New("invalid build custody input")
		}
		proof, e := buildstorage.PrepareInstalled(config.StorageIdentitySHA256, config.NodeBootID)
		if e != nil {
			return e
		}
		return json.NewEncoder(output).Encode(proof)
	default:
		return errors.New("unknown build installation operation")
	}
}
