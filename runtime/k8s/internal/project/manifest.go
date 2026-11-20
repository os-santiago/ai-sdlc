package project

import (
	"embed"
	"encoding/json"
	"errors"
	"path"
	"regexp"
	"strings"
	"unicode"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
)

// ManifestVersion identifies the reviewed per-repository delivery declaration.
// A manifest is installation configuration: it is never read from the target
// repository, an issue or model output, and it grants no authority by itself.
const ManifestVersion = "ai-sdlc.project/v1"

// MaxManifestBytes bounds an installed manifest.
const MaxManifestBytes = 16 * 1024

// Manifest declares how the runtime delivers changes for one repository: how
// candidates are verified, which paths a publication may change, and the merge
// policy. Reference repositories have built-in manifests that express exactly
// their existing recipes, allowlists and merge policies.
type Manifest struct {
	Version      string        `json:"version"`
	Repository   string        `json:"repository"`
	BaseBranch   string        `json:"baseBranch"`
	Verification *Verification `json:"verification,omitempty"`
	Publication  Publication   `json:"publication"`
	Merge        Merge         `json:"merge"`
	Recipes      []Recipe      `json:"recipes,omitempty"`
}

// Verification runs the repository's own tests in the sandboxed verifier.
type Verification struct {
	Image            string   `json:"image"`
	Command          []string `json:"command"`
	WorkingDir       string   `json:"workingDir,omitempty"`
	TimeoutSeconds   int      `json:"timeoutSeconds"`
	CPU              string   `json:"cpu"`
	Memory           string   `json:"memory"`
	EphemeralStorage string   `json:"ephemeralStorage"`
	Network          string   `json:"network"`
	Result           Result   `json:"result"`
	Repeat           int      `json:"repeat"`
	TestGlobs        []string `json:"testGlobs"`
}

type Result struct {
	ExitCode  bool   `json:"exitCode"`
	JUnitPath string `json:"junitPath,omitempty"`
}

// Publication bounds the files a candidate may change. Deny wins; a path no
// allow pattern matches is denied; repository automation is always denied.
type Publication struct {
	Allow           []string `json:"allow"`
	Deny            []string `json:"deny,omitempty"`
	MaxChangedFiles int      `json:"maxChangedFiles"`
	MaxChangedBytes int      `json:"maxChangedBytes"`
	MaxFileBytes    int      `json:"maxFileBytes"`
}

type Merge struct {
	RulesetID      int64   `json:"rulesetID"`
	Required       []Check `json:"required"`
	StrictChecks   bool    `json:"strictChecks"`
	ResolveThreads bool    `json:"resolveThreads"`
	HumanApprovals int     `json:"humanApprovals"`
}

type Check struct {
	Context string `json:"context"`
	AppID   int64  `json:"appID"`
}

// Recipe names a reviewed verification plug-in, such as a per-issue oracle.
type Recipe struct {
	Name   string `json:"name"`
	Marker string `json:"marker"`
}

var (
	ErrManifest       = errors.New("invalid project manifest")
	repositoryName    = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}/[A-Za-z0-9_.-]{1,100}$`)
	branchName        = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}(/[A-Za-z0-9._-]{1,100}){0,4}$`)
	pinnedImage       = regexp.MustCompile(`^[a-z0-9.-]+(:[0-9]{1,5})?(/[a-z0-9._-]+)+@sha256:[a-f0-9]{64}$`)
	cpuQuantity       = regexp.MustCompile(`^([1-9][0-9]{0,4}m|[1-8])$`)
	byteQuantity      = regexp.MustCompile(`^[1-9][0-9]{0,5}(Mi|Gi)$`)
	recipeName        = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*/v[1-9][0-9]{0,2}$`)
	alwaysDenied      = []string{".github/**"}
	maxPublishedBytes = 4 * 1024 * 1024
)

// ParseManifest decodes an installed manifest strictly (unknown fields and
// duplicate keys are refused) and validates it.
func ParseManifest(raw []byte) (Manifest, error) {
	var m Manifest
	if len(raw) == 0 || len(raw) > MaxManifestBytes || authority.DecodeStrict(raw, &m) != nil {
		return Manifest{}, ErrManifest
	}
	if m.Validate() != nil {
		return Manifest{}, ErrManifest
	}
	return m, nil
}

// Digest is the canonical sha256 of the manifest, suitable for binding it into
// signed installation policy.
func (m Manifest) Digest() string {
	raw, _ := json.Marshal(m)
	return authority.Digest(raw)
}

func (m Manifest) Validate() error {
	if m.Version != ManifestVersion || !validRepository(m.Repository) || !branchName.MatchString(m.BaseBranch) ||
		strings.Contains(m.BaseBranch, "..") {
		return ErrManifest
	}
	if m.Verification == nil && len(m.Recipes) == 0 {
		return ErrManifest
	}
	if m.Verification != nil && m.Verification.validate() != nil {
		return ErrManifest
	}
	if m.Publication.validate() != nil || m.Merge.validate() != nil || len(m.Recipes) > 8 {
		return ErrManifest
	}
	seen := map[string]bool{}
	for _, r := range m.Recipes {
		if !recipeName.MatchString(r.Name) || (r.Marker != "required" && r.Marker != "none") || seen[r.Name] {
			return ErrManifest
		}
		seen[r.Name] = true
	}
	return nil
}

func validRepository(repository string) bool {
	if !repositoryName.MatchString(repository) {
		return false
	}
	for _, part := range strings.Split(repository, "/") {
		if part == "." || part == ".." {
			return false
		}
	}
	return true
}

func (v Verification) validate() error {
	if !pinnedImage.MatchString(v.Image) || len(v.Command) == 0 || len(v.Command) > 32 ||
		v.TimeoutSeconds < 30 || v.TimeoutSeconds > 600 || !cpuQuantity.MatchString(v.CPU) ||
		!byteQuantity.MatchString(v.Memory) || !byteQuantity.MatchString(v.EphemeralStorage) ||
		v.Network != "none" || !v.Result.ExitCode || v.Repeat < 1 || v.Repeat > 3 ||
		len(v.TestGlobs) == 0 || len(v.TestGlobs) > 32 {
		return ErrManifest
	}
	for _, arg := range v.Command {
		if arg == "" || len(arg) > 1024 || strings.IndexFunc(arg, unicode.IsControl) >= 0 {
			return ErrManifest
		}
	}
	if v.WorkingDir != "" && cleanPath(v.WorkingDir) != nil {
		return ErrManifest
	}
	if v.Result.JUnitPath != "" && cleanPath(v.Result.JUnitPath) != nil {
		return ErrManifest
	}
	for _, g := range v.TestGlobs {
		if validGlob(g) != nil {
			return ErrManifest
		}
	}
	return nil
}

func (p Publication) validate() error {
	if len(p.Allow) == 0 || len(p.Allow) > 64 || len(p.Deny) > 64 ||
		p.MaxChangedFiles < 1 || p.MaxChangedFiles > 200 ||
		p.MaxChangedBytes < 1 || p.MaxChangedBytes > maxPublishedBytes ||
		p.MaxFileBytes < 1 || p.MaxFileBytes > p.MaxChangedBytes {
		return ErrManifest
	}
	for _, g := range append(append([]string{}, p.Allow...), p.Deny...) {
		if validGlob(g) != nil {
			return ErrManifest
		}
	}
	return nil
}

func (m Merge) validate() error {
	if m.RulesetID < 1 || len(m.Required) == 0 || len(m.Required) > 16 || m.HumanApprovals < 0 || m.HumanApprovals > 6 {
		return ErrManifest
	}
	seen := map[string]bool{}
	for _, c := range m.Required {
		if c.Context == "" || len(c.Context) > 128 || strings.IndexFunc(c.Context, unicode.IsControl) >= 0 ||
			c.AppID < 1 || seen[c.Context] {
			return ErrManifest
		}
		seen[c.Context] = true
	}
	return nil
}

// Allows reports whether a publication may change the repository path.
// Paths must be clean, relative and slash-separated; deny patterns (including
// the always-denied repository automation) win over allow patterns.
func (p Publication) Allows(file string) bool {
	if cleanPath(file) != nil {
		return false
	}
	for _, g := range append(append([]string{}, alwaysDenied...), p.Deny...) {
		if matchGlob(g, file) {
			return false
		}
	}
	for _, g := range p.Allow {
		if matchGlob(g, file) {
			return true
		}
	}
	return false
}

// MatchesTest reports whether a path is one of the repository's test files.
func (v Verification) MatchesTest(file string) bool {
	if cleanPath(file) != nil {
		return false
	}
	for _, g := range v.TestGlobs {
		if matchGlob(g, file) {
			return true
		}
	}
	return false
}

func cleanPath(p string) error {
	if p == "" || len(p) > 1024 || strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") ||
		strings.ContainsAny(p, "\\") || strings.IndexFunc(p, unicode.IsControl) >= 0 || path.Clean(p) != p {
		return ErrManifest
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." || part == ".git" {
			return ErrManifest
		}
	}
	return nil
}

// validGlob accepts slash-separated segments made of literal characters, `*`
// and `?` within a segment, and `**` as a whole segment matching zero or more
// segments. Character classes and brace alternation are refused.
func validGlob(g string) error {
	if g == "" || len(g) > 256 || strings.HasPrefix(g, "/") || strings.HasSuffix(g, "/") ||
		strings.ContainsAny(g, "\\[]{}") || strings.IndexFunc(g, unicode.IsControl) >= 0 {
		return ErrManifest
	}
	for _, part := range strings.Split(g, "/") {
		if part == "" || part == "." || part == ".." || part == ".git" ||
			(strings.Contains(part, "**") && part != "**") {
			return ErrManifest
		}
	}
	return nil
}

func matchGlob(g, file string) bool {
	return matchSegments(strings.Split(g, "/"), strings.Split(file, "/"))
}

func matchSegments(pattern, parts []string) bool {
	for len(pattern) > 0 {
		if pattern[0] == "**" {
			rest := pattern[1:]
			for i := 0; i <= len(parts); i++ {
				if matchSegments(rest, parts[i:]) {
					return true
				}
			}
			return false
		}
		if len(parts) == 0 {
			return false
		}
		if ok, err := path.Match(pattern[0], parts[0]); err != nil || !ok {
			return false
		}
		pattern, parts = pattern[1:], parts[1:]
	}
	return len(parts) == 0
}

//go:embed builtin/*.json
var builtin embed.FS

// Builtin returns the reviewed manifest of a reference repository. It
// expresses exactly that repository's existing recipe, publication allowlist
// and merge policy.
func Builtin(repository string) (Manifest, bool) {
	name := map[string]string{Homedir: "builtin/homedir.json", Saferus: "builtin/saferus.json"}[repository]
	if name == "" {
		return Manifest{}, false
	}
	raw, err := builtin.ReadFile(name)
	if err != nil {
		return Manifest{}, false
	}
	m, err := ParseManifest(raw)
	if err != nil || m.Repository != repository {
		return Manifest{}, false
	}
	return m, true
}
