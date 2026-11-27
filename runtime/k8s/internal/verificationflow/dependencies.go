package verificationflow

import (
	"context"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/podtransport"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

// dependencyStop names why a manifest with verification.dependencies could
// not use a cache. Both stop the verification before any effect.
type dependencyStop string

const (
	// The base tree's dependency key could not be computed.
	stopDependencyKey dependencyStop = "dependencies-key"
	// No sealed, bound cache claim exists for the key: run the operator's
	// prefetch for the base revision (docs/operations/dependency-cache.md).
	stopDependencyUnavailable dependencyStop = "dependencies-unavailable"
)

func (s dependencyStop) Error() string { return string(s) }

// dependencyCache returns the sealed cache claim the runs mount, or "" for a
// manifest without dependencies. The key covers only the base tree: the
// cross tree adds test files and the candidate cannot change dependency
// files without being refused as a harness change.
func dependencyCache(ctx context.Context, client *podtransport.Client, v project.Verification, base workspacebundle.Bundle) (string, error) {
	if v.Dependencies == nil {
		return "", nil
	}
	files := make([]project.DependencyFile, 0, len(base.Files))
	for _, f := range base.Files {
		files = append(files, project.DependencyFile{Path: f.Path, Data: f.Data})
	}
	key, ok := v.DependencyKey(files)
	if !ok {
		return "", stopDependencyKey
	}
	if client.CheckDependencyCache(ctx, key) != nil {
		return "", stopDependencyUnavailable
	}
	return project.DependencyClaimName(key), nil
}
