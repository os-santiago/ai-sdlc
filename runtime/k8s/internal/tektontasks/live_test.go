package tektontasks

import (
	"context"
	"os"
	"testing"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	outcome, err := Run(ctx, DynamicCluster{Client: client}, request, "", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	summary, err := ParseMavenSummary(outcome.Summary)
	if err != nil || !outcome.Succeeded || outcome.Commit != sha || summary.Tests == 0 || summary.MavenExitCode != 0 {
		t.Fatal("live outcome", outcome, summary, err)
	}
	t.Logf("%s: %d tests, %d failures, %d errors", outcome.Name, summary.Tests, summary.Failures, summary.Errors)
}
