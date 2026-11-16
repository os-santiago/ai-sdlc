package mergegate

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/forge"
)

// This optional probe reads existing GitHub state only. The known closed PR is
// a schema/authentication fixture; it is never a merge candidate or E2E proof.
func TestRealHomedirReadonlyGateFixture(t *testing.T) {
	path := os.Getenv("AI_SDLC_MERGE_GATE_FIXTURE_CREDENTIAL")
	if path == "" {
		t.Skip("explicit read-only GitHub credential fixture required")
	}
	file, e := os.Open(path)
	if e != nil {
		t.Fatal("private credential unavailable")
	}
	defer file.Close()
	info, e := file.Stat()
	if e != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		t.Fatal("private credential mode required")
	}
	raw, e := io.ReadAll(io.LimitReader(file, 16385))
	if e != nil || len(raw) > 16384 {
		t.Fatal("private credential bound")
	}
	var credential forge.Credential
	if authority.DecodeStrict(raw, &credential) != nil {
		t.Fatal("private credential schema")
	}
	c, e := New(credential)
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	_, required, safe, e := c.nativePolicy(ctx, PilotPolicy())
	if e != nil {
		t.Fatal("native policy API observation", e)
	}
	var p pull
	if c.read(ctx, repoPath("/pulls/1580"), nil, &p) != nil || p.Number != 1580 || p.State != "closed" || p.Merged == nil || !*p.Merged {
		t.Fatal("closed read-only fixture identity")
	}
	expected := Expected{Repository: repository, PullRequest: 1580, HeadSHA: p.Head.SHA, BaseSHA: p.Base.SHA, Branch: p.Head.Ref}
	checks, _, e := c.checks(ctx, expected.HeadSHA, required)
	if e != nil {
		t.Fatal("authenticated checks API", e)
	}
	reviews, _, e := c.reviews(ctx, expected)
	if e != nil {
		t.Fatal("authenticated reviews/threads API", e)
	}
	report, _ := json.MarshalIndent(map[string]any{"scope": "read-only-authenticated-github-gate-schema", "repository": repository, "closedPullRequest": 1580, "nativeProtectionSafe": safe, "checksPassed": checks, "reviewFeedbackResolved": reviews, "githubWrites": false, "mergeAuthorized": false, "autonomousE2E": false}, "", "  ")
	if out := os.Getenv("AI_SDLC_MERGE_GATE_FIXTURE_REPORT"); out != "" {
		if os.WriteFile(out, report, 0600) != nil {
			t.Fatal("sanitized report")
		}
	}
}
