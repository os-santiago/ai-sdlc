// Package tektontasks is the bounded adapter through which ai-sdlc components
// run reviewed Tekton catalog pipelines and read back their bounded results.
//
// Only pipelines named in the project manifest's tekton section can run, with
// targets declared there. A generator tool request supplies nothing but a
// declared target and, for npm, package specifications that pass a strict
// token check; repository, revision, services and Maven switches come from
// the manifest and the runtime. Rendered PipelineRuns reference installed
// pipelines only (Tekton refuses inline specifications) and mirror
// tools/tekton_ci_run.py. See docs/architecture/tekton-task-integration.md.
package tektontasks

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
)

// ErrRequest refuses an invocation outside the manifest's reviewed scope.
var ErrRequest = errors.New("tekton request outside the reviewed catalog scope")

// Namespace, account and cache claim installed by `./install.sh local --tekton-ci`.
const (
	Namespace      = "ai-sdlc-ci"
	ServiceAccount = "ai-sdlc-ci-runner"
	MaxPackages    = 16
)

// CacheClaim is the repository's own Maven cache claim, named from the digest
// of owner/name exactly as tools/tekton_ci_install.py creates it, so a run can
// bind only its own repository's cache.
func CacheClaim(repository string) string {
	sum := sha256.Sum256([]byte(repository))
	return "ai-sdlc-ci-maven-cache-" + hex.EncodeToString(sum[:])[:16]
}

var (
	revision = regexp.MustCompile(`^[0-9a-f]{40}$`)
	// npm update takes package names only (versions fail with EUPDATEARGS)
	// and stays within the package.json ranges.
	npmPackage = regexp.MustCompile(`^(@[a-z0-9][a-z0-9._-]{0,100}/)?[a-z0-9][a-z0-9._-]{0,100}$`)
	repository = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,99}/[a-z0-9][a-z0-9._-]{0,99}$`)
	// pipelines maps catalog names to installed Pipeline objects.
	pipelines = map[string]string{
		project.TektonMavenBuildTest:    "ai-sdlc-maven-build-test",
		project.TektonMavenDependencies: "ai-sdlc-maven-dependencies",
		project.TektonNpmLockUpdate:     "ai-sdlc-npm-lock-update",
	}
)

// Request is one validated invocation of a catalog pipeline.
type Request struct {
	Pipeline   string
	Repository string
	Revision   string
	Target     string
	Services   []string
	MavenArgs  []string
	Mode       string
	Packages   []string
	// BaseHead is the current base-branch head; maven-dependencies, the only
	// writer of the shared cache, runs only when Revision equals it.
	BaseHead string
}

// FromInvocation builds a pre- or post-generation request from the manifest.
// The manifest cannot name maven-dependencies (an operator step that only
// cluster administrators may run); the base-head check below still guards a
// request assembled by other code.
// Pre-generation runs use the base revision; post-generation runs use the
// candidate head. The caller supplies the revision and the current
// base-branch head it observed; the manifest supplies the rest. A
// maven-dependencies request for any revision other than baseHead is refused.
func FromInvocation(m project.Manifest, run project.TektonInvocation, commit, baseHead string) (Request, error) {
	if m.Tekton == nil {
		return Request{}, ErrRequest
	}
	return resolve(m, run.Pipeline, run.Target, nil, commit, baseHead)
}

// FromTool builds a request for a generator tool call. Only target and npm
// packages come from the generator, and both are checked against the
// manifest and strict patterns before anything is rendered.
func FromTool(m project.Manifest, tool, target string, packages []string, commit string) (Request, error) {
	if m.Tekton == nil {
		return Request{}, ErrRequest
	}
	for _, declared := range m.Tekton.Tools {
		if declared.Name == tool {
			// Tools never include maven-dependencies (the manifest refuses
			// it), so no base head is needed.
			return resolve(m, declared.Pipeline, target, packages, commit, "")
		}
	}
	return Request{}, ErrRequest
}

func resolve(m project.Manifest, pipeline, target string, packages []string, commit, baseHead string) (Request, error) {
	r := Request{Pipeline: pipeline, Repository: m.Repository, Revision: commit, Target: target, BaseHead: baseHead}
	switch project.TektonTargetKind(pipeline) {
	case "module":
		module, ok := m.Tekton.Modules[target]
		if !ok || len(packages) > 0 {
			return Request{}, ErrRequest
		}
		r.Services, r.MavenArgs = module.Services, module.MavenArgs
	case "npm":
		declared := false
		for _, directory := range m.Tekton.NpmDirectories {
			declared = declared || directory == target
		}
		if !declared {
			return Request{}, ErrRequest
		}
		r.Mode, r.Packages = "update", packages
		if len(packages) == 0 {
			r.Mode = "audit-fix"
		}
	default:
		return Request{}, ErrRequest
	}
	return r, r.Validate()
}

// Validate re-checks every field against the catalog's own bounds.
func (r Request) Validate() error {
	if pipelines[r.Pipeline] == "" || !repository.MatchString(r.Repository) || !revision.MatchString(r.Revision) {
		return ErrRequest
	}
	if r.Pipeline == project.TektonMavenDependencies && r.Revision != r.BaseHead {
		return ErrRequest
	}
	switch project.TektonTargetKind(r.Pipeline) {
	case "module":
		if !project.TektonTargetPath(r.Target, true) || !project.TektonServiceList(r.Services) ||
			!project.TektonMavenArgs(r.MavenArgs) || r.Mode != "" || len(r.Packages) > 0 {
			return ErrRequest
		}
	case "npm":
		if !project.TektonTargetPath(r.Target, false) || len(r.Services) > 0 || len(r.MavenArgs) > 0 ||
			len(r.Packages) > MaxPackages || (r.Mode == "update") != (len(r.Packages) > 0) ||
			(r.Mode != "update" && r.Mode != "audit-fix") {
			return ErrRequest
		}
		for _, spec := range r.Packages {
			if !npmPackage.MatchString(spec) {
				return ErrRequest
			}
		}
	}
	return nil
}

// PipelineRun renders the request as a PipelineRun object for the dynamic
// client. runtimeClass, when set, applies only to the offline build-test task
// (the installed gVisor class has no network, which fetch tasks need).
func (r Request) PipelineRun(runtimeClass string) (map[string]any, error) {
	if r.Validate() != nil {
		return nil, ErrRequest
	}
	pod := map[string]any{"automountServiceAccountToken": false, "securityContext": map[string]any{
		"runAsNonRoot": true, "runAsUser": int64(65532), "runAsGroup": int64(65532), "fsGroup": int64(65532),
		"seccompProfile": map[string]any{"type": "RuntimeDefault"}}}
	params := []any{param("repository", r.Repository), param("revision", r.Revision)}
	workspaces := []any{map[string]any{"name": "source", "volumeClaimTemplate": map[string]any{"spec": map[string]any{
		"accessModes": []any{"ReadWriteOnce"}, "resources": map[string]any{"requests": map[string]any{"storage": "4Gi"}}}}}}
	switch r.Pipeline {
	case project.TektonMavenBuildTest, project.TektonMavenDependencies:
		params = append(params, param("module", r.Target), param("services", strings.Join(r.Services, ",")),
			param("maven-args", strings2any(r.MavenArgs)))
		cache := map[string]any{"claimName": CacheClaim(r.Repository)}
		if r.Pipeline == project.TektonMavenBuildTest {
			cache["readOnly"] = true
		} else {
			params = append(params, param("maven-mirror-url", ""))
		}
		workspaces = append(workspaces, map[string]any{"name": "maven-cache", "persistentVolumeClaim": cache})
	case project.TektonNpmLockUpdate:
		params = append(params, param("directory", r.Target), param("mode", r.Mode),
			param("packages", strings2any(r.Packages)), param("registry-url", ""))
	}
	spec := map[string]any{
		"pipelineRef":     map[string]any{"name": pipelines[r.Pipeline]},
		"params":          params,
		"timeouts":        map[string]any{"pipeline": "1h0m0s"},
		"taskRunTemplate": map[string]any{"serviceAccountName": ServiceAccount, "podTemplate": pod},
		"workspaces":      workspaces,
	}
	if runtimeClass != "" && r.Pipeline == project.TektonMavenBuildTest {
		sandboxed := map[string]any{"runtimeClassName": runtimeClass}
		for key, value := range pod {
			sandboxed[key] = value
		}
		spec["taskRunSpecs"] = []any{map[string]any{"pipelineTaskName": "build-test", "podTemplate": sandboxed}}
	}
	return map[string]any{
		"apiVersion": "tekton.dev/v1", "kind": "PipelineRun",
		"metadata": map[string]any{
			"generateName": r.Pipeline + "-", "namespace": Namespace,
			"labels": map[string]any{"app.kubernetes.io/part-of": "ai-sdlc", "ai-sdlc.io/pipeline": pipelines[r.Pipeline],
				"ai-sdlc.io/repository": labelValue(r.Repository), "ai-sdlc.io/revision": r.Revision},
			"annotations": map[string]any{"ai-sdlc.io/repository": r.Repository, "ai-sdlc.io/target": r.Target},
		},
		"spec": spec,
	}, nil
}

// labelValue maps owner/name to a label value of at most 63 characters.
func labelValue(value string) string {
	value = strings.ReplaceAll(value, "/", ".")
	if len(value) > 63 {
		value = value[:63]
	}
	return strings.Trim(value, "._-")
}

func param(name string, value any) map[string]any {
	return map[string]any{"name": name, "value": value}
}

func strings2any(values []string) []any {
	out := make([]any, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	return out
}
