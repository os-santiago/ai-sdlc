package podtransport

import (
	"context"
	"encoding/json"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/buildworkload"
	"os"
	"testing"
	"time"
)

// API custody only: this opt-in fixture creates no Pod and mounts no workspace.
func TestRealBuildSlotAPICustody(t *testing.T) {
	path := os.Getenv("AI_SDLC_BUILD_SLOT_FIXTURE")
	if path == "" {
		t.Skip("isolated real API fixture required")
	}
	raw, e := os.ReadFile(path)
	var input struct {
		Config Config
		Slot   building.Slot
	}
	if e != nil || len(raw) > 256*1024 || json.Unmarshal(raw, &input) != nil {
		t.Fatal("private fixture input")
	}
	client, e := NewClient(input.Config)
	if e != nil {
		t.Fatal("private fixture client")
	}
	r := &BuildResources{client: client, slot: input.Slot, compiled: buildworkload.Compiled{ExpiresAt: time.Now().Add(2 * time.Minute)}}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if r.inspectBuildSlot(ctx) != nil {
		t.Fatal("actual original slot API custody rejected")
	}
	accepted, rejected := 1, 0
	for _, field := range []string{"cluster", "namespace", "pvc", "pv", "node", "boot", "physical"} {
		r.slot = input.Slot
		replacement := "ffffffff-ffff-ffff-ffff-ffffffffffff"
		switch field {
		case "cluster":
			r.slot.ClusterUID = replacement
		case "namespace":
			r.slot.NamespaceUID = replacement
		case "pvc":
			r.slot.PVCUID = replacement
		case "pv":
			r.slot.PVUID = replacement
		case "node":
			r.slot.NodeUID = replacement
		case "boot":
			r.slot.NodeBootID = replacement
		case "physical":
			r.slot.StorageIdentitySHA256 = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
		}
		if r.inspectBuildSlot(ctx) == nil {
			t.Fatal("mismatched real API custody accepted", field)
		}
		rejected++
	}
	result, _ := json.Marshal(map[string]int{"accepted": accepted, "rejected": rejected})
	t.Logf("AI_SDLC_SLOT_RESULT %s", result)
	t.Log("original bound PVC/PV, namespace, node and fresh lease accepted; seven changed custody identities rejected; no Pod or mount")
}
