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

// Verification runs offline with only the image's packages, so the guidance
// asks for the existing test framework and names the reviewed command.
func TestOfflineWorkspaceGuidanceNamesFrameworkAndVerificationCommand(t *testing.T) {
	a := assignmentFixture(time.Now())
	a.Workspace = WorkspaceOfflineTests
	plain := BuildPrompt(a, "policy")
	flat := strings.Join(strings.Fields(plain), " ")
	for _, rule := range []string{"same test framework, imports and conventions as the repository's existing tests", "add no new dependency", "only what its image already provides"} {
		if !strings.Contains(flat, rule) {
			t.Fatal("framework guidance missing", rule)
		}
	}
	if strings.Contains(plain, "Verification runs this command") {
		t.Fatal("command named without a reviewed verification")
	}
	raw, _ := json.Marshal(a)
	if strings.Contains(string(raw), "verification") {
		t.Fatal("absent verification changed the assignment bytes")
	}
	a.Verification = &Verification{Command: []string{"python3", "-I", "-B", "-m", "unittest", "discover", "-s", ".", "-t", ".", "-p", "test_*.py"}}
	prompt := BuildPrompt(a, "policy")
	command := strings.Index(prompt, "from the repository root:\npython3 -I -B -m unittest discover -s . -t . -p 'test_*.py'\n")
	untrusted := strings.Index(prompt, "<untrusted-issue-data>")
	if command < 0 || untrusted < 0 || command > untrusted || strings.Contains(prompt, "run_shell") {
		t.Fatal("verification command guidance", prompt)
	}
	raw, _ = json.Marshal(a)
	got, err := ParseAssignment(raw, workspacebundle.Digest(raw), time.Now())
	if err != nil || got.Verification == nil || got.Verification.Command[0] != "python3" {
		t.Fatal("verification refused", err)
	}
	a.Verification.WorkingDir = "svc"
	if !strings.Contains(BuildPrompt(a, "policy"), "from the repository directory svc:\n") {
		t.Fatal("working directory not named")
	}
	for name, mutate := range map[string]func(*Assignment){
		"no workspace":   func(a *Assignment) { a.Workspace = "" },
		"empty command":  func(a *Assignment) { a.Verification.Command = nil },
		"hidden rune":    func(a *Assignment) { a.Verification.Command = []string{"python3", "x\ny"} },
		"escaping dir":   func(a *Assignment) { a.Verification.WorkingDir = "../x" },
		"with toolchain": func(a *Assignment) { a.Toolchain = toolchainFixture() },
	} {
		b := a
		v := *a.Verification
		b.Verification = &v
		mutate(&b)
		raw, _ := json.Marshal(b)
		if _, err := ParseAssignment(raw, workspacebundle.Digest(raw), time.Now()); err == nil {
			t.Fatal("invalid verification accepted:", name)
		}
	}
}
