package tektontasks

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
)

// TestLiveRunAgainstAnOwnedCluster runs maven-build-test through the adapter
// on a cluster prepared with `./install.sh local --tekton-ci` and a filled
// Maven cache. It is skipped unless AI_SDLC_TEKTON_API names a local
// `kubectl proxy` for that cluster (for example http://127.0.0.1:8001), so the
// test itself handles no credential; it never runs in CI.
func TestLiveRunAgainstAnOwnedCluster(t *testing.T) {
	host := os.Getenv("AI_SDLC_TEKTON_API")
	if host == "" {
		t.Skip("set AI_SDLC_TEKTON_API to a kubectl proxy of an owned Tekton CI cluster")
	}
	client, err := dynamic.NewForConfig(&rest.Config{Host: host})
	if err != nil {
		t.Fatal(err)
	}
	request, err := FromTool(manifest(), "build_test", "rest-villains", nil, sha)
	if err != nil {
		t.Fatal(err)
	}
	request.MavenArgs = append(request.MavenArgs, "-Dit.test=*IT,!UIResourceIT",
		"-Dsurefire.failIfNoSpecifiedTests=false", "-Dfailsafe.failIfNoSpecifiedTests=false")
	// The second run points the datasource at a closed port: a failing build
	// must still return its failing tests.
	failing := request
	failing.MavenArgs = append(append([]string{}, request.MavenArgs...), "-Dquarkus.datasource.jdbc.url=jdbc:postgresql://127.0.0.1:1/ci")
	for _, c := range []struct {
		request Request
		pass    bool
	}{{request, true}, {failing, false}} {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		outcome, err := Run(ctx, DynamicCluster{Client: client}, c.request, "", 10*time.Second)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		summary, err := ParseMavenSummary(outcome.Summary)
		if err != nil || !outcome.Succeeded || outcome.Commit != sha || summary.Tests == 0 || summary.Passed() != c.pass ||
			(!c.pass && len(summary.FailingTests) == 0) {
			t.Fatal("live outcome", outcome, summary, err)
		}
		t.Logf("%s: passed=%v %d tests, %d failures, %d errors, failing %v", outcome.Name, summary.Passed(), summary.Tests,
			summary.Failures, summary.Errors, summary.FailingTests)
	}
}

// TestLiveToolCallOnACandidate runs the build_test tool on candidate diffs
// against an owned cluster: AI_SDLC_TEKTON_API (a kubectl proxy) and
// AI_SDLC_TEKTON_DIFFS (a colon-separated list of PASS=path or FAIL=path).
func TestLiveToolCallOnACandidate(t *testing.T) {
	host, diffs := os.Getenv("AI_SDLC_TEKTON_API"), os.Getenv("AI_SDLC_TEKTON_DIFFS")
	if host == "" || diffs == "" {
		t.Skip("set AI_SDLC_TEKTON_API and AI_SDLC_TEKTON_DIFFS to run against an owned Tekton CI cluster")
	}
	client, err := dynamic.NewForConfig(&rest.Config{Host: host})
	if err != nil {
		t.Fatal(err)
	}
	m := manifest()
	m.Tekton.Modules["rest-villains"] = project.TektonModule{Services: []string{"postgres"}, MavenArgs: []string{
		"-Dtest=!UIResourceTests,!*IT", "-Dit.test=*IT,!UIResourceIT", "-Dsurefire.failIfNoSpecifiedTests=false",
		"-Dfailsafe.failIfNoSpecifiedTests=false"}}
	budget := &ToolBudget{MaxCalls: 4, MaxSeconds: 3600}
	for _, entry := range strings.Split(diffs, ":") {
		want, path, _ := strings.Cut(entry, "=")
		diff, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		result, err := InvokeTool(ctx, DynamicCluster{Client: client}, &memJournal{}, budget, m,
			ToolCall{Tool: "build_test", Target: "rest-villains", Diff: string(diff)}, sha, os.Getenv("AI_SDLC_TEKTON_RUNTIME_CLASS"), 10*time.Second)
		cancel()
		if err != nil {
			t.Fatal(path, err)
		}
		summary, _ := ParseMavenSummary(string(result.Summary))
		if result.Passed != (want == "PASS") || result.Tree == "" {
			t.Fatal(path, result.Text())
		}
		t.Logf("%s: %s passed=%v tree=%s tests=%d compilerErrors=%v", path, result.PipelineRun, result.Passed, result.Tree,
			summary.Tests, summary.CompilerErrors)
	}
}
