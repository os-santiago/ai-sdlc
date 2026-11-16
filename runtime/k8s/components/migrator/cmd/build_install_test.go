package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildworkload"
)

func TestBuildInstallationRejectsUnboundedOrAmbiguousInput(t *testing.T) {
	for _, operation := range []string{"build-admission", "prepare-build-slot", "repair-build-slot"} {
		for _, input := range []string{"", "{} {}", `{"unknown":true}`, strings.Repeat(" ", 128*1024+1)} {
			var out bytes.Buffer
			if buildInstallation(operation, strings.NewReader(input), &out) == nil || out.Len() != 0 {
				t.Fatal("invalid installation input produced output", operation)
			}
		}
	}
}

func TestBuildAdmissionCLICompilesOnlyFiniteDefinitions(t *testing.T) {
	id := "11111111-1111-1111-1111-111111111111"
	slot := building.Slot{ClusterUID: id, NamespaceUID: id, PVCUID: id, PVUID: id, NodeUID: id, NodeBootID: id, StorageIdentitySHA256: strings.Repeat("a", 64), Namespace: "ai-sdlc-builds", PVC: "slot-1", Node: "node-1", CapacityBytes: 4 << 30, InodeLimit: 32768}
	config := buildworkload.AdmissionConfiguration{Version: "ai-sdlc.build-admission-input/v1", ClusterUID: id, NamespaceUID: id, PublisherImage: "quay.io/os-santiago/ai-sdlc@sha256:" + strings.Repeat("a", 64), DeniedTargets: []buildworkload.DeniedTarget{{Host: "10.0.0.1", Port: 443}, {Host: "10.0.0.2", Port: 443}}}
	raw, _ := json.Marshal(map[string]any{"admission": config, "slots": []building.Slot{slot}})
	var out bytes.Buffer
	if e := buildInstallation("build-admission", bytes.NewReader(raw), &out); e != nil {
		t.Fatal(e)
	}
	var list struct {
		APIVersion, Kind string
		Items            []json.RawMessage
	}
	if json.Unmarshal(out.Bytes(), &list) != nil || list.APIVersion != "v1" || list.Kind != "List" || len(list.Items) != 4 {
		t.Fatal("invalid installation definitions")
	}
	policy, binding, _ := buildworkload.AdmissionForInstallation(config, []building.Slot{slot})
	mutation, mutationBinding := buildworkload.BoundsMutation()
	for i, want := range [][]byte{policy, binding, mutation, mutationBinding} {
		if !bytes.Equal(list.Items[i], want) {
			t.Fatal("definition changed", i)
		}
	}
}
