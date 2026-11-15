// Package reviewresult validates untrusted SCC reviewer output. A valid result
// is advisory: only a separate controller with durable authority and independently
// bound model-call evidence may authorize publication.
package reviewresult

import (
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
)

const MaxBytes = 64 * 1024

var ErrInvalid = errors.New("review summary rejected")

type Result struct {
	Verdict      string
	FinalMessage string
	Iterations   int
	ReadCalls    int
}

// Parse requires a single completed reviewer phase on the expected model, no
// rework or fallback, only read tools, and one unambiguous terminal verdict.
// Summary bytes can never substitute for trusted model response evidence.
func Parse(raw []byte, model string, maxSteps int) (Result, error) {
	if len(raw) == 0 || len(raw) > MaxBytes || !utf8.Valid(raw) || model == "" || maxSteps < 1 || maxSteps > 8 {
		return Result{}, ErrInvalid
	}
	var fields map[string]json.RawMessage
	if authority.DecodeStrict(raw, &fields) != nil || fields == nil {
		return Result{}, ErrInvalid
	}
	// SCC includes telemetry fields that do not grant authority. Parse only the
	// exact required names; duplicate/case-ambiguous keys were rejected above.
	get := func(name string, out any) bool {
		v, ok := fields[name]
		return ok && string(v) != "null" && authority.DecodeStrict(v, out) == nil
	}
	var version, steps, calls int
	var success bool
	var servedModel, reason, message string
	var review struct {
		Verdict   string `json:"verdict"`
		Explicit  *bool  `json:"explicit"`
		FixRounds *int   `json:"fix_rounds"`
		MaxFixes  *int   `json:"max_fixes"`
	}
	var phases []struct {
		Role       string `json:"role"`
		Provider   string `json:"provider"`
		Model      string `json:"model"`
		Iterations int    `json:"iterations"`
	}
	var counts map[string]int
	if !get("v", &version) || version != 1 || !get("success", &success) || success || !get("model", &servedModel) || servedModel != model || !get("exit_reason", &reason) || reason != "no_changes" || !get("iterations", &steps) || steps < 1 || steps > maxSteps || !get("tool_calls_total", &calls) || calls < 1 || calls > 32 || !get("tool_calls", &counts) || counts == nil || !get("review", &review) || review.Explicit == nil || !*review.Explicit || review.FixRounds == nil || *review.FixRounds != 0 || review.MaxFixes == nil || *review.MaxFixes != 0 || !get("phases", &phases) || len(phases) != 1 || phases[0].Role != "reviewer" || phases[0].Model != model || phases[0].Provider != "custom" || phases[0].Iterations != steps || !get("final_message", &message) || len(message) == 0 || len(message) >= 4000 {
		return Result{}, ErrInvalid
	}
	if v, ok := fields["error"]; !ok || string(v) != "null" {
		return Result{}, ErrInvalid
	}
	if _, ok := fields["role_fallback"]; ok {
		var fallbacks []string
		if !get("role_fallback", &fallbacks) || len(fallbacks) != 0 {
			return Result{}, ErrInvalid
		}
	}
	total := 0
	for tool, count := range counts {
		switch tool {
		case "read_file", "list_dir", "search_text", "code_query":
		default:
			return Result{}, ErrInvalid
		}
		if count < 1 || count > 32 {
			return Result{}, ErrInvalid
		}
		total += count
	}
	if total != calls {
		return Result{}, ErrInvalid
	}
	verdict, err := Verdict(message)
	if err != nil || verdict != review.Verdict {
		return Result{}, ErrInvalid
	}
	return Result{verdict, message, steps, calls}, nil
}

// Verdict intentionally rejects SCC's prose/last-marker fallback, quoted
// markers, multiple verdicts, code fences and truncated final messages.
func Verdict(message string) (string, error) {
	if len(message) == 0 || len(message) >= 4000 || !utf8.ValidString(message) || strings.ContainsAny(message, "\x00\r") || strings.Contains(message, "```") || strings.Contains(message, "~~~") || strings.Count(strings.ToLower(message), "verdict:") != 1 {
		return "", ErrInvalid
	}
	lines := strings.Split(strings.TrimSpace(message), "\n")
	last := lines[len(lines)-1]
	for _, verdict := range []string{"approve", "request_changes"} {
		if last == "VERDICT: "+verdict {
			return verdict, nil
		}
	}
	return "", ErrInvalid
}
