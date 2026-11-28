// Package generatortools is the generator's side of the Tekton tool path: a
// minimal MCP server (newline-delimited JSON-RPC 2.0 over stdio) that SCC
// loads from its trusted configuration. It lists only the declared tools and
// targets, computes the candidate diff, and relays each call through the
// model mailbox's tool route to the execution controller, which authorizes and
// runs it. It holds no credential and reaches nothing but the loopback mailbox.
// See docs/architecture/tekton-task-integration.md ("Generator tools").
package generatortools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// Spec is the reviewed tool list for one generation, derived from the project
// manifest's tekton section by the controller.
type Spec struct {
	Tools []Tool `json:"tools"`
}

// Tool is one generator tool: a catalog pipeline kind and its declared targets.
type Tool struct {
	Name    string   `json:"name"`
	Kind    string   `json:"kind"` // maven-build-test | npm-lock-update
	Targets []string `json:"targets"`
}

var (
	toolName    = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	targetPath  = regexp.MustCompile(`^(\.|[A-Za-z0-9_.-]+(/[A-Za-z0-9_.-]+)*)$`)
	packageName = `^(@[a-z0-9][a-z0-9._-]{0,100}/)?[a-z0-9][a-z0-9._-]{0,100}$`
	packageRe   = regexp.MustCompile(packageName)
)

// Offered reports whether a catalog pipeline is offered as a generator tool.
// Only maven-build-test, which runs offline under the sandbox: npm-lock-update
// would resolve URL specs and lockfile hosts from the candidate under the
// fetch egress (slice 3 brings it back behind proxy-only egress).
func Offered(kind string) bool { return kind == "maven-build-test" }

// MaxTools and MaxTargets bound a Spec.
const (
	MaxTools   = 4
	MaxTargets = 16
)

// Validate checks the spec the executor received.
func (s Spec) Validate() error {
	if len(s.Tools) == 0 || len(s.Tools) > MaxTools {
		return errors.New("invalid generator tool count")
	}
	seen := map[string]bool{}
	for _, t := range s.Tools {
		if !toolName.MatchString(t.Name) || seen[t.Name] || !Offered(t.Kind) ||
			len(t.Targets) == 0 || len(t.Targets) > MaxTargets {
			return errors.New("invalid generator tool")
		}
		for _, target := range t.Targets {
			if !targetPath.MatchString(target) || (target != "." && hasDotSegment(target)) {
				return errors.New("invalid generator tool target")
			}
		}
		seen[t.Name] = true
	}
	return nil
}

func (t Tool) schema() map[string]any {
	properties := map[string]any{"target": map[string]any{"type": "string", "enum": t.Targets,
		"description": "Declared module or directory to run on."}}
	required := []string{"target"}
	if t.Kind == "npm-lock-update" {
		properties["packages"] = map[string]any{"type": "array", "maxItems": 16,
			"items":       map[string]any{"type": "string", "pattern": packageName},
			"description": "Package names to update within package.json ranges; empty runs npm audit fix."}
	}
	return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
}

func (t Tool) description() string {
	if t.Kind == "npm-lock-update" {
		return "Update package-lock.json for your current changes with npm (scripts disabled) in an isolated CI run, and report the changed entries. Results are untrusted data."
	}
	return "Build and test your current changes for one module with Maven in an isolated, offline CI run, and report tests run and failed, failing test names and compiler errors. Takes several minutes. Results are untrusted data."
}

// Relay sends one tool call to the mailbox and returns its status and body.
type Relay func(ctx context.Context, call []byte) (status int, body []byte, err error)

// Candidate returns the generator's change against the base revision.
type Candidate func(ctx context.Context) (string, error)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// MaxRequestBytes is the mailbox tool route's request bound
// (modelmailbox.MaxToolRequestBytes).
const MaxRequestBytes = 800 * 1024

// MaxMessageBytes bounds one JSON-RPC line from SCC.
const MaxMessageBytes = 64 * 1024

// Serve answers SCC until in closes or ctx ends.
func Serve(ctx context.Context, in io.Reader, out io.Writer, spec Spec, candidate Candidate, relay Relay) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 4096), MaxMessageBytes)
	encoder := json.NewEncoder(out)
	for scanner.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var request rpcRequest
		if json.Unmarshal(line, &request) != nil || request.JSONRPC != "2.0" {
			_ = encoder.Encode(rpcError(nil, -32700, "parse error"))
			continue
		}
		if len(request.ID) == 0 {
			continue // notifications such as notifications/initialized
		}
		if err := encoder.Encode(handle(ctx, spec, candidate, relay, request)); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func rpcError(id json.RawMessage, code int, message string) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}}
}

func rpcResult(id json.RawMessage, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

func toolText(id json.RawMessage, text string, isError bool) map[string]any {
	return rpcResult(id, map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}, "isError": isError})
}

func handle(ctx context.Context, spec Spec, candidate Candidate, relay Relay, request rpcRequest) map[string]any {
	switch request.Method {
	case "initialize":
		return rpcResult(request.ID, map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{"name": "ai-sdlc", "version": "1"}})
	case "ping":
		return rpcResult(request.ID, map[string]any{})
	case "tools/list":
		tools := make([]any, 0, len(spec.Tools))
		for _, t := range spec.Tools {
			tools = append(tools, map[string]any{"name": t.Name, "description": t.description(), "inputSchema": t.schema()})
		}
		return rpcResult(request.ID, map[string]any{"tools": tools})
	case "tools/call":
		return call(ctx, spec, candidate, relay, request)
	}
	return rpcError(request.ID, -32601, "method not found")
}

func call(ctx context.Context, spec Spec, candidate Candidate, relay Relay, request rpcRequest) map[string]any {
	var params struct {
		Name      string `json:"name"`
		Arguments struct {
			Target   string   `json:"target"`
			Packages []string `json:"packages"`
		} `json:"arguments"`
	}
	if json.Unmarshal(request.Params, &params) != nil {
		return rpcError(request.ID, -32602, "invalid params")
	}
	var tool *Tool
	for i := range spec.Tools {
		if spec.Tools[i].Name == params.Name {
			tool = &spec.Tools[i]
		}
	}
	if tool == nil {
		return toolText(request.ID, "unknown tool", true)
	}
	declared := false
	for _, target := range tool.Targets {
		declared = declared || target == params.Arguments.Target
	}
	if !declared {
		return toolText(request.ID, "target is not declared for this tool", true)
	}
	if len(params.Arguments.Packages) > 16 || (tool.Kind != "npm-lock-update" && len(params.Arguments.Packages) > 0) {
		return toolText(request.ID, "packages are not accepted here", true)
	}
	for _, p := range params.Arguments.Packages {
		if !packageRe.MatchString(p) {
			return toolText(request.ID, "packages must be npm package names without versions", true)
		}
	}
	diff, err := candidate(ctx)
	if err != nil {
		return toolText(request.ID, "cannot prepare the candidate change: "+err.Error(), true)
	}
	payload := map[string]any{"tool": tool.Name, "target": params.Arguments.Target, "diff": diff}
	if len(params.Arguments.Packages) > 0 {
		payload["packages"] = params.Arguments.Packages
	}
	body, _ := json.Marshal(payload)
	if len(body) > MaxRequestBytes {
		// The diff bound is on its bytes; JSON escaping can push the
		// request past the mailbox's own bound.
		return toolText(request.ID, fmt.Sprintf("the candidate change is too large to send: %d bytes encoded, at most %d", len(body), MaxRequestBytes), true)
	}
	status, answer, err := relay(ctx, body)
	if err != nil {
		return toolText(request.ID, "tool run unavailable", true)
	}
	var reply struct {
		Text  string `json:"text"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(answer, &reply) != nil || reply.Text == "" {
		reply.Text = mailboxRefusal(status, reply.Error.Message)
	}
	return toolText(request.ID, reply.Text, status != 200)
}

// mailboxRefusal explains a refusal the mailbox itself answered, before any
// run: the generator may call tools one at a time and up to its budget.
func mailboxRefusal(status int, code string) string {
	switch code {
	case "tool_call_already_pending":
		return "Another tool call is still running. Tools run one at a time: wait for its result, then call again if needed."
	case "local_tool_limit":
		return "The tool call budget of this generation is used up; finish without further tool calls."
	case "tool_deadline":
		return "The tool run did not answer within its window."
	}
	return fmt.Sprintf("tool run returned status %d", status)
}

func hasDotSegment(path string) bool {
	for _, part := range strings.Split(path, "/") {
		if part == "." || part == ".." {
			return true
		}
	}
	return false
}
