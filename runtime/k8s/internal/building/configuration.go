package building

import (
	"encoding/json"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
	"regexp"
)

var uuid = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)
var resourceName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// Slot is installed custody of one bounded tmpfs, not a request to provision a
// volume. Physical storage identity remains stable across policy changes. The
// operational controller must re-observe every UID, boot and mount before use.
// StorageIdentitySHA256 is globally unique observed physical custody; replacing
// Kubernetes identities must never make the same backing workspace reusable.
type Slot struct {
	ClusterUID            string `json:"clusterUID"`
	NamespaceUID          string `json:"namespaceUID"`
	PVCUID                string `json:"pvcUID"`
	PVUID                 string `json:"pvUID"`
	NodeUID               string `json:"nodeUID"`
	NodeBootID            string `json:"nodeBootID"`
	StorageIdentitySHA256 string `json:"storageIdentitySHA256"`
	Namespace             string `json:"namespace"`
	PVC                   string `json:"pvc"`
	Node                  string `json:"node"`
	CapacityBytes         int64  `json:"capacityBytes"`
	InodeLimit            int64  `json:"inodeLimit"`
}

func (s Slot) Validate() error {
	for _, v := range []string{s.ClusterUID, s.NamespaceUID, s.PVCUID, s.PVUID, s.NodeUID, s.NodeBootID} {
		if !uuid.MatchString(v) {
			return ErrBuild
		}
	}
	if s.Namespace != "ai-sdlc-builds" || !resourceName.MatchString(s.PVC) || !resourceName.MatchString(s.Node) || !workspacebundle.ValidDigest(s.StorageIdentitySHA256) || s.CapacityBytes != 4*1024*1024*1024 || s.InodeLimit != 32768 {
		return ErrBuild
	}
	return nil
}
func (s Slot) ID() string {
	return "slot-" + authority.Digest([]byte("ai-sdlc.physical-build-slot/v1\x00"+s.StorageIdentitySHA256))
}
func (s Slot) Digest() string { raw, _ := json.Marshal(s); return authority.Digest(raw) }

// Configuration is loaded from reviewed installation mounts. Issue/model data
// cannot choose a policy, slot, freshness budget or profile revision.
type Configuration struct {
	Inner, Outer                      authority.Policy
	ProfileSHA256, IntakeConfigSHA256 string
	IssueMaxAgeSeconds                int
	Slots                             []Slot
}

func (c Configuration) Validate() error {
	if authority.ValidateLoopPolicies(c.Inner, c.Outer) != nil || !workspacebundle.ValidDigest(c.ProfileSHA256) || !workspacebundle.ValidDigest(c.IntakeConfigSHA256) || c.IssueMaxAgeSeconds < 30 || c.IssueMaxAgeSeconds > 300 || len(c.Slots) < 1 || len(c.Slots) > 8 {
		return ErrBuild
	}
	return c.Binding().Validate()
}

// ProfileDigest binds the reviewed recipe configuration and the exact custody
// inventory/freshness limit. Changing a slot cannot reuse an old build command.
// ProfileBinding is the exact public preimage of the signed configuration digest.
// Field names and order preserve the original v1 digest encoding.
type ProfileBinding struct {
	Version            string
	RecipeProfile      string
	IssueMaxAgeSeconds int
	Slots              []Slot
}

func (c Configuration) Binding() ProfileBinding {
	return ProfileBinding{"ai-sdlc.build-profile/v1", c.ProfileSHA256, c.IssueMaxAgeSeconds, append([]Slot(nil), c.Slots...)}
}
func (b ProfileBinding) Digest() string       { raw, _ := json.Marshal(b); return authority.Digest(raw) }
func (c Configuration) ProfileDigest() string { return c.Binding().Digest() }

func (b ProfileBinding) Validate() error {
	if b.Version != "ai-sdlc.build-profile/v1" || !workspacebundle.ValidDigest(b.RecipeProfile) || b.IssueMaxAgeSeconds < 30 || b.IssueMaxAgeSeconds > 300 || len(b.Slots) < 1 || len(b.Slots) > 8 {
		return ErrBuild
	}
	ids, volumes := map[string]bool{}, map[string]bool{}
	for _, s := range b.Slots {
		if s.Validate() != nil || ids[s.ID()] || volumes[s.ClusterUID+"/"+s.PVCUID] || volumes[s.ClusterUID+"/"+s.PVUID] {
			return ErrBuild
		}
		ids[s.ID()] = true
		volumes[s.ClusterUID+"/"+s.PVCUID] = true
		volumes[s.ClusterUID+"/"+s.PVUID] = true
	}
	return nil
}
