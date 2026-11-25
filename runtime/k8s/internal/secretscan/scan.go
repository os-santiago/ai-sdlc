// Package secretscan refuses candidate deltas that add a high-confidence
// credential. It is deterministic and offline: a fixed, reviewed rule set
// runs over the lines a candidate adds, never over the base revision, the
// network or a model.
//
// A finding names only the rule and a bounded class of the file's path. The
// matched text, its line and the path itself are never returned, so no
// caller can log, store or publish the secret.
package secretscan

import (
	"bytes"
	"path"
	"regexp"
	"strings"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/candidate"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

// Version names the rule set below. Any change to a rule, its filters or what
// counts as an added line must change it: the repository-tests recipe
// revision binds it, so a command signed under other rules no longer verifies
// and no proof from them can be published.
const Version = "secret-scan/v2"

// Rule identifiers, the only rule names a finding can carry.
const (
	RulePrivateKey         = "private-key"
	RuleAWSAccessKeyID     = "aws-access-key-id"
	RuleAWSSecretAccessKey = "aws-secret-access-key"
	RuleGitHubToken        = "github-token"
	RuleGitLabToken        = "gitlab-token"
	RuleGoogleAPIKey       = "google-api-key"
	RuleAzureStorageKey    = "azure-storage-key"
)

// Path classes, the only description of a file a finding can carry.
const (
	ClassCode   = "code"
	ClassConfig = "config"
	ClassDocs   = "docs"
	ClassKey    = "key"
	ClassOther  = "other"
)

// Finding is a refusal reason that is safe to log: a rule ID and a path class.
type Finding struct {
	Rule      string
	PathClass string
}

type rule struct {
	id string
	// pattern's first submatch is the candidate secret value.
	pattern *regexp.Regexp
	// accept filters out documented examples and placeholders.
	accept func(value string) bool
}

// The patterns are anchored on vendor prefixes or exact formats. The left
// boundary keeps a prefix inside a longer identifier from matching, and fixed
// lengths with a right boundary keep longer random strings from matching.
var rules = []rule{
	{RuleAWSAccessKeyID, regexp.MustCompile(`(?:^|[^A-Za-z0-9])((?:AKIA|ASIA)[A-Z2-7]{16})(?:[^A-Za-z0-9]|$)`), notExample},
	{RuleAWSSecretAccessKey, regexp.MustCompile(`(?i:aws_?secret_?access_?key)["']?\s*(?::=|=>|[:=])\s*["']?([A-Za-z0-9/+]{40})(?:[^A-Za-z0-9/+=]|$)`), func(v string) bool { return notExample(v) && random(v) }},
	{RuleGitHubToken, regexp.MustCompile(`(?:^|[^A-Za-z0-9_])((?:gh[pousr]_[A-Za-z0-9]{36})|(?:github_pat_[A-Za-z0-9]{22}_[A-Za-z0-9]{59}))(?:[^A-Za-z0-9_]|$)`), random},
	{RuleGitLabToken, regexp.MustCompile(`(?:^|[^A-Za-z0-9_-])(glpat-[A-Za-z0-9_-]{20,64})(?:[^A-Za-z0-9_-]|$)`), random},
	{RuleGoogleAPIKey, regexp.MustCompile(`(?:^|[^A-Za-z0-9_-])(AIza[A-Za-z0-9_-]{35})(?:[^A-Za-z0-9_-]|$)`), random},
	{RuleAzureStorageKey, regexp.MustCompile(`(?:^|[^A-Za-z0-9])AccountKey=([A-Za-z0-9+/]{86}==)`), random},
}

// matches reports whether an accepted value of the rule occurs in line. The
// right boundary consumes the character after a value, so a scan that
// resumed after the whole match would miss a value that starts right after a
// rejected one (a placeholder or documented example followed by a real
// token). Each search therefore resumes at the end of the value itself; the
// boundary character there satisfies the next value's left boundary, and
// the anchor ^ cannot match inside a token because that character is never
// part of one.
func (r rule) matches(line string) bool {
	for pos := 0; pos < len(line); {
		m := r.pattern.FindStringSubmatchIndex(line[pos:])
		if m == nil {
			return false
		}
		if r.accept(line[pos+m[2] : pos+m[3]]) {
			return true
		}
		pos += m[3]
	}
	return false
}

// A private key needs its header and, on the same line after an escaped
// newline or within the next lines, a base64 body: code that only names the
// header (a PEM parser, a log message) does not match.
var (
	keyHeader     = regexp.MustCompile(`-----BEGIN (?:[A-Z0-9]+ ){0,3}PRIVATE KEY(?: BLOCK)?-----`)
	keyInlineBody = regexp.MustCompile(`-----BEGIN (?:[A-Z0-9]+ ){0,3}PRIVATE KEY(?: BLOCK)?-----(?:\\r)?(?:\\n){1,2}([A-Za-z0-9+/]{40,})`)
	// A body line may continue a string concatenation (`+ "MIIE..."`).
	keyBody  = regexp.MustCompile(`^[\s"'` + "`" + `]*(?:\+[\s"'` + "`" + `]*)?([A-Za-z0-9+/]{40,})`)
	keyArmor = regexp.MustCompile(`^[\s"'` + "`" + `]*(?:\+[\s"'` + "`" + `]*)?(?:Proc-Type|DEK-Info|Version|Comment):`)
)

// keyLookahead bounds the lines read after a private key header before its
// first body line; keyBlockLines bounds the body lines of one block (a 16 KiB
// RSA key in 64-character lines has about 300).
const (
	keyLookahead  = 4
	keyBlockLines = 512
)

// keyContinuation is a body line after the first: base64, possibly short
// (the last line) and possibly inside a quoted, concatenated string.
var keyContinuation = regexp.MustCompile(`^[\s"'` + "`" + `]*(?:\+[\s"'` + "`" + `]*)?[A-Za-z0-9+/]{4,}={0,2}`)

// Rules lists every rule ID a finding can carry, in evaluation order.
func Rules() []string {
	ids := []string{RulePrivateKey}
	for _, r := range rules {
		ids = append(ids, r.id)
	}
	return ids
}

// KnownRule reports whether id is a rule ID of this version.
func KnownRule(id string) bool {
	for _, known := range Rules() {
		if id == known {
			return true
		}
	}
	return false
}

// KnownClass reports whether class is a path class of this version.
func KnownClass(class string) bool {
	switch class {
	case ClassCode, ClassConfig, ClassDocs, ClassKey, ClassOther:
		return true
	}
	return false
}

// Scan reports the first finding, in delta path order, among the lines the
// candidate adds. A line counts as added when the file's base content has no
// identical line, so moving or keeping existing text never refuses a
// candidate. Deleted files add nothing. The delta must already be decoded and
// apply to source; its size is bounded by candidate.MaxBytes.
func Scan(source workspacebundle.Bundle, d candidate.Delta) (Finding, bool) {
	base := make(map[string][]byte, len(d.Changes))
	for _, c := range d.Changes {
		if c.Before != nil {
			base[c.Path] = nil
		}
	}
	for _, f := range source.Files {
		if _, changed := base[f.Path]; changed {
			base[f.Path] = f.Data
		}
	}
	for _, c := range d.Changes {
		if c.After == nil {
			continue
		}
		if id, ok := scanFile(base[c.Path], c.After.Data); ok {
			return Finding{Rule: id, PathClass: PathClass(c.Path)}, true
		}
	}
	return Finding{}, false
}

// scanFile returns the first rule an added line of after matches.
func scanFile(before, after []byte) (string, bool) {
	existing := map[string]struct{}{}
	for _, line := range splitLines(before) {
		existing[line] = struct{}{}
	}
	lines := splitLines(after)
	added := func(line string) bool { _, kept := existing[line]; return !kept }
	for i, line := range lines {
		// Every key shares its header line, so a new key under an existing
		// header is found by its new body line.
		if body, ok := privateKey(lines, i); ok {
			// A block is new when its header or any of its body lines is:
			// a replaced key often keeps the header and the first body line
			// (the key type and its encoding prefix). Only a block whose
			// every line is unchanged is existing text.
			changed := added(line)
			for _, j := range body {
				changed = changed || added(lines[j])
			}
			if changed {
				return RulePrivateKey, true
			}
		}
		if !added(line) {
			continue
		}
		for _, r := range rules {
			if r.matches(line) {
				return r.id, true
			}
		}
	}
	return "", false
}

func splitLines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	parts := bytes.Split(data, []byte("\n"))
	lines := make([]string, len(parts))
	for i, part := range parts {
		lines[i] = string(bytes.TrimSuffix(part, []byte("\r")))
	}
	return lines
}

// privateKey reports whether lines[i] starts a private key, and the indexes
// of every line of its body: i itself for an inline body (the whole key is on
// that line), otherwise the first body line and each following base64 line
// up to the END line, a non-base64 line or keyBlockLines.
func privateKey(lines []string, i int) ([]int, bool) {
	if !keyHeader.MatchString(lines[i]) {
		return nil, false
	}
	for _, m := range keyInlineBody.FindAllStringSubmatch(lines[i], -1) {
		if keyMaterial(m[1]) {
			return []int{i}, true
		}
	}
	for j := i + 1; j < len(lines) && j <= i+keyLookahead; j++ {
		line := lines[j]
		if strings.TrimSpace(line) == "" || keyArmor.MatchString(line) {
			continue
		}
		m := keyBody.FindStringSubmatch(line)
		if m == nil || !keyMaterial(m[1]) {
			return nil, false
		}
		body := []int{j}
		for k := j + 1; k < len(lines) && len(body) < keyBlockLines; k++ {
			if strings.Contains(lines[k], "-----END") || !keyContinuation.MatchString(lines[k]) {
				break
			}
			body = append(body, k)
		}
		return body, true
	}
	return nil, false
}

// keyMaterial requires the mix of cases and digits that encoded key bytes
// have, so a long word or identifier after a header does not count.
func keyMaterial(v string) bool {
	return strings.ContainsAny(v, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") && strings.ContainsAny(v, "abcdefghijklmnopqrstuvwxyz") &&
		strings.ContainsAny(v, "0123456789")
}

// notExample refuses the values vendors publish in their documentation, which
// end with EXAMPLE (for example AKIAIOSFODNN7EXAMPLE).
func notExample(v string) bool {
	return !strings.HasSuffix(v, "EXAMPLE") && !strings.HasSuffix(v, "EXAMPLEKEY")
}

// minDistinct is the fewest distinct characters a token body must have. A
// random 20-character base62 body has about 17; placeholders such as
// ghp_xxxx... or AIzaAAAA... have one or a few.
const minDistinct = 10

// random reports whether the token body after its vendor prefix looks
// random rather than a placeholder.
func random(v string) bool {
	body := v
	for _, prefix := range []string{"github_pat_", "ghp_", "gho_", "ghu_", "ghs_", "ghr_", "glpat-", "AIza"} {
		if strings.HasPrefix(body, prefix) {
			body = body[len(prefix):]
			break
		}
	}
	seen := map[rune]bool{}
	for _, r := range body {
		seen[r] = true
	}
	return len(seen) >= minDistinct
}

// PathClass maps a repository path to a bounded class, so a finding can say
// where a secret was without naming the file.
func PathClass(file string) string {
	name := strings.ToLower(path.Base(file))
	if name == ".env" || strings.HasPrefix(name, ".env.") || name == ".npmrc" || name == ".netrc" || name == ".pypirc" {
		return ClassConfig
	}
	if strings.HasPrefix(name, "id_rsa") || strings.HasPrefix(name, "id_ed25519") || strings.HasPrefix(name, "id_ecdsa") {
		return ClassKey
	}
	switch path.Ext(name) {
	case ".pem", ".key", ".p8", ".p12", ".pfx", ".jks", ".keystore", ".gpg", ".asc":
		return ClassKey
	case ".json", ".yaml", ".yml", ".toml", ".ini", ".cfg", ".conf", ".properties", ".xml", ".env", ".tfvars", ".hcl":
		return ClassConfig
	case ".md", ".markdown", ".txt", ".rst", ".adoc", ".html":
		return ClassDocs
	case ".go", ".java", ".kt", ".kts", ".scala", ".groovy", ".js", ".mjs", ".cjs", ".jsx", ".ts", ".tsx", ".py", ".rb", ".php",
		".rs", ".c", ".h", ".cc", ".cpp", ".hpp", ".cs", ".swift", ".m", ".sh", ".bash", ".ps1", ".sql", ".tf", ".vue", ".svelte":
		return ClassCode
	}
	return ClassOther
}
