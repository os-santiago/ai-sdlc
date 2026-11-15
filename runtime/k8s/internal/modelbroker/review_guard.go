package modelbroker

import (
	"encoding/json"
	"path"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/reviewresult"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

const reviewObservationVersion = "ai-sdlc.review-observation/v1"

var reviewToolID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// ReviewObservation is derived from the exact provider response BEFORE mailbox
// delivery. It retains no source, arguments or model prose. SCC cannot supply it.
type ReviewObservation struct {
	Version     string         `json:"version"`
	Tools       map[string]int `json:"tools"`
	Verdict     string         `json:"verdict"`
	FinalSHA256 string         `json:"finalSHA256"`
}

func reviewPath(value string, glob, root bool) bool {
	if value == "." {
		return root
	}
	if value == "" || len(value) > 512 || !utf8.ValidString(value) || path.IsAbs(value) || strings.HasPrefix(value, "-") || strings.ContainsAny(value, "\\~:!{}()") || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." || part == ".git" {
			return false
		}
	}
	return glob || !strings.ContainsAny(value, "*?[]")
}
func reviewArguments(name, arguments string) bool {
	if len(arguments) == 0 || len(arguments) > 2048 || !utf8.ValidString(arguments) {
		return false
	}
	var fields map[string]json.RawMessage
	if authority.DecodeStrict([]byte(arguments), &fields) != nil || fields == nil {
		return false
	}
	stringField := func(key string) string {
		var v string
		if authority.DecodeStrict(fields[key], &v) != nil {
			return ""
		}
		return v
	}
	switch name {
	case "read_file", "code_query", "list_dir":
		p := stringField("path")
		if name == "list_dir" && fields["path"] == nil {
			p = "."
		}
		if !reviewPath(p, false, name == "list_dir") {
			return false
		}
		for key, v := range fields {
			if key == "path" {
				continue
			}
			if name != "read_file" || (key != "offset" && key != "limit") {
				return false
			}
			var n int
			if authority.DecodeStrict(v, &n) != nil || n < 1 || n > 100000 {
				return false
			}
		}
	case "search_text":
		pattern := stringField("pattern")
		if pattern == "" || len(pattern) > 512 || strings.IndexFunc(pattern, unicode.IsControl) >= 0 {
			return false
		}
		// Regex execution is unnecessary for this bounded recipe and can consume
		// unbounded CPU in SCC's JS engine. Literal search only.
		if raw, ok := fields["regex"]; ok {
			var value bool
			if authority.DecodeStrict(raw, &value) != nil || value || string(raw) == "null" {
				return false
			}
		}
		_, hasPath := fields["path"]
		_, hasGlob := fields["glob"]
		if hasPath == hasGlob {
			return false
		}
		key := "path"
		if hasGlob {
			key = "glob"
		}
		if !reviewPath(stringField(key), true, false) {
			return false
		}
		for key := range fields {
			if key != "pattern" && key != "path" && key != "glob" && key != "regex" {
				return false
			}
		}
	default:
		return false
	}
	return true
}

// ObserveReviewResponse accepts one complete assistant choice, either bounded
// read-only tools OR an explicit final verdict. Unknown message/choice fields,
// legacy function calls, ambiguous JSON and truncated completions fail closed.
func ObserveReviewResponse(raw []byte, model string) (ReviewObservation, error) {
	invalid := func() (ReviewObservation, error) { return ReviewObservation{}, ErrRejected }
	if len(raw) < 1 || len(raw) > 32768 || !utf8.Valid(raw) || model == "" {
		return invalid()
	}
	var fields map[string]json.RawMessage
	if authority.DecodeStrict(raw, &fields) != nil || fields == nil {
		return invalid()
	}
	var served string
	if authority.DecodeStrict(fields["model"], &served) != nil || served != model {
		return invalid()
	}
	var choices []struct {
		Index   *int   `json:"index"`
		Finish  string `json:"finish_reason"`
		Message struct {
			Role    string  `json:"role"`
			Content *string `json:"content"`
			Tools   []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
	}
	if authority.DecodeStrict(fields["choices"], &choices) != nil || len(choices) != 1 || choices[0].Index == nil || *choices[0].Index != 0 || choices[0].Message.Role != "assistant" {
		return invalid()
	}
	c := choices[0]
	o := ReviewObservation{Version: reviewObservationVersion, Tools: map[string]int{}}
	if len(c.Message.Tools) > 0 {
		if c.Finish != "tool_calls" || len(c.Message.Tools) > 4 {
			return invalid()
		}
		// Standard chat completions may include a short explanation alongside
		// read-only tools. It is not a verdict and confers no authority.
		if c.Message.Content != nil {
			text := *c.Message.Content
			if len(text) >= 4000 || strings.ContainsAny(text, "\x00\r") || strings.Contains(strings.ToLower(text), "verdict:") {
				return invalid()
			}
		}
		seen := map[string]bool{}
		for _, tool := range c.Message.Tools {
			if !reviewToolID.MatchString(tool.ID) || seen[tool.ID] || tool.Type != "function" || !reviewArguments(tool.Function.Name, tool.Function.Arguments) {
				return invalid()
			}
			seen[tool.ID] = true
			o.Tools[tool.Function.Name]++
		}
	} else {
		if c.Finish != "stop" || c.Message.Content == nil {
			return invalid()
		}
		verdict, err := reviewresult.Verdict(*c.Message.Content)
		if err != nil {
			return invalid()
		}
		o.Verdict = verdict
		o.FinalSHA256 = authority.Digest([]byte(*c.Message.Content))
	}
	return o, nil
}

// MatchReviewSummary compares untrusted SCC output to independently committed
// provider observations, read using the active review claim. This is a binding
// check, not publication authority or proof of semantic review quality.
func MatchReviewSummary(summary reviewresult.Result, evidence []store.ReviewModelEvidence) error {
	if len(evidence) < 2 || len(evidence) > 8 || summary.Iterations != len(evidence) {
		return ErrRejected
	}
	total := 0
	for i, item := range evidence {
		var o ReviewObservation
		if item.Sequence != i+1 || item.InputBytes < 1 || item.OutputBytes < 1 || !reviewDigest(item.RequestDigest) || !reviewDigest(item.ResponseDigest) || authority.DecodeStrict(item.Observation, &o) != nil || o.Version != reviewObservationVersion || o.Tools == nil {
			return ErrRejected
		}
		if i == len(evidence)-1 {
			if len(o.Tools) != 0 || o.Verdict != summary.Verdict || o.FinalSHA256 != authority.Digest([]byte(summary.FinalMessage)) {
				return ErrRejected
			}
		} else {
			if o.Verdict != "" || o.FinalSHA256 != "" || len(o.Tools) == 0 {
				return ErrRejected
			}
			count := 0
			for name, n := range o.Tools {
				if name != "read_file" && name != "list_dir" && name != "search_text" && name != "code_query" {
					return ErrRejected
				}
				if n < 1 || n > 4 {
					return ErrRejected
				}
				count += n
			}
			if count > 4 {
				return ErrRejected
			}
			total += count
		}
	}
	verdict, err := reviewresult.Verdict(summary.FinalMessage)
	if err != nil || verdict != summary.Verdict || total != summary.ReadCalls {
		return ErrRejected
	}
	return nil
}
func reviewDigest(v string) bool {
	if len(v) != 64 {
		return false
	}
	for _, c := range v {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
