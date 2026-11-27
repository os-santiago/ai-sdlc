package executor

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/generatortools"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

func toolProfileFixture() Profile {
	p := profileFixture()
	p.MaxSeconds = 900
	p.Tools = &ToolProfile{Calls: 2, Seconds: 600, Spec: generatortools.Spec{Tools: []generatortools.Tool{
		{Name: "build_test", Kind: "maven-build-test", Targets: []string{"rest-villains"}}}}}
	return p
}

// A generator profile with tools parses within its bounds; any profile
// outside them, and any review profile carrying tools, is refused.
func TestToolProfileBounds(t *testing.T) {
	p := toolProfileFixture()
	raw, _ := json.Marshal(p)
	got, err := ParseProfile(raw, workspacebundle.Digest(raw))
	if err != nil || got.Tools == nil || got.Tools.Calls != 2 || got.Tools.Spec.Tools[0].Targets[0] != "rest-villains" {
		t.Fatal("tool profile", err)
	}
	for _, change := range []func(*Profile){
		func(p *Profile) { p.Tools.Calls = 0 },
		func(p *Profile) { p.Tools.Calls = MaxToolCalls + 1 },
		func(p *Profile) { p.Tools.Seconds = MinToolSeconds - 1 },
		func(p *Profile) { p.Tools.Seconds = MaxToolSeconds + 1 },
		func(p *Profile) { p.MaxSeconds = 300 },
		func(p *Profile) { p.Tools.Spec.Tools[0].Targets = []string{"../etc"} },
		func(p *Profile) { p.Tools.Spec.Tools[0].Kind = "shell" },
		func(p *Profile) { p.Tools.Spec = generatortools.Spec{} },
	} {
		q := toolProfileFixture()
		change(&q)
		raw, _ := json.Marshal(q)
		if _, err := ParseProfile(raw, workspacebundle.Digest(raw)); err == nil {
			t.Fatal("out-of-bounds tool profile accepted", q.Tools, q.MaxSeconds)
		}
	}
	review := toolProfileFixture()
	review.Version = ReviewProfileVersion
	raw, _ = json.Marshal(review)
	if _, err := ParseReviewProfile(raw, workspacebundle.Digest(raw)); err == nil {
		t.Fatal("review profile accepted tools")
	}
}

// Tools reach SCC only through its trusted configuration: one MCP server that
// runs the executor's tools-mcp subcommand with the reviewed spec, auto-approved
// tool names, and a timeout past the mailbox window. The mailbox enables its
// tool route with the same bounds. Without tools, nothing changes.
func TestToolProfileConfiguresSCCAndMailbox(t *testing.T) {
	p := toolProfileFixture()
	var cfg struct {
		Permissions struct {
			AutoApprove []string `json:"autoApprove"`
		} `json:"permissions"`
		MCP struct {
			Servers map[string]struct {
				Command   string   `json:"command"`
				Args      []string `json:"args"`
				TimeoutMs int      `json:"timeoutMs"`
			} `json:"servers"`
		} `json:"mcp"`
	}
	if json.Unmarshal(SCCConfiguration(p, nil), &cfg) != nil {
		t.Fatal("config")
	}
	server, ok := cfg.MCP.Servers[ToolServerName]
	if !ok || len(cfg.MCP.Servers) != 1 || server.Command != "/executor" || server.TimeoutMs != 630000 {
		t.Fatal("mcp server", cfg.MCP)
	}
	if len(server.Args) != 5 || server.Args[0] != "tools-mcp" || server.Args[1] != "--spec" || server.Args[3] != "--wait-seconds" || server.Args[4] != "600" {
		t.Fatal("mcp arguments", server.Args)
	}
	var spec generatortools.Spec
	if json.Unmarshal([]byte(server.Args[2]), &spec) != nil || spec.Validate() != nil || spec.Tools[0].Name != "build_test" {
		t.Fatal("mcp spec", server.Args[2])
	}
	if last := cfg.Permissions.AutoApprove[len(cfg.Permissions.AutoApprove)-1]; last != "mcp__ai-sdlc__build_test" {
		t.Fatal("tool not auto-approved", cfg.Permissions.AutoApprove)
	}
	plain := string(SCCConfiguration(profileFixture(), nil))
	if strings.Contains(plain, "mcp") {
		t.Fatal("tools leaked into a plain profile")
	}

	command := func(p Profile) string {
		now := time.Now()
		adev := []byte("reviewed profile fixture")
		p.ADevSHA256 = workspacebundle.Digest(adev)
		profile, _ := json.Marshal(p)
		a := assignmentFixture(now)
		a.ProfileSHA256 = workspacebundle.Digest(profile)
		assignment, _ := json.Marshal(a)
		image := "quay.io/os-santiago/ai-sdlc@sha256:" + strings.Repeat("a", 64)
		w, err := Compile("fixture", image, image, DefaultRuntimeClass, assignment, profile, adev, now)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(w.Pod)
		return string(raw)
	}
	if !strings.Contains(command(p), `"--tool-calls","2","--tool-wait-seconds","600"`) || strings.Contains(command(profileFixture()), "--tool-calls") {
		t.Fatal("mailbox tool route bounds")
	}
}
