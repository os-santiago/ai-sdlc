// Package executor defines the hidden, bounded SCC workload boundary. Its input
// is prepared by a trusted controller; parsing it is not execution authority.
package executor

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/candidate"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/generatortools"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/repair"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

const (
	AssignmentVersion  = "ai-sdlc.executor-assignment/v1"
	ProfileVersion     = "ai-sdlc.executor-profile/v1"
	MaxAssignmentBytes = 64 * 1024
	MaxProfileBytes    = 16 * 1024
)

var ErrInvalid = errors.New("invalid executor assignment or profile")
var uuidPattern = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)
var repoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var modelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)

type Assignment struct {
	Version            string             `json:"version"`
	WorkItemID         string             `json:"workItemId"`
	Attempt            int                `json:"attempt"`
	Repository         string             `json:"repository"`
	SourceSHA          string             `json:"sourceSHA"`
	TreeSHA256         string             `json:"treeSHA256"`
	ProfileSHA256      string             `json:"profileSHA256"`
	AuthorityRevision  string             `json:"authorityRevision"`
	IssueNumbers       []int              `json:"issueNumbers"`
	Title              string             `json:"title"`
	Body               string             `json:"body"`
	AcceptanceCriteria []string           `json:"acceptanceCriteria"`
	ExpiresAt          int64              `json:"expiresAt"`
	Repair             *repair.Assignment `json:"repair,omitempty"`
	// Workspace is installation-controlled context for the prompt, never
	// issue text: WorkspaceOfflineTests when the repository is verified with
	// its own tests offline and the generator's workspace cannot run them.
	Workspace string `json:"workspace,omitempty"`
	// Toolchain is the repository's self-contained toolchain from its
	// reviewed manifest; it requires WorkspaceOfflineTests. Omitted keeps
	// the original assignment bytes and workload.
	Toolchain *Toolchain `json:"toolchain,omitempty"`
	// Verification is the reviewed manifest's verification command, named in
	// the prompt of an offline workspace without a toolchain (a toolchain
	// already carries it). It requires WorkspaceOfflineTests and excludes
	// Toolchain. Omitted keeps the original assignment bytes.
	Verification *Verification `json:"verification,omitempty"`
	// Tools are the generator tools the reviewed manifest declares (its
	// tekton.tools with their targets). They are offered only when the
	// profile also enables tools. Omitted keeps the original assignment bytes.
	Tools *generatortools.Spec `json:"tools,omitempty"`
}

// Verification is installation-controlled assignment context, never issue or
// model text: verification.command and verification.workingDir from the
// repository's reviewed project manifest. It only informs the prompt.
type Verification struct {
	Command    []string `json:"command"`
	WorkingDir string   `json:"workingDir,omitempty"`
}

// Profile contains reviewed local bounds, never arbitrary shell commands,
// URLs, mounts, environment or credentials. Durable provider quotas are separate.
type Profile struct {
	Version    string `json:"version"`
	Model      string `json:"model"`
	MaxSteps   int    `json:"maxSteps"`
	MaxSeconds int    `json:"maxSeconds"`
	MaxTokens  int    `json:"maxTokens"`
	ADevSHA256 string `json:"aDevSHA256"`
	MaxRepairs int    `json:"maxRepairs,omitempty"`
	// CallSeconds is the per-model-call window (SCC wait, mailbox wait and the
	// call reservation). Omitted keeps the original 60 s window and identical
	// workloads; slower installed models select 60-600 s, within MaxSeconds.
	CallSeconds int `json:"callSeconds,omitempty"`
	// Tools enables generator tools backed by Tekton catalog runs (an MCP
	// server in SCC's trusted configuration and the mailbox tool route).
	// Omitted keeps the original configuration and workloads.
	Tools *ToolProfile `json:"tools,omitempty"`
}

// ToolProfile bounds the generator's tool calls: how many and how long each
// may take. It is installation configuration; which tools and targets exist
// comes from the repository's reviewed manifest (Assignment.Tools). Tool time
// is spent inside the generation's own MaxSeconds, because the generator waits
// for each result.
type ToolProfile struct {
	Calls   int `json:"calls"`
	Seconds int `json:"seconds"`
}

// Bounds of ToolProfile.
const (
	MaxToolCalls   = 8
	MinToolSeconds = 120
	MaxToolSeconds = 900
)

func (t ToolProfile) validate(maxSeconds int) error {
	if t.Calls < 1 || t.Calls > MaxToolCalls || t.Seconds < MinToolSeconds || t.Seconds > MaxToolSeconds || t.Seconds > maxSeconds {
		return ErrInvalid
	}
	return nil
}

const (
	DefaultCallSeconds = 60
	MaxCallSeconds     = 600
)

// CallWindow is the bounded wait for one model call.
func (p Profile) CallWindow() time.Duration {
	if p.CallSeconds == 0 {
		return DefaultCallSeconds * time.Second
	}
	return time.Duration(p.CallSeconds) * time.Second
}

// BaseSeconds is the default generation budget the fixed executor bounds
// were sized for. Every bound below equals its original value at or below it.
const BaseSeconds = 600

// extraSeconds is the reviewed generation time above BaseSeconds (0-300).
func (p Profile) extraSeconds() int {
	return max(0, min(p.MaxSeconds, 900)-BaseSeconds)
}

// SCCSeconds is the generation budget handed to SCC. SCC checks its time budget
// only before each model call and never interrupts one in flight, so a
// deadline one call window plus SCC's 15 s provider-wait slack before the
// launcher's kill at MaxSeconds lets the last call it starts answer (or time out
// at the mailbox); SCC then ends gracefully with exit 22 and its summary instead
// of being killed with exit 124. The margin comes only from seconds above the
// 600 s default and is capped at an eighth of the budget: the default profile
// gives SCC exactly the 600 s it had before, and a larger budget never gives
// SCC less than that. Reviewer profiles keep their original budget.
func SCCSeconds(p Profile) int {
	if p.Version != ProfileVersion {
		return p.MaxSeconds
	}
	margin := min(int((p.CallWindow()+15*time.Second)/time.Second), p.MaxSeconds/8, p.extraSeconds())
	return p.MaxSeconds - margin
}

// PodDeadlineSeconds is the executor Pod's activeDeadlineSeconds: the original
// 1,020 s plus the generation seconds above 600 (at most 1,320 s at 900). The
// Pod therefore keeps the same 300 s beyond the lifecycle claim (MaxSeconds
// plus 120 s) at every reviewed budget, and the default workload is unchanged.
func PodDeadlineSeconds(p Profile) int {
	return 1020 + p.extraSeconds()
}

// ProcessSeconds is the launcher's absolute bound from process start: the
// original 980 s plus the generation seconds above 600 (at most 1,280 s). It
// keeps 380 s beyond MaxSeconds for the bounded start wait (60 s), baseline
// (20 s), source verification and SCC exit, and always ends 40 s before the
// Pod deadline.
func ProcessSeconds(p Profile) int {
	return 980 + p.extraSeconds()
}

// DefaultProcessSeconds bounds the launcher before its reviewed profile has
// been read, and whenever that profile cannot be read (the run then fails).
const DefaultProcessSeconds = 980

func strictBound(raw []byte, expected string, maximum int, keys []string, value any) error {
	if len(raw) == 0 || len(raw) > maximum || !utf8.Valid(raw) || !workspacebundle.ValidDigest(expected) || workspacebundle.Digest(raw) != expected || authority.DecodeStrict(raw, value) != nil {
		return ErrInvalid
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || len(fields) != len(keys) {
		return ErrInvalid
	}
	for _, key := range keys {
		v, ok := fields[key]
		if !ok || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return ErrInvalid
		}
	}
	return nil
}
func parseProfile(raw []byte, expected, version string) (Profile, error) {
	var p Profile
	keys := []string{"version", "model", "maxSteps", "maxSeconds", "maxTokens", "aDevSHA256"}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) == nil && fields["maxRepairs"] != nil && version == ProfileVersion {
		keys = append(keys, "maxRepairs")
	}
	window := fields["callSeconds"] != nil
	if window {
		keys = append(keys, "callSeconds")
	}
	tools := fields["tools"] != nil
	if tools {
		keys = append(keys, "tools")
	}
	if strictBound(raw, expected, MaxProfileBytes, keys, &p) != nil || p.Version != version || !modelPattern.MatchString(p.Model) || p.MaxSteps < 1 || p.MaxSteps > 32 || p.MaxSeconds < 1 || p.MaxSeconds > 900 || p.MaxTokens < 1 || p.MaxTokens > 8192 || !workspacebundle.ValidDigest(p.ADevSHA256) {
		return Profile{}, ErrInvalid
	}
	if p.MaxRepairs < 0 || p.MaxRepairs > 1 {
		return Profile{}, ErrInvalid
	}
	if window && (p.CallSeconds < DefaultCallSeconds || p.CallSeconds > MaxCallSeconds || p.CallSeconds > p.MaxSeconds) {
		return Profile{}, ErrInvalid
	}
	if tools && (version != ProfileVersion || p.Tools == nil || p.Tools.validate(p.MaxSeconds) != nil) {
		return Profile{}, ErrInvalid
	}
	return p, nil
}

// Generation parsing remains purpose-specific: a reviewer profile cannot be
// substituted into an installed generation controller.
func ParseProfile(raw []byte, expected string) (Profile, error) {
	return parseProfile(raw, expected, ProfileVersion)
}

const ReviewProfileVersion = "ai-sdlc.reviewer-profile/v1"

func ParseReviewProfile(raw []byte, expected string) (Profile, error) {
	p, err := parseProfile(raw, expected, ReviewProfileVersion)
	if err != nil || p.MaxSteps > 8 || p.MaxSeconds > 180 {
		return Profile{}, ErrInvalid
	}
	return p, nil
}
func parseWorkloadProfile(raw []byte, expected string) (Profile, error) {
	if p, err := ParseProfile(raw, expected); err == nil {
		return p, nil
	}
	return ParseReviewProfile(raw, expected)
}

// WorkspaceOfflineTests marks an assignment whose repository is verified
// with its own tests in independent offline verification.
const WorkspaceOfflineTests = "offline-repository-tests"

func ParseAssignment(raw []byte, expected string, now time.Time) (Assignment, error) {
	var a Assignment
	keys := []string{"version", "workItemId", "attempt", "repository", "sourceSHA", "treeSHA256", "profileSHA256", "authorityRevision", "issueNumbers", "title", "body", "acceptanceCriteria", "expiresAt"}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) == nil && fields["repair"] != nil {
		keys = append(keys, "repair")
	}
	if fields["workspace"] != nil {
		keys = append(keys, "workspace")
	}
	if fields["toolchain"] != nil {
		keys = append(keys, "toolchain")
	}
	if fields["verification"] != nil {
		keys = append(keys, "verification")
	}
	if fields["tools"] != nil {
		keys = append(keys, "tools")
	}
	if strictBound(raw, expected, MaxAssignmentBytes, keys, &a) != nil || a.Version != AssignmentVersion || !uuidPattern.MatchString(a.WorkItemID) || a.Attempt < 1 || a.Attempt > 16 || len(a.Repository) > 256 || !repoPattern.MatchString(a.Repository) || !workspacebundle.ValidSourceSHA(a.SourceSHA) || !workspacebundle.ValidDigest(a.TreeSHA256) || !workspacebundle.ValidDigest(a.ProfileSHA256) || !workspacebundle.ValidDigest(a.AuthorityRevision) {
		return Assignment{}, ErrInvalid
	}
	if a.Repair != nil && (a.Attempt != 2 || !a.Repair.Valid() || a.TreeSHA256 != a.Repair.Reference.ParentTreeSHA256) {
		return Assignment{}, ErrInvalid
	}
	if a.Workspace != "" && a.Workspace != WorkspaceOfflineTests {
		return Assignment{}, ErrInvalid
	}
	if a.Toolchain != nil && (a.Workspace != WorkspaceOfflineTests || !a.Toolchain.valid()) {
		return Assignment{}, ErrInvalid
	}
	if a.Verification != nil && (a.Workspace != WorkspaceOfflineTests || a.Toolchain != nil || !project.ValidCommand(a.Verification.Command, a.Verification.WorkingDir)) {
		return Assignment{}, ErrInvalid
	}
	if a.Tools != nil && a.Tools.Validate() != nil {
		return Assignment{}, ErrInvalid
	}
	for _, part := range strings.Split(a.Repository, "/") {
		if part == "." || part == ".." {
			return Assignment{}, ErrInvalid
		}
	}
	if len(a.IssueNumbers) < 1 || len(a.IssueNumbers) > 16 || len(a.AcceptanceCriteria) < 1 || len(a.AcceptanceCriteria) > 8 || len(a.Title) > 256 || strings.TrimSpace(a.Title) == "" || len(a.Body) > 32*1024 || strings.TrimSpace(a.Body) == "" {
		return Assignment{}, ErrInvalid
	}
	seen := map[int]bool{}
	for _, issue := range a.IssueNumbers {
		if issue < 1 || issue > 2147483647 || seen[issue] {
			return Assignment{}, ErrInvalid
		}
		seen[issue] = true
	}
	for _, criterion := range a.AcceptanceCriteria {
		if strings.TrimSpace(criterion) == "" || len(criterion) > 4096 {
			return Assignment{}, ErrInvalid
		}
	}
	expires := time.UnixMilli(a.ExpiresAt)
	if !expires.After(now) || expires.After(now.Add(time.Hour)) {
		return Assignment{}, ErrInvalid
	}
	return a, nil
}

// StepBudgetExitCode is the launcher's own exit code for an SCC generation
// that ended with SCC's exit 22 (BUDGET_EXCEEDED) on its tool-step budget. It
// is outside SCC's documented codes, and the launcher never passes an
// arbitrary SCC code through, so it cannot be produced any other way. The
// controller exports the candidate of such a run, when the workspace holds a
// change, for independent verification and review; it is not success.
const StepBudgetExitCode = candidate.StepBudgetExitCode

// budgetExitCode classifies SCC's exit 22 from facts the launcher measures
// itself, never from SCC's output, summary or audit log, which the generated
// tools can write. The pinned SCC ends a generation run with exit 22 for its
// time budget or its step budget only (no token budget is configured: the
// launcher's environment and arguments set none), and it checks the time
// budget first, on the wall clock (Date.now), measured from a start later
// than the launcher's own clock starts. The launcher reads the same interval
// on its monotonic clock and on the wall clock. The monotonic clock stops
// while a host or VM is suspended, and the wall clock can be stepped, so only
// when both readings are strictly below SCC's time budget (the larger is
// compared) can the time budget not have ended the run: it is the step
// budget. At or after that point the cause is ambiguous (a step budget
// reached at the deadline looks the same), so the run keeps exit 22 and stops
// as before. Reviewer runs always keep exit 22. A change of the worker image's
// SCC_ENGINE_PIN must recheck that SCC still orders its budget checks this way.
func budgetExitCode(p Profile, monotonic, wall time.Duration) int {
	elapsed := max(monotonic, wall)
	if p.Version == ProfileVersion && elapsed >= 0 && elapsed < time.Duration(SCCSeconds(p))*time.Second {
		return StepBudgetExitCode
	}
	return 22
}
