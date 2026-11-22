package reviewresult

import (
	"encoding/json"
	"strings"
	"testing"
)

func validSummary() map[string]any {
	return map[string]any{"v": 1, "success": false, "model": "fixture-model", "exit_reason": "no_changes", "iterations": 2, "tool_calls_total": 1, "tool_calls": map[string]int{"read_file": 1}, "phases": []any{map[string]any{"role": "reviewer", "provider": "custom", "model": "fixture-model", "iterations": 2}}, "review": map[string]any{"verdict": "approve", "explicit": true, "fix_rounds": 0, "max_fixes": 0}, "final_message": "Inspected the implementation.\nVERDICT: approve", "error": nil}
}
func TestSummaryRequiresExplicitBoundedReadonlyReview(t *testing.T) {
	raw, _ := json.Marshal(validSummary())
	result, err := Parse(raw, "fixture-model", 4)
	if err != nil || result.Verdict != "approve" || result.ReadCalls != 1 {
		t.Fatal(result, err)
	}
	for name, mutate := range map[string]func(map[string]any){
		"no success":          func(m map[string]any) { delete(m, "success") },
		"no inspection":       func(m map[string]any) { m["tool_calls_total"] = 0; m["tool_calls"] = map[string]int{} },
		"mutating tool":       func(m map[string]any) { m["tool_calls"] = map[string]int{"write_file": 1} },
		"shell":               func(m map[string]any) { m["tool_calls"] = map[string]int{"run_shell": 1} },
		"different model":     func(m map[string]any) { m["model"] = "other" },
		"fallback":            func(m map[string]any) { m["role_fallback"] = []string{"reviewer"} },
		"multiple phases":     func(m map[string]any) { m["phases"] = append(m["phases"].([]any), m["phases"].([]any)[0]) },
		"wrong role":          func(m map[string]any) { m["phases"].([]any)[0].(map[string]any)["role"] = "executor" },
		"implicit":            func(m map[string]any) { m["review"].(map[string]any)["explicit"] = false },
		"rework":              func(m map[string]any) { m["review"].(map[string]any)["fix_rounds"] = 1 },
		"enabled rework":      func(m map[string]any) { m["review"].(map[string]any)["max_fixes"] = 3 },
		"missing rework":      func(m map[string]any) { delete(m["review"].(map[string]any), "max_fixes") },
		"disagreeing verdict": func(m map[string]any) { m["review"].(map[string]any)["verdict"] = "request_changes" },
		"error":               func(m map[string]any) { m["error"] = "provider unavailable" },
		"budget":              func(m map[string]any) { m["iterations"] = 9 },
		"count overflow":      func(m map[string]any) { m["tool_calls"] = map[string]int{"read_file": int(^uint(0) >> 1)} },
	} {
		t.Run(name, func(t *testing.T) {
			m := validSummary()
			mutate(m)
			raw, _ := json.Marshal(m)
			if _, err := Parse(raw, "fixture-model", 4); err == nil {
				t.Fatal("invalid summary accepted")
			}
		})
	}
	if _, err := Parse(append([]byte(`{"success":false,`), raw[1:]...), "fixture-model", 4); err == nil {
		t.Fatal("duplicate accepted")
	}
}
func TestVerdictRejectsFallbackAmbiguityAndTruncation(t *testing.T) {
	for _, good := range []string{"approve", "request_changes"} {
		if v, e := Verdict("Observed evidence.\nVERDICT: " + good); e != nil || v != good {
			t.Fatal(v, e)
		}
	}
	for _, good := range []string{
		"The parser now trims keys:\n```go\nk = strings.TrimSpace(k)\n```\nTests cover it.\nVERDICT: approve",
		"Two blocks:\n~~~\na\n~~~\n```\nb\n```\nVERDICT: request_changes",
	} {
		if _, e := Verdict(good); e != nil {
			t.Fatal("balanced fenced review refused")
		}
	}
	for _, bad := range []string{
		"```\nVERDICT: approve",
		"```go\ncode\nVERDICT: approve",
		"Use ```inline``` here.\nVERDICT: approve",
		"```\na\n~~~\nVERDICT: approve",
		"```\na\n```go\nVERDICT: approve",
		"```\nverdict: x\n```\nVERDICT: approve",
	} {
		if _, e := Verdict(bad); e == nil {
			t.Fatalf("fence ambiguity accepted: %q", bad)
		}
	}
	for _, bad := range []string{"Looks good", "VERDICT: APPROVE", "VERDICT: approve\nmore", "> VERDICT: approve", "```\nVERDICT: approve\n```", "VERDICT: request_changes\nVERDICT: approve", "verdict: request_changes\nVERDICT: approve", strings.Repeat("a", 4000) + "\nVERDICT: approve", "\x00\nVERDICT: approve"} {
		if _, e := Verdict(bad); e == nil {
			t.Fatal("ambiguous verdict accepted")
		}
	}
}
