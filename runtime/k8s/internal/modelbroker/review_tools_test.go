package modelbroker

import (
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
)

// The review gateway's tool allowlist (chart), the reviewer's SCC autoApprove
// and this response policy must name exactly the same tools: the model is
// offered only what the policy can accept, and the policy still rejects any
// other call (defence in depth).
func TestReviewGatewayToolAllowlistMatchesThePolicy(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/helm/ai-sdlc-k8s-runtime/templates/verification.yaml")
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`\{name: AI_SDLC_MODEL_GATEWAY_TOOLS, value: "([^"]*)"\}`).FindAllSubmatch(raw, -1)
	if len(match) != 1 {
		t.Fatalf("chart allowlist entries: %d", len(match))
	}
	chart := strings.Split(string(match[0][1]), ",")
	slices.Sort(chart)
	sample := map[string]string{"read_file": `{"path":"a"}`, "list_dir": `{}`, "search_text": `{"pattern":"x","path":"a"}`, "code_query": `{"path":"a"}`}
	// Every tool SCC registers (dist/tools/registry.js ALL_TOOLS).
	var accepted []string
	for _, name := range []string{"read_file", "write_file", "edit_file", "list_dir", "search_text", "run_shell", "web_fetch", "memory_read", "memory_write", "git", "code_query", "mcp_validate", "repo_probe"} {
		args, ok := sample[name]
		if !ok {
			args = `{"path":"a"}`
		}
		if reviewArguments(name, args) {
			accepted = append(accepted, name)
		}
	}
	slices.Sort(accepted)
	var config struct {
		Permissions struct {
			AutoApprove []string `json:"autoApprove"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(executor.SCCConfiguration(executor.Profile{Version: executor.ReviewProfileVersion, Model: "m"}, nil), &config); err != nil {
		t.Fatal(err)
	}
	approved := slices.Clone(config.Permissions.AutoApprove)
	slices.Sort(approved)
	if !slices.Equal(chart, accepted) || !slices.Equal(chart, approved) {
		t.Fatalf("chart %v, policy %v, SCC autoApprove %v", chart, accepted, approved)
	}
}
