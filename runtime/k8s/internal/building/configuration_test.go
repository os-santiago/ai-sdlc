package building

import (
	"strings"
	"testing"
)

// Pins the original anonymous-struct v1 encoding, before ProfileBinding existed.
func TestProfileBindingPreservesV1SignedDigest(t *testing.T) {
	const uid = "11111111-1111-1111-1111-111111111111"
	s := Slot{ClusterUID: uid, NamespaceUID: uid, PVCUID: uid, PVUID: uid, NodeUID: uid, NodeBootID: uid, StorageIdentitySHA256: strings.Repeat("a", 64), Namespace: "ai-sdlc-builds", PVC: "build-slot-0", Node: "fixture-node", CapacityBytes: 4 << 30, InodeLimit: 32768}
	cfg := Configuration{ProfileSHA256: strings.Repeat("b", 64), IssueMaxAgeSeconds: 60, Slots: []Slot{s}}
	const expected = "fe77affdff9d477b572a7a4b0b18666929806ad5b9102e8ec8a38fe22fb6515a"
	if cfg.ProfileDigest() != expected || cfg.Binding().Digest() != expected {
		t.Fatal("signed v1 configuration digest changed")
	}
	copied := cfg.Binding()
	cfg.Slots[0].Node = "mutated"
	if copied.Digest() != expected {
		t.Fatal("binding shares mutable installation slots")
	}
}
