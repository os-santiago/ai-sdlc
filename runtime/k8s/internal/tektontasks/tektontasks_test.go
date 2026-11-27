package tektontasks

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	k8syaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
)

const sha = "ae00455c01a7a50d89ae8f1db99483abd27abe25"

func manifest() project.Manifest {
	return project.Manifest{Repository: "os-santiago/quarkus-super-heroes", Tekton: &project.Tekton{
		Modules: map[string]project.TektonModule{"rest-villains": {Services: []string{"postgres"},
			MavenArgs: []string{"-Dtest=!UIResourceTests,!*IT"}}},
		NpmDirectories: []string{"ui-super-heroes/src/main/webui"},
		Pre:            []project.TektonInvocation{{Pipeline: project.TektonMavenBuildTest, Target: "rest-villains"}},
		Tools: []project.TektonTool{{Name: "build_test", Pipeline: project.TektonMavenBuildTest},
			{Name: "npm_lock_update", Pipeline: project.TektonNpmLockUpdate}},
	}}
}

// installedParams reads the reviewed Pipeline's parameter names from the desired state.
func installedParams(t *testing.T, pipeline string) []string {
	raw, err := os.ReadFile(filepath.Join("../../deploy/tekton-ci/ci", "pipeline-"+strings.TrimPrefix(pipeline, "ai-sdlc-")+".yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Metadata struct{ Name string }
		Spec     struct{ Params []struct{ Name string } }
	}
	if err := k8syaml.NewYAMLOrJSONDecoder(strings.NewReader(string(raw)), 4096).Decode(&doc); err != nil || doc.Metadata.Name != pipeline {
		t.Fatal("pipeline desired state", pipeline, err)
	}
	var names []string
	for _, p := range doc.Spec.Params {
		names = append(names, p.Name)
	}
	sort.Strings(names)
	return names
}

func renderedParams(object map[string]any) []string {
	var names []string
	for _, p := range object["spec"].(map[string]any)["params"].([]any) {
		names = append(names, p.(map[string]any)["name"].(string))
	}
	sort.Strings(names)
	return names
}

func TestToolRequestsRenderInstalledPipelinesWithManifestInputs(t *testing.T) {
	m := manifest()
	build, err := FromTool(m, "build_test", "rest-villains", nil, sha)
	if err != nil || build.Services[0] != "postgres" || build.MavenArgs[0] != "-Dtest=!UIResourceTests,!*IT" {
		t.Fatal("build request", build, err)
	}
	npm, err := FromTool(m, "npm_lock_update", "ui-super-heroes/src/main/webui", []string{"axios", "vite@6.3.5"}, sha)
	if err != nil || npm.Mode != "update" {
		t.Fatal("npm request", npm, err)
	}
	audit, err := FromTool(m, "npm_lock_update", "ui-super-heroes/src/main/webui", nil, sha)
	if err != nil || audit.Mode != "audit-fix" {
		t.Fatal("audit request", audit, err)
	}
	for _, r := range []Request{build, npm} {
		object, err := r.PipelineRun("")
		if err != nil {
			t.Fatal(err)
		}
		spec := object["spec"].(map[string]any)
		name := spec["pipelineRef"].(map[string]any)["name"].(string)
		if got, want := renderedParams(object), installedParams(t, name); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatal("parameters drift from the installed pipeline", name, got, want)
		}
		if _, inline := spec["pipelineSpec"]; inline || spec["taskRunSpecs"] != nil {
			t.Fatal("inline spec or unrequested sandbox", name)
		}
		pod := spec["taskRunTemplate"].(map[string]any)["podTemplate"].(map[string]any)
		if pod["automountServiceAccountToken"] != false {
			t.Fatal("API token mounted")
		}
	}
	object, _ := build.PipelineRun("ai-sdlc-offline")
	spec := object["spec"].(map[string]any)
	cache := spec["workspaces"].([]any)[1].(map[string]any)["persistentVolumeClaim"].(map[string]any)
	if cache["readOnly"] != true {
		t.Fatal("build-test can write the shared cache")
	}
	sandbox := spec["taskRunSpecs"].([]any)[0].(map[string]any)
	if sandbox["pipelineTaskName"] != "build-test" || sandbox["podTemplate"].(map[string]any)["runtimeClassName"] != "ai-sdlc-offline" {
		t.Fatal("sandbox not applied to build-test", sandbox)
	}
	npmObject, _ := npm.PipelineRun("ai-sdlc-offline")
	if npmObject["spec"].(map[string]any)["taskRunSpecs"] != nil {
		t.Fatal("the offline sandbox would cut the npm fetch off")
	}
}

func TestRequestsOutsideTheManifestAreRefused(t *testing.T) {
	m := manifest()
	for name, call := range map[string]func() (Request, error){
		"undeclared tool":   func() (Request, error) { return FromTool(m, "shell", "rest-villains", nil, sha) },
		"undeclared module": func() (Request, error) { return FromTool(m, "build_test", "rest-heroes", nil, sha) },
		"module traversal":  func() (Request, error) { return FromTool(m, "build_test", "../rest-villains", nil, sha) },
		"maven packages":    func() (Request, error) { return FromTool(m, "build_test", "rest-villains", []string{"x"}, sha) },
		"undeclared dir":    func() (Request, error) { return FromTool(m, "npm_lock_update", "other", []string{"axios"}, sha) },
		"package injection": func() (Request, error) {
			return FromTool(m, "npm_lock_update", "ui-super-heroes/src/main/webui", []string{"axios; rm -rf /"}, sha)
		},
		"package url": func() (Request, error) {
			return FromTool(m, "npm_lock_update", "ui-super-heroes/src/main/webui", []string{"git+https://x/y"}, sha)
		},
		"too many packages": func() (Request, error) {
			return FromTool(m, "npm_lock_update", "ui-super-heroes/src/main/webui", make17(), sha)
		},
		"branch revision": func() (Request, error) { return FromTool(m, "build_test", "rest-villains", nil, "main") },
		"no section":      func() (Request, error) { return FromTool(project.Manifest{}, "build_test", "rest-villains", nil, sha) },
	} {
		if _, err := call(); !errors.Is(err, ErrRequest) {
			t.Fatal("accepted:", name, err)
		}
	}
	if _, err := (Request{Pipeline: "shell", Repository: m.Repository, Revision: sha}).PipelineRun(""); err == nil {
		t.Fatal("rendered an unknown pipeline")
	}
	pre, err := FromInvocation(m, m.Tekton.Pre[0], sha, sha)
	if err != nil || pre.Pipeline != project.TektonMavenBuildTest {
		t.Fatal("pre invocation", err)
	}
	deps := project.TektonInvocation{Pipeline: project.TektonMavenDependencies, Target: "rest-villains"}
	if _, err := FromInvocation(m, deps, sha, strings.Repeat("0", 40)); !errors.Is(err, ErrRequest) {
		t.Fatal("maven-dependencies accepted a revision other than the base head", err)
	}
	if _, err := FromInvocation(m, deps, sha, ""); !errors.Is(err, ErrRequest) {
		t.Fatal("maven-dependencies accepted an unknown base head", err)
	}
	filled, err := FromInvocation(m, deps, sha, sha)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (Request{Pipeline: project.TektonMavenDependencies, Repository: m.Repository, Revision: sha, Target: "rest-villains",
		Services: []string{"none"}}).PipelineRun(""); err == nil {
		t.Fatal("rendered maven-dependencies without a base head")
	}
	object, _ := filled.PipelineRun("")
	cache := object["spec"].(map[string]any)["workspaces"].([]any)[1].(map[string]any)["persistentVolumeClaim"].(map[string]any)
	if cache["readOnly"] != nil || cache["claimName"] != CacheClaim(m.Repository) {
		t.Fatal("dependencies cache binding", cache)
	}
}

// The installer (tools/tekton_ci_install.py cache_claim) created exactly this
// claim for the repository in the disposable cluster run.
func TestCacheClaimMatchesTheInstaller(t *testing.T) {
	if got := CacheClaim("os-santiago/quarkus-super-heroes"); got != "ai-sdlc-ci-maven-cache-ccbf9cd8126eafc6" {
		t.Fatal(got)
	}
	if CacheClaim("os-santiago/a") == CacheClaim("os-santiago/b") {
		t.Fatal("repositories share a cache")
	}
}

func make17() []string {
	out := make([]string, 17)
	for i := range out {
		out[i] = "a"
	}
	return out
}

func TestSummariesAreDecodedStrictlyAndBounded(t *testing.T) {
	good := `{"module":"rest-villains","mavenExitCode":0,"reports":6,"tests":94,"failures":0,"errors":0,"skipped":0,"failingTests":[],"compilerErrors":[],"missingDependencies":false,"truncated":false}`
	if s, err := ParseMavenSummary(good); err != nil || s.Tests != 94 || !s.Passed() {
		t.Fatal("maven summary", err)
	}
	failing := `{"module":"rest-villains","mavenExitCode":1,"reports":6,"tests":94,"failures":0,"errors":3,"skipped":0,"failingTests":["a.B.c"],"compilerErrors":[],"missingDependencies":false,"truncated":false}`
	if s, err := ParseMavenSummary(failing); err != nil || s.Passed() || s.FailingTests[0] != "a.B.c" {
		t.Fatal("failing summary", err)
	}
	for _, bad := range []string{
		strings.Replace(good, `"module"`, `"extra":1,"module"`, 1),
		strings.Replace(good, `"failingTests":[]`, `"failingTests":["a","b","c","d","e","f","g","h","i","j","k"]`, 1),
		strings.Replace(good, `"failingTests":[],`, ``, 1),
		strings.Replace(good, `"mavenExitCode":0`, `"mavenExitCode":-1`, 1),
		good + ` {}`, strings.Repeat(" ", MaxResultBytes) + good, "",
	} {
		if _, err := ParseMavenSummary(bad); err == nil {
			t.Fatal("accepted", bad[:min(60, len(bad))])
		}
	}
	npm := `{"directory":"ui","mode":"update","npmExitCode":0,"changed":2,"changes":[{"path":"axios","from":"1.21.0","to":"1.21.1"},{"path":"new","from":null,"to":"1.0.0"}],"truncated":false,"resolvedHosts":["registry.npmjs.org"],"newResolvedHosts":[],"diffBytes":3230,"diffSHA256":"` + strings.Repeat("f", 64) + `"}`
	if s, err := ParseNpmSummary(npm); err != nil || len(s.Changes) != 2 || s.Changes[1].From != nil || !s.Passed() {
		t.Fatal("npm summary", err)
	}
	for _, bad := range []string{strings.Replace(npm, `"changed":2`, `"changed":1`, 1), strings.Replace(npm, `"update"`, `"install"`, 1),
		strings.Replace(npm, strings.Repeat("f", 64), "zz", 1), strings.Replace(npm, `"newResolvedHosts":[],`, ``, 1)} {
		if _, err := ParseNpmSummary(bad); err == nil {
			t.Fatal("accepted npm", bad[:60])
		}
	}
}

type fakeCluster struct {
	reads     []map[string]any
	fails     int
	created   map[string]any
	cancelled string
}

func (f *fakeCluster) Cancel(_ context.Context, name string) error {
	f.cancelled = name
	return nil
}

func (f *fakeCluster) Create(_ context.Context, object map[string]any) (string, error) {
	f.created = object
	return "run-1", nil
}

func (f *fakeCluster) Get(context.Context, string) (map[string]any, error) {
	if f.fails > 0 {
		f.fails--
		return nil, errors.New("transient")
	}
	next := f.reads[0]
	if len(f.reads) > 1 {
		f.reads = f.reads[1:]
	}
	return next, nil
}

func status(state, commit string) map[string]any {
	return map[string]any{"metadata": map[string]any{"name": "run-1", "uid": "u-1"}, "status": map[string]any{
		"conditions": []any{map[string]any{"type": "Succeeded", "status": state, "reason": "r"}},
		"results":    []any{map[string]any{"name": "commit", "value": commit}, map[string]any{"name": "summary", "value": "{}"}}}}
}

func TestRunPollsToCompletionAndBindsTheRequestedCommit(t *testing.T) {
	request, _ := FromTool(manifest(), "build_test", "rest-villains", nil, sha)
	cluster := &fakeCluster{reads: []map[string]any{status("Unknown", ""), status("True", sha)}, fails: 1}
	outcome, err := Run(context.Background(), cluster, request, "", time.Millisecond)
	if err != nil || !outcome.Succeeded || outcome.Commit != sha || outcome.UID != "u-1" || cluster.created == nil {
		t.Fatal("run", outcome, err)
	}
	other := &fakeCluster{reads: []map[string]any{status("False", strings.Repeat("0", 40))}}
	if _, err := Run(context.Background(), other, request, "", time.Millisecond); !errors.Is(err, ErrOutcome) {
		t.Fatal("accepted a run of another commit", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	stuck := &fakeCluster{reads: []map[string]any{status("Unknown", "")}}
	if _, err := Run(ctx, stuck, request, "", time.Millisecond); !errors.Is(err, ErrDeadline) || stuck.cancelled != "run-1" {
		t.Fatal("unbounded wait or run left running", err, stuck.cancelled)
	}
	if _, err := Run(context.Background(), &fakeCluster{reads: []map[string]any{status("True", "")}}, request, "", time.Millisecond); !errors.Is(err, ErrOutcome) {
		t.Fatal("accepted a succeeded run without its commit", err)
	}
	if out, err := Run(context.Background(), &fakeCluster{reads: []map[string]any{status("False", "")}}, request, "", time.Millisecond); err != nil || out.Succeeded {
		t.Fatal("a run whose checkout failed is a failure, not an error", err)
	}
	// A failing build is a succeeded pipeline whose summary carries the verdict.
	failed := status("True", sha)
	failed["status"].(map[string]any)["results"] = []any{map[string]any{"name": "commit", "value": sha}, map[string]any{"name": "summary",
		"value": `{"module":"rest-villains","mavenExitCode":1,"reports":1,"tests":2,"failures":1,"errors":0,"skipped":0,"failingTests":["a.B.c"],"compilerErrors":[],"missingDependencies":false,"truncated":false}`}}
	out, err := Run(context.Background(), &fakeCluster{reads: []map[string]any{failed}}, request, "", time.Millisecond)
	summary, perr := ParseMavenSummary(out.Summary)
	if err != nil || perr != nil || !out.Succeeded || summary.Passed() || summary.FailingTests[0] != "a.B.c" {
		t.Fatal("failing build outcome", err, perr, summary)
	}
	huge := status("True", sha)
	huge["status"].(map[string]any)["results"] = []any{map[string]any{"name": "summary", "value": strings.Repeat("x", MaxResultBytes+1)}}
	if _, err := Run(context.Background(), &fakeCluster{reads: []map[string]any{huge}}, request, "", time.Millisecond); !errors.Is(err, ErrOutcome) {
		t.Fatal("accepted an oversized result", err)
	}
}
