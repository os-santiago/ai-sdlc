package executor

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

func TestOfflineWorkspaceGuidanceIsTrustedContextOutsideIssueData(t *testing.T) {
	a := assignmentFixture(time.Now())
	a.Workspace = WorkspaceOfflineTests
	prompt := BuildPrompt(a, "policy")
	guidance := strings.Index(prompt, "no build toolchain")
	untrusted := strings.Index(prompt, "<untrusted-issue-data>")
	if guidance < 0 || untrusted < 0 || guidance > untrusted {
		t.Fatal("guidance must precede the untrusted issue block")
	}
	a.Workspace = ""
	if strings.Contains(BuildPrompt(a, "policy"), "no build toolchain") {
		t.Fatal("guidance without the workspace mark")
	}
	a.Workspace = "anything else"
	raw, _ := json.Marshal(a)
	if _, err := ParseAssignment(raw, workspacebundle.Digest(raw), time.Now()); err == nil {
		t.Fatal("unknown workspace accepted")
	}
	a.Workspace = WorkspaceOfflineTests
	raw, _ = json.Marshal(a)
	if _, err := ParseAssignment(raw, workspacebundle.Digest(raw), time.Now()); err != nil {
		t.Fatal("offline workspace refused", err)
	}
}
