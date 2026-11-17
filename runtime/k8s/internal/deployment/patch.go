package deployment

import (
	"encoding/json"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/building"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/releasing"
	"regexp"
	"strings"
	"time"
)

var resourceVersion = regexp.MustCompile(`^[1-9][0-9]{0,30}$`)
var digest = regexp.MustCompile(`^[a-f0-9]{64}$`)

func candidateImage(s string) bool {
	return strings.HasPrefix(s, building.Registry+"@sha256:") && len(s) == len(building.Registry+"@sha256:")+64 && digest.MatchString(strings.TrimPrefix(s, building.Registry+"@sha256:"))
}

// ConsolePatch compiles only tests and a single image replacement. All objects,
// paths and operation choices are compiled, never copied from issue contents.
func CompilePatch(t releasing.Target, before releasing.Snapshot, image string, now time.Time) ([]byte, error) {
	if t.Validate(now) != nil || before.DeploymentUID != t.DeploymentUID || before.TargetSHA256 != t.Digest() || !resourceVersion.MatchString(before.ResourceVersion) || before.Generation < 1 || before.Image == image || (!candidateImage(image) && image != t.BaselineImage) {
		return nil, ErrDeployment
	}
	patch := []map[string]any{
		{"op": "test", "path": "/metadata/uid", "value": before.DeploymentUID},
		{"op": "test", "path": "/metadata/resourceVersion", "value": before.ResourceVersion},
		{"op": "test", "path": "/metadata/generation", "value": before.Generation},
		{"op": "test", "path": "/spec/template/spec/containers/0/name", "value": "console"},
		{"op": "test", "path": "/spec/template/spec/containers/0/image", "value": before.Image},
		{"op": "replace", "path": "/spec/template/spec/containers/0/image", "value": image},
	}
	raw, e := json.Marshal(patch)
	if e != nil {
		return nil, ErrDeployment
	}
	return raw, nil
}
