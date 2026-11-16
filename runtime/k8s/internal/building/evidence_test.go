package building

import (
	"strings"
	"testing"
	"time"
)

func TestPublisherImageRequiresExactHostname(t *testing.T) {
	for _, host := range []string{"quayXio", "quay-io", "quay_io", "evil.quay.io", "quay.io.evil"} {
		if publisherImage.MatchString(host + "/os-santiago/ai-sdlc@sha256:" + strings.Repeat("a", 64)) {
			t.Fatal("foreign registry accepted", host)
		}
	}
	for _, repository := range []string{"quay.io/os-santiago/ai-sdlc", "localhost/ai-sdlc-artifact-builder"} {
		if !publisherImage.MatchString(repository + "@sha256:" + strings.Repeat("a", 64)) {
			t.Fatal("reviewed publisher rejected")
		}
	}
}

func TestContainerStartUsesKubernetesSecondPrecision(t *testing.T) {
	_, _, _, c, _ := fixture(t)
	now := time.Unix(1700000000, 100000000)
	c.IssuedAt = now.UnixMilli()
	c.ExpiresAt = now.Add(20 * time.Minute).UnixMilli()
	uid := "11111111-1111-1111-1111-111111111111"
	s := Slot{ClusterUID: uid, NamespaceUID: uid, PVCUID: uid, PVUID: uid, NodeUID: uid, NodeBootID: uid, StorageIdentitySHA256: strings.Repeat("a", 64), Namespace: "ai-sdlc-builds", PVC: "slot", Node: "node", CapacityBytes: 4 << 30, InodeLimit: 32768}
	p := Plan{Version: "ai-sdlc.build-plan/v1", BuildID: c.ID(), SourceSHA: c.SourceSHA, TreeSHA: c.TreeSHA, ProfileSHA256: c.ProfileSHA256, SlotDigest: s.Digest(), DefinitionSHA256: strings.Repeat("a", 64), BuildRequestSHA256: strings.Repeat("b", 64), PublishRequestSHA256: strings.Repeat("c", 64), PublisherImage: "localhost/ai-sdlc-artifact-builder@sha256:" + strings.Repeat("d", 64), Output: Registry + ":" + c.ID()[6:38]}
	o := Termination{Version: "ai-sdlc.build-termination/v1", Phase: "build", RequestSHA256: p.BuildRequestSHA256, PipelineRunUID: uid, TaskRunUID: "22222222-2222-2222-2222-222222222222", PodUID: "33333333-3333-3333-3333-333333333333", NodeUID: uid, NodeBootID: uid, SlotDigest: s.Digest(), PodSpecSHA256: strings.Repeat("e", 64), ObservedAt: now.Add(time.Second).UnixMilli()}
	for i, kind := range []string{"init", "normal"} {
		o.Containers = append(o.Containers, ContainerExit{Name: kind, Kind: kind, ImageDigest: "sha256:" + strings.Repeat("f", 64), ContainerID: "containerd://" + strings.Repeat(string(rune('a'+i)), 64), StartedAt: now.Truncate(time.Second).UnixMilli(), FinishedAt: now.Truncate(time.Second).Add(time.Second).UnixMilli()})
	}
	if e := o.Validate(c, p, s, now.Add(time.Second)); e != nil {
		t.Fatal("same-second Kubernetes start rejected", e)
	}
	o.Containers[0].StartedAt -= 1000
	if e := o.Validate(c, p, s, now.Add(time.Second)); e == nil {
		t.Fatal("previous issuance second accepted")
	}
}
