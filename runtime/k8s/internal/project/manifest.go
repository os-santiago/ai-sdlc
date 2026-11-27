package project

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

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
	// Generation is optional and omitted when absent, so manifests without
	// it keep their canonical form and digest. It is a separate section, not
	// part of Verification, because the verifier's settings digest encodes
	// Verification.
	Generation *Generation `json:"generation,omitempty"`
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
	// HarnessGlobs are files that change how tests run or report (build and
	// dependency files, test configuration, shared helpers, test data); a
	// candidate that changes one is refused.
	HarnessGlobs []string `json:"harnessGlobs,omitempty"`
	// Env adds reviewed variables for the toolchain (for example PATH or a
	// tool home inside the image). HOME, TMPDIR and the runner's fixed
	// variables cannot be overridden.
	Env map[string]string `json:"env,omitempty"`
	// Analysis is optional static analysis, omitted when absent so manifests
	// without it keep their canonical form, digest and verifier settings.
	Analysis *Analysis `json:"analysis,omitempty"`
	// Dependencies is an optional read-only dependency cache, omitted when
	// absent so manifests without it keep their canonical form, digest and
	// verifier settings. See dependencies.go.
	Dependencies *Dependencies `json:"dependencies,omitempty"`
}

// Analysis declares static analysis commands (for example go vet ./...) that
// the verifier runs, offline, in the same pinned image, working directory and
// environment as the tests, on the baseline and on the candidate. A command
// that passes on the baseline and fails on the candidate refuses it; a command
// that already fails on the baseline gives no signal for that candidate.
type Analysis struct {
	Commands       [][]string `json:"commands"`
	TimeoutSeconds int        `json:"timeoutSeconds"`
}

// Bounds of Analysis.
const (
	MaxAnalysisCommands       = 4
	MinAnalysisTimeoutSeconds = 10
	MaxAnalysisTimeoutSeconds = 300
	// MaxAnalysisTotalSeconds bounds all commands together: analysis runs
	// inside the same bounded verification command window as the tests.
	MaxAnalysisTotalSeconds = 600
)

func (a Analysis) validate(workingDir string) error {
	if len(a.Commands) == 0 || len(a.Commands) > MaxAnalysisCommands ||
		a.TimeoutSeconds < MinAnalysisTimeoutSeconds || a.TimeoutSeconds > MaxAnalysisTimeoutSeconds ||
		len(a.Commands)*a.TimeoutSeconds > MaxAnalysisTotalSeconds {
		return ErrManifest
	}
	for _, command := range a.Commands {
		if !ValidCommand(command, workingDir) {
			return ErrManifest
		}
	}
	return nil
}

// AnalysisCommands is the number of analysis commands, or 0 without analysis.
func (v Verification) AnalysisCommands() int {
	if v.Analysis == nil {
		return 0
	}
	return len(v.Analysis.Commands)
}

// Bounds of Verification.Env.
const (
	MaxEnv           = 16
	MaxEnvValueBytes = 1024
)

var (
	envName     = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
	reservedEnv = map[string]bool{"HOME": true, "TMPDIR": true, "CI": true, "LC_ALL": true, "TZ": true}
)

// SearchPath returns the directories of a reviewed PATH entry, or nil.
func (v Verification) SearchPath() []string {
	if v.Env["PATH"] == "" {
		return nil
	}
	return strings.Split(v.Env["PATH"], ":")
}

func validEnv(env map[string]string) bool {
	if len(env) > MaxEnv {
		return false
	}
	for name, value := range env {
		if !envName.MatchString(name) || reservedEnv[name] || strings.HasPrefix(name, "LD_") || value == "" ||
			len(value) > MaxEnvValueBytes || strings.IndexFunc(value, hiddenRune) >= 0 {
			return false
		}
	}
	if path, ok := env["PATH"]; ok {
		for _, dir := range strings.Split(path, ":") {
			if !strings.HasPrefix(dir, "/") || (dir != "/" && cleanPath(dir[1:]) != nil) {
				return false
			}
			// Tools come from the toolchain image, never from directories the
			// repository or the run can write.
			for _, writable := range []string{"/workspaces", "/tmp", "/runner"} {
				if dir == writable || strings.HasPrefix(dir, writable+"/") {
					return false
				}
			}
		}
	}
	return true
}

// Generation declares reviewed inputs for the generation sandbox. It requires
// Verification: the toolchain is copied from the verifier's pinned image.
type Generation struct {
	Toolchain *Toolchain `json:"toolchain"`
}

// Toolchain names a self-contained toolchain inside the verification image.
// Root is copied, bounded, into a volume the generation worker mounts
// read-only; Path lists directories under Root to put on the worker's PATH.
type Toolchain struct {
	Root string   `json:"root"`
	Path []string `json:"path"`
}

// Bounds of Toolchain.
const (
	MaxToolchainPath      = 8
	MaxToolchainRootBytes = 256
)

// toolchainExcluded are directories a toolchain root may not be or be under:
// the run's writable directories and kernel or device filesystems.
// Runtime, credential and writable locations: a service account token or host
// state must never be copied into a volume the generator can read.
var toolchainExcluded = []string{"/workspaces", "/tmp", "/runner", "/proc", "/sys", "/dev", "/run", "/var/run", "/var/tmp", "/etc"}

// ValidateToolchainRoot reports whether root is an absolute, clean directory
// path in the conservative character set, other than / and outside the run's
// writable and kernel directories.
func ValidateToolchainRoot(root string) error {
	if len(root) < 2 || len(root) > MaxToolchainRootBytes || root[0] != '/' || cleanPath(root[1:]) != nil {
		return ErrManifest
	}
	for _, excluded := range toolchainExcluded {
		if root == excluded || strings.HasPrefix(root, excluded+"/") {
			return ErrManifest
		}
	}
	return nil
}

func (g Generation) validate() error {
	if g.Toolchain == nil {
		return ErrManifest
	}
	return g.Toolchain.validate()
}

// generationEnvNames is the strict allowlist of verification.env names the
// generation toolchain receives: Go's, cgo's and Rust's. They reach SCC's own
// Node.js process as well as run_shell children, so nothing that configures
// Node.js, OpenSSL, the C library, a shell, Python, Git, npm, SCC or the
// launcher qualifies. Other variables stay verification-only.
var generationEnvNames = regexp.MustCompile(`^(GO[A-Z0-9]+|CGO_[A-Z0-9_]+|CARGO_[A-Z0-9_]+|RUST[A-Z0-9_]*)$`)

// writableValue matches a value that names the generation worker's writable
// workspace or scratch volume, for example GOCACHE=/workspaces/cache or
// GOFLAGS=-modfile=/scratch/go.mod.
var writableValue = regexp.MustCompile(`(^|[:=, ])/(workspaces|scratch)(/|$|[:, ])`)

// GenerationEnvName reports whether a verification variable reaches the
// generation toolchain.
func GenerationEnvName(name string) bool { return generationEnvNames.MatchString(name) }

// GenerationEnvValue reports whether a value may reach the generation
// toolchain: it must not name /workspaces or /scratch.
func GenerationEnvValue(value string) bool {
	if writableValue.MatchString(value) {
		return false
	}
	// A path that is not in canonical form (for example /opt/go/../../scratch)
	// could resolve to a writable volume the lexical check above misses.
	for _, token := range pathTokens.Split(value, -1) {
		if i := strings.IndexByte(token, '/'); i >= 0 && path.Clean(token[i:]) != token[i:] {
			return false
		}
	}
	return true
}

// pathTokens splits a value into the pieces that can hold a path: list
// entries, flag values, words and quoted or semicolon-separated parts.
var pathTokens = regexp.MustCompile(`[:=, \t;"']`)

// namesRoot reports whether s mentions root as a whole path component: root
// followed by the end of s or by a character that cannot continue a path
// component name ('/', a quote, ';' and so on). "/opt/go-old" does not name
// "/opt/go".
func namesRoot(root, s string) bool {
	for i := strings.Index(s, root); i >= 0; {
		end := i + len(root)
		if end == len(s) || !componentByte(s[end]) {
			return true
		}
		next := strings.Index(s[i+1:], root)
		if next < 0 {
			return false
		}
		i += 1 + next
	}
	return false
}

// ErrGenerationEnv refuses a manifest with generation.toolchain whose
// verification.env gives a toolchain variable a value under /workspaces or
// /scratch, the generator's writable volumes.
var ErrGenerationEnv = fmt.Errorf("%w: a verification.env variable passed to generation.toolchain (GO*, CGO_*, CARGO_*, RUST*) names /workspaces or /scratch, uses a non-canonical path, or names the toolchain root other than as a whole ':'-separated path entry", ErrManifest)

// underRoot reports whether p is root or a path below it.
func underRoot(root, p string) bool {
	return p == root || strings.HasPrefix(p, root+"/")
}

// componentByte reports whether b can continue a path component name.
func componentByte(b byte) bool {
	return b == '.' || b == '_' || b == '-' || b == '+' || b == '@' || b == '~' ||
		(b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// RelocateToolchainPath maps a path at or below root to the same path below
// dest, where the generation worker mounts the copy; any other path is
// returned unchanged.
func RelocateToolchainPath(root, dest, p string) string {
	if !underRoot(root, p) {
		return p
	}
	return dest + strings.TrimPrefix(p, root)
}

// RelocateToolchainValue maps every ':'-separated entry of value that is root
// or lies below it to dest (GOROOT=/usr/local/go becomes the copy). ok is
// false when value names root anywhere else (for example inside a flag such
// as -toolexec=/usr/local/go/x), which cannot be translated safely.
func RelocateToolchainValue(root, dest, value string) (string, bool) {
	entries := strings.Split(value, ":")
	for i, entry := range entries {
		if underRoot(root, entry) {
			if path.Clean(entry) != entry {
				return "", false
			}
			entries[i] = RelocateToolchainPath(root, dest, entry)
			continue
		}
		// Any other mention of root, including one attached to a flag such as
		// -I/opt/go/include or inside a file:// URL, cannot be translated.
		if namesRoot(root, entry) {
			return "", false
		}
	}
	return strings.Join(entries, ":"), true
}

// Validate reports whether t is a valid toolchain declaration. The executor
// rechecks the copy of it that an assignment carries.
func (t Toolchain) Validate() error { return t.validate() }

// ValidEnv reports whether env is a valid Verification.Env.
func ValidEnv(env map[string]string) bool { return validEnv(env) }

// ValidCommand reports whether command and workingDir are a valid
// Verification.Command and Verification.WorkingDir.
func ValidCommand(command []string, workingDir string) bool {
	if len(command) == 0 || len(command) > 32 || (workingDir != "" && cleanPath(workingDir) != nil) {
		return false
	}
	for _, arg := range command {
		if arg == "" || len(arg) > 1024 || strings.IndexFunc(arg, hiddenRune) >= 0 {
			return false
		}
	}
	return true
}

// ValidImage reports whether image is a pinned verification image reference.
func ValidImage(image string) bool { return pinnedImage.MatchString(image) }

func (t Toolchain) validate() error {
	if ValidateToolchainRoot(t.Root) != nil || len(t.Path) == 0 || len(t.Path) > MaxToolchainPath {
		return ErrManifest
	}
	seen := map[string]bool{}
	for _, dir := range t.Path {
		if len(dir) > MaxToolchainRootBytes || cleanPath(dir) != nil || seen[dir] {
			return ErrManifest
		}
		seen[dir] = true
	}
	return nil
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
	// CommitIdentity is optional and omitted when absent, so manifests
	// without it keep their canonical form and digest. Absent means
	// DefaultCommitIdentity.
	CommitIdentity *CommitIdentity `json:"commitIdentity,omitempty"`
}

// CommitIdentity is the author and committer of every commit a publication
// creates, and the identity of its Signed-off-by trailer.
type CommitIdentity struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// DefaultCommitIdentity is a neutral identity. The reserved .invalid domain
// (RFC 2606) never resolves, so the address cannot be delivered to, or
// attributed to, any real person or forge account.
var DefaultCommitIdentity = CommitIdentity{Name: "ai-sdlc[bot]", Email: "noreply@ai-sdlc.invalid"}

// Identity returns the configured commit identity, or the default.
func (p Publication) Identity() CommitIdentity {
	if p.CommitIdentity == nil {
		return DefaultCommitIdentity
	}
	return *p.CommitIdentity
}

var commitEmail = regexp.MustCompile(`^[A-Za-z0-9._%+-]{1,64}@[A-Za-z0-9-]{1,63}(\.[A-Za-z0-9-]{1,63})+$`)

// Validate refuses an identity that Git would rewrite or that could change
// the meaning of a commit header. Git trims whitespace and the characters
// . , : ; < > " \ ' from both ends of a name, and a name cannot hold an angle
// bracket or a line break. A rewritten identity would make the commit the
// forge stores differ from the commit the runtime planned. Unicode white
// space is also refused at either end (a forge may trim it), and line and
// paragraph separators anywhere.
func (c CommitIdentity) Validate() error {
	if len(c.Name) == 0 || len(c.Name) > 100 || !utf8.ValidString(c.Name) || strings.ContainsAny(c.Name, "<>") ||
		strings.IndexFunc(c.Name, hiddenRune) >= 0 || strings.IndexFunc(c.Name, separatorRune) >= 0 ||
		gitCrud(c.Name[0]) || gitCrud(c.Name[len(c.Name)-1]) || spaceEnd(c.Name) ||
		len(c.Email) > 254 || !commitEmail.MatchString(c.Email) {
		return ErrManifest
	}
	return nil
}

func separatorRune(r rune) bool { return unicode.In(r, unicode.Zl, unicode.Zp) }

func spaceEnd(s string) bool {
	first, _ := utf8.DecodeRuneInString(s)
	last, _ := utf8.DecodeLastRuneInString(s)
	return unicode.IsSpace(first) || unicode.IsSpace(last)
}

func gitCrud(b byte) bool { return b <= ' ' || strings.IndexByte(".,:;<>\"\\'", b) >= 0 }

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
	ErrManifest = errors.New("invalid project manifest")
	// GitHub names are case-insensitive: manifests use the lowercase form.
	// The same pattern as the publication schema (migration 0038), so a valid
	// manifest can always be published.
	repositoryName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,99}/[a-z0-9][a-z0-9._-]{0,99}$`)
	branchName     = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,99}(/[A-Za-z0-9_][A-Za-z0-9._-]{0,99}){0,4}$`)
	// Paths and globs use a conservative character set: no Unicode format
	// characters, lookalikes or shell metacharacters.
	pathChars    = regexp.MustCompile(`^[A-Za-z0-9._@+/-]+$`)
	globChars    = regexp.MustCompile(`^[A-Za-z0-9._@+/*?-]+$`)
	pinnedImage  = regexp.MustCompile(`^[a-z0-9.-]+(:[0-9]{1,5})?(/[a-z0-9._-]+)+@sha256:[a-f0-9]{64}$`)
	cpuQuantity  = regexp.MustCompile(`^([1-9][0-9]{0,3}m|[1-8])$`)
	byteQuantity = regexp.MustCompile(`^([1-9][0-9]{0,4}Mi|([1-9]|[1-5][0-9]|6[0-4])Gi)$`)
	recipeName   = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*/v[1-9][0-9]{0,2}$`)
	// Repository automation and Git metadata are never publishable.
	alwaysDenied      = []string{".github/**", "**/.gitmodules", "**/.gitattributes", "**/CODEOWNERS"}
	maxPublishedBytes = 4 * 1024 * 1024
)

// ParseManifest decodes an installed manifest strictly (unknown fields and
// duplicate keys are refused) and validates it.
func ParseManifest(raw []byte) (Manifest, error) {
	var m Manifest
	// Object keys must be ASCII (escaped or literal): encoding/json folds
	// Unicode keys (for example U+017F to "s"), which would let a duplicate
	// key escape the strict decoder. Values may use JSON escapes.
	if len(raw) == 0 || len(raw) > MaxManifestBytes || asciiKeys(raw) != nil {
		return Manifest{}, ErrManifest
	}
	if len(raw) == 0 || len(raw) > MaxManifestBytes || authority.DecodeStrict(raw, &m) != nil || explicitMerge(raw) != nil {
		return Manifest{}, ErrManifest
	}
	if err := m.Validate(); err != nil {
		if errors.Is(err, ErrGenerationEnv) {
			return Manifest{}, ErrGenerationEnv
		}
		return Manifest{}, ErrManifest
	}
	return m, nil
}

func asciiKeys(raw []byte) error {
	var doc any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&doc) != nil {
		return ErrManifest
	}
	var walk func(any) error
	walk = func(v any) error {
		switch value := v.(type) {
		case map[string]any:
			for key, child := range value {
				if strings.IndexFunc(key, func(r rune) bool { return r > 0x7f }) >= 0 || walk(child) != nil {
					return ErrManifest
				}
			}
		case []any:
			for _, child := range value {
				if walk(child) != nil {
					return ErrManifest
				}
			}
		}
		return nil
	}
	return walk(doc)
}

// explicitMerge requires the merge booleans and approval count to be stated,
// so an omitted field cannot silently weaken the merge policy.
func explicitMerge(raw []byte) error {
	var doc struct {
		Merge map[string]json.RawMessage `json:"merge"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return ErrManifest
	}
	for _, field := range []string{"strictChecks", "resolveThreads", "humanApprovals"} {
		if _, ok := doc.Merge[field]; !ok {
			return ErrManifest
		}
	}
	return nil
}

// Digest is the canonical sha256 of the manifest (its re-marshalled struct,
// not the installed file bytes), suitable for binding into signed policy.
func (m Manifest) Digest() string {
	return authority.Digest(m.Canonical())
}

// Canonical is the manifest's canonical JSON: struct field order, no HTML
// escaping (which would emit \\u escapes the parser refuses) and no trailing
// newline. ParseManifest accepts it and Digest hashes it.
func (m Manifest) Canonical() []byte {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(m)
	return bytes.TrimSuffix(out.Bytes(), []byte("\n"))
}

func (m Manifest) Validate() error {
	if m.Version != ManifestVersion || !validRepository(m.Repository) || !branchName.MatchString(m.BaseBranch) ||
		strings.Contains(m.BaseBranch, "..") || m.BaseBranch == "HEAD" || strings.HasSuffix(m.BaseBranch, ".lock") {
		return ErrManifest
	}
	if m.Verification == nil && len(m.Recipes) == 0 {
		return ErrManifest
	}
	if m.Verification != nil && m.Verification.validate() != nil {
		return ErrManifest
	}
	if m.Generation != nil && (m.Verification == nil || m.Generation.validate() != nil) {
		return ErrManifest
	}
	if m.Generation != nil {
		for name, value := range m.Verification.Env {
			if !GenerationEnvName(name) {
				continue
			}
			if _, ok := RelocateToolchainValue(m.Generation.Toolchain.Root, "/", value); !GenerationEnvValue(value) || !ok {
				return ErrGenerationEnv
			}
		}
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

// Validate reports whether the settings satisfy the reviewed manifest rules
// on their own, as the offline runner and the workload compiler receive them.
func (v Verification) Validate() error { return v.validate() }

// hiddenRune reports control and format characters (bidirectional overrides,
// zero-width characters), which would make a reviewed value read differently
// from what runs.
func hiddenRune(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }

func (v Verification) validate() error {
	if !pinnedImage.MatchString(v.Image) || len(v.Command) == 0 || len(v.Command) > 32 ||
		v.TimeoutSeconds < 30 || v.TimeoutSeconds > 600 || !cpuQuantity.MatchString(v.CPU) ||
		!byteQuantity.MatchString(v.Memory) || !byteQuantity.MatchString(v.EphemeralStorage) ||
		v.Network != "none" || !v.Result.ExitCode || v.Repeat < 1 || v.Repeat > 3 ||
		len(v.TestGlobs) == 0 || len(v.TestGlobs) > 32 || len(v.HarnessGlobs) > 64 || !validEnv(v.Env) {
		return ErrManifest
	}
	for _, g := range v.HarnessGlobs {
		if validGlob(g) != nil {
			return ErrManifest
		}
	}
	if !ValidCommand(v.Command, v.WorkingDir) {
		return ErrManifest
	}
	if v.Analysis != nil && v.Analysis.validate(v.WorkingDir) != nil {
		return ErrManifest
	}
	if v.Dependencies != nil && v.Dependencies.validate() != nil {
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
		p.MaxFileBytes < 1 || p.MaxFileBytes > p.MaxChangedBytes ||
		(p.CommitIdentity != nil && p.CommitIdentity.Validate() != nil) {
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
		if c.Context == "" || len(c.Context) > 128 || strings.IndexFunc(c.Context, hiddenRune) >= 0 ||
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

// MatchesHarness reports whether a path is part of the test harness.
func (v Verification) MatchesHarness(file string) bool {
	if cleanPath(file) != nil {
		return false
	}
	for _, g := range v.HarnessGlobs {
		if matchGlob(g, file) {
			return true
		}
	}
	// The files that define the dependency set are always harness: the
	// cache was prefetched, with network access, from the base revision's.
	return v.MatchesDependencies(file)
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

// CleanPath reports whether p is a clean, relative, slash-separated
// repository path in the conservative character set.
func CleanPath(p string) error { return cleanPath(p) }

func cleanPath(p string) error {
	if p == "" || len(p) > 1024 || !pathChars.MatchString(p) || strings.HasPrefix(p, "/") ||
		strings.HasSuffix(p, "/") || path.Clean(p) != p {
		return ErrManifest
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." || strings.EqualFold(part, ".git") {
			return ErrManifest
		}
	}
	return nil
}

// validGlob accepts slash-separated segments made of literal characters, `*`
// and `?` within a segment, and `**` as a whole segment matching zero or more
// segments. Character classes and brace alternation are refused.
func validGlob(g string) error {
	if g == "" || len(g) > 256 || !globChars.MatchString(g) || strings.HasPrefix(g, "/") || strings.HasSuffix(g, "/") {
		return ErrManifest
	}
	previous, doubles := "", 0
	for _, part := range strings.Split(g, "/") {
		if part == "" || part == "." || part == ".." || strings.EqualFold(part, ".git") ||
			(strings.Contains(part, "**") && part != "**") || (part == "**" && previous == "**") {
			return ErrManifest
		}
		if part == "**" {
			doubles++
		}
		previous = part
	}
	if doubles > 4 {
		return ErrManifest
	}
	return nil
}

// matchGlob is a dynamic program over (pattern segment, path segment): it is
// O(len(pattern) * len(path)) for any mix of `**` segments.
func matchGlob(g, file string) bool {
	pattern, parts := strings.Split(g, "/"), strings.Split(file, "/")
	// reach[j] reports whether pattern[:i] matches parts[:j].
	reach := make([]bool, len(parts)+1)
	reach[0] = true
	for _, segment := range pattern {
		next := make([]bool, len(parts)+1)
		if segment == "**" {
			seen := false
			for j := range reach {
				seen = seen || reach[j]
				next[j] = seen
			}
		} else {
			for j := 1; j <= len(parts); j++ {
				if reach[j-1] {
					if ok, err := path.Match(segment, parts[j-1]); err == nil && ok {
						next[j] = true
					}
				}
			}
		}
		reach = next
	}
	return reach[len(parts)]
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
