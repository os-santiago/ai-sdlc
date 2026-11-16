package building

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

const Registry = "registry-service.kind-registry/ai-sdlc-fixtures/saferus"

var publisherImage = regexp.MustCompile(`^(quay.io/os-santiago/ai-sdlc|localhost/ai-sdlc-artifact-builder)@sha256:[a-f0-9]{64}$`)
var containerID = regexp.MustCompile(`^containerd://[a-f0-9]{64}$`)
var imageDigest = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// Plan is compiled by trusted runtime code, then persisted before either POST.
// Digests bind the exact request bodies; this is not an arbitrary-manifest API.
type Plan struct {
	Version              string `json:"version"`
	BuildID              string `json:"buildID"`
	SourceSHA            string `json:"sourceSHA"`
	TreeSHA              string `json:"treeSHA"`
	ProfileSHA256        string `json:"profileSHA256"`
	SlotDigest           string `json:"slotDigest"`
	DefinitionSHA256     string `json:"definitionSHA256"`
	BuildRequestSHA256   string `json:"buildRequestSHA256"`
	PublishRequestSHA256 string `json:"publishRequestSHA256"`
	PublisherImage       string `json:"publisherImage"`
	Output               string `json:"output"`
}

func (p Plan) Digest() string { raw, _ := json.Marshal(p); return authority.Digest(raw) }
func (p Plan) Validate(c Command, slot Slot) error {
	if p.Version != "ai-sdlc.build-plan/v1" || p.BuildID != c.ID() || p.SourceSHA != c.SourceSHA || p.TreeSHA != c.TreeSHA || p.ProfileSHA256 != c.ProfileSHA256 || p.SlotDigest != slot.Digest() || !publisherImage.MatchString(p.PublisherImage) || p.Output != Registry+":"+strings.TrimPrefix(c.ID(), "build-")[:32] {
		return ErrBuild
	}
	for _, v := range []string{p.DefinitionSHA256, p.BuildRequestSHA256, p.PublishRequestSHA256} {
		if !workspacebundle.ValidDigest(v) {
			return ErrBuild
		}
	}
	if p.BuildRequestSHA256 == p.PublishRequestSHA256 {
		return ErrBuild
	}
	return nil
}

// ContainerExit is normalized from an authenticated original Pod observation.
// The transport must independently prove complete spec/status coverage and
// admitted profile equality; these scalar checks alone cannot establish it.
type ContainerExit struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	ImageDigest string `json:"imageDigest"`
	ContainerID string `json:"containerID"`
	StartedAt   int64  `json:"startedAt"`
	FinishedAt  int64  `json:"finishedAt"`
	ExitCode    int    `json:"exitCode"`
	Restarts    int    `json:"restarts"`
}
type Termination struct {
	Version        string          `json:"version"`
	Phase          string          `json:"phase"`
	RequestSHA256  string          `json:"requestSHA256"`
	PipelineRunUID string          `json:"pipelineRunUID"`
	TaskRunUID     string          `json:"taskRunUID"`
	PodUID         string          `json:"podUID"`
	NodeUID        string          `json:"nodeUID"`
	NodeBootID     string          `json:"nodeBootID"`
	SlotDigest     string          `json:"slotDigest"`
	PodSpecSHA256  string          `json:"podSpecSHA256"`
	Containers     []ContainerExit `json:"containers"`
	ObservedAt     int64           `json:"observedAt"`
}

func (o Termination) Digest() string { raw, _ := json.Marshal(o); return authority.Digest(raw) }
func (o Termination) Validate(c Command, p Plan, s Slot, now time.Time) error {
	expected := p.BuildRequestSHA256
	if o.Phase == "publish" {
		expected = p.PublishRequestSHA256
	} else if o.Phase != "build" {
		return ErrBuild
	}
	if o.Version != "ai-sdlc.build-termination/v1" || p.Validate(c, s) != nil || o.RequestSHA256 != expected || o.SlotDigest != s.Digest() || o.NodeUID != s.NodeUID || o.NodeBootID != s.NodeBootID || !workspacebundle.ValidDigest(o.PodSpecSHA256) || len(o.Containers) < 2 || len(o.Containers) > 16 {
		return ErrBuild
	}
	seenUID := map[string]bool{}
	for _, v := range []string{o.PipelineRunUID, o.TaskRunUID, o.PodUID} {
		if !uuid.MatchString(v) || seenUID[v] {
			return ErrBuild
		}
		seenUID[v] = true
	}
	observed := time.UnixMilli(o.ObservedAt)
	if observed.Before(now.Add(-30*time.Second)) || observed.After(now.Add(5*time.Second)) || o.ObservedAt < c.IssuedAt || o.ObservedAt >= c.ExpiresAt {
		return ErrBuild
	}
	seen := map[string]bool{}
	ids := map[string]bool{}
	kinds := map[string]int{}
	for _, v := range o.Containers {
		key := v.Name
		if !resourceName.MatchString(v.Name) || (v.Kind != "init" && v.Kind != "normal") || seen[key] || ids[v.ContainerID] || !imageDigest.MatchString(v.ImageDigest) || !containerID.MatchString(v.ContainerID) || v.ExitCode != 0 || v.Restarts != 0 || v.StartedAt < c.IssuedAt || v.FinishedAt < v.StartedAt || v.FinishedAt > o.ObservedAt {
			return ErrBuild
		}
		seen[key] = true
		ids[v.ContainerID] = true
		kinds[v.Kind]++
	}
	if kinds["init"] == 0 || kinds["normal"] == 0 {
		return ErrBuild
	}
	return nil
}

// Result is historical publication evidence awaiting independent provenance
// verification. Successful publication never grants promotion or deployment.
type Result struct {
	Version                  string `json:"version"`
	BuildID                  string `json:"buildID"`
	PlanSHA256               string `json:"planSHA256"`
	ImageURL                 string `json:"imageURL"`
	ImageDigest              string `json:"imageDigest"`
	SourceSHA                string `json:"sourceSHA"`
	TreeSHA                  string `json:"treeSHA"`
	DefinitionSHA256         string `json:"definitionSHA256"`
	PublisherImage           string `json:"publisherImage"`
	BuildRunUID              string `json:"buildRunUID"`
	PublishRunUID            string `json:"publishRunUID"`
	BuildObservationSHA256   string `json:"buildObservationSHA256"`
	PublishObservationSHA256 string `json:"publishObservationSHA256"`
	ReleaseAuthorized        bool   `json:"releaseAuthorized"`
}

func (r Result) Validate(c Command, p Plan, b, pub Termination) error {
	if r.Version != "ai-sdlc.build-result/v1" || r.BuildID != c.ID() || r.PlanSHA256 != p.Digest() || r.ImageURL != Registry || !imageDigest.MatchString(r.ImageDigest) || r.SourceSHA != c.SourceSHA || r.TreeSHA != c.TreeSHA || r.DefinitionSHA256 != p.DefinitionSHA256 || r.PublisherImage != p.PublisherImage || r.BuildRunUID != b.PipelineRunUID || r.PublishRunUID != pub.PipelineRunUID || r.BuildObservationSHA256 != b.Digest() || r.PublishObservationSHA256 != pub.Digest() || r.ReleaseAuthorized {
		return ErrBuild
	}
	return nil
}
