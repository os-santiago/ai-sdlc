package mergegate

import (
	"context"
	_ "embed"
	"encoding/json"
	"regexp"
	"sort"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

// Expected must come from a checked terminal publication record, not an issue
// field or unauthenticated caller. This collector alone does not verify it.
type Expected struct {
	Repository                        string
	PullRequest                       int
	HeadSHA, BaseSHA, TreeSHA, Branch string
}
type RequiredCheck struct {
	Context string
	AppID   int64
}

// Policy is reviewed installation configuration, outside generated workspaces.
type Policy struct {
	Repository string `json:"Repository"`
	Name       string `json:"Name,omitempty"`
	RulesetID  int
	Required   []RequiredCheck
}

//go:embed homedir-policy.json
var pilotManifest []byte

//go:embed saferus-policy.json
var saferusManifest []byte

func PilotPolicy() Policy {
	return PolicyForRepository(project.Homedir, "")
}

// PolicyForRepository returns the merge policy for a repository: the compiled
// policy of a built-in reference, otherwise the merge settings of the
// manifest the authorizing policy binds (projectSHA256 is that policy's
// ProjectSHA256), never another policy load's manifest.
func PolicyForRepository(repository, projectSHA256 string) Policy {
	var raw []byte
	switch repository {
	case project.Homedir:
		raw = pilotManifest
	case project.Saferus:
		raw = saferusManifest
	default:
		// Other repositories take their merge policy from the
		// policy-bound project manifest.
		m, ok := project.Bound(repository, projectSHA256)
		// Only values the merge gate enforces today are accepted: strict
		// required checks, resolved threads, no human approval count and
		// the main branch.
		if !ok || m.Merge.RulesetID > 2147483647 || !m.Merge.StrictChecks || !m.Merge.ResolveThreads ||
			m.Merge.HumanApprovals != 0 || m.BaseBranch != "main" {
			return Policy{}
		}
		p := Policy{Repository: m.Repository, RulesetID: int(m.Merge.RulesetID)}
		for _, c := range m.Merge.Required {
			p.Required = append(p.Required, RequiredCheck{Context: c.Context, AppID: c.AppID})
		}
		return p
	}
	var p Policy
	if json.Unmarshal(raw, &p) != nil {
		return Policy{}
	}
	return p
}

type Observation struct {
	MergeAuthorized bool      `json:"mergeAuthorized"`
	Expected        Expected  `json:"expected"`
	PolicySHA256    string    `json:"policySHA256"`
	ChecksSHA256    string    `json:"checksSHA256"`
	ReviewsSHA256   string    `json:"reviewsSHA256"`
	ObservedAt      time.Time `json:"observedAt"`
	Ready           bool      `json:"ready"`
	Reasons         []string  `json:"reasons"`
	// UnresolvedThreads counts the pull request's unresolved review threads
	// (at most 100; more fails closed). It is nonzero exactly when Reasons
	// holds "review-threads-unresolved", so a ready gate omits it.
	UnresolvedThreads int `json:"unresolvedThreads,omitempty"`
}

// The closed set of reasons an observation may carry. Each names one blocker.
const (
	ReasonNativeMergeNotReady          = "native-merge-not-ready"
	ReasonNativeProtectionInsufficient = "native-protection-insufficient"
	ReasonChecksNotPassed              = "checks-not-passed"
	ReasonChangesRequested             = "review-changes-requested"
	ReasonThreadsUnresolved            = "review-threads-unresolved"
)

// Reasons lists every reason Collect can report.
var Reasons = []string{ReasonNativeProtectionInsufficient, ReasonChecksNotPassed, ReasonChangesRequested, ReasonThreadsUnresolved, ReasonNativeMergeNotReady}

var branch = regexp.MustCompile(`^ai-sdlc/issue-[1-9][0-9]{0,9}-[a-f0-9]{32}$`)

func (p Policy) valid() bool {
	if !project.Supported(p.Repository) || p.RulesetID <= 0 || len(p.Required) < 1 || len(p.Required) > 16 {
		return false
	}
	seen := map[string]bool{}
	for _, v := range p.Required {
		if v.Context == "" || len(v.Context) > 256 || v.AppID <= 0 || seen[v.Context] {
			return false
		}
		seen[v.Context] = true
	}
	return true
}

type pull struct {
	Number                   int    `json:"number"`
	State                    string `json:"state"`
	Draft, Merged, Mergeable *bool
	MergeableState           string `json:"mergeable_state"`
	Head, Base               struct {
		Ref, SHA string
		Repo     struct {
			FullName      string `json:"full_name"`
			DefaultBranch string `json:"default_branch"`
		}
	}
}

func (p pull) matches(e Expected, repository string) bool {
	return p.Number == e.PullRequest && p.State == "open" && p.Draft != nil && !*p.Draft && p.Merged != nil && !*p.Merged && p.Head.SHA == e.HeadSHA && p.Head.Ref == e.Branch && p.Head.Repo.FullName == repository && p.Base.SHA == e.BaseSHA && p.Base.Ref == "main" && p.Base.Repo.FullName == repository && p.Base.Repo.DefaultBranch == "main"
}

// closed reports an authenticated observation of the expected pull request,
// same head branch and commit, that is no longer open: ErrClosed when it was
// closed unmerged, ErrMergedOutside when something other than this controller
// merged it. Anything else is not a closure and returns nil.
func (p pull) closed(e Expected, repository string) error {
	if p.Number != e.PullRequest || p.State != "closed" || p.Merged == nil || p.Head.SHA != e.HeadSHA || p.Head.Ref != e.Branch || p.Head.Repo.FullName != repository || p.Base.Ref != "main" || p.Base.Repo.FullName != repository {
		return nil
	}
	if *p.Merged {
		return ErrMergedOutside
	}
	return ErrClosed
}

func (c *Client) Collect(ctx context.Context, e Expected, p Policy) (Observation, error) {
	deny := func() (Observation, error) { return Observation{}, ErrGate }
	if c == nil || c.http == nil || !project.Supported(c.repository) || p.Repository != c.repository || e.Repository != c.repository || e.PullRequest < 1 || e.PullRequest > 2147483647 || !workspacebundle.ValidSourceSHA(e.HeadSHA) || !workspacebundle.ValidSourceSHA(e.BaseSHA) || !workspacebundle.ValidSourceSHA(e.TreeSHA) || !branch.MatchString(e.Branch) || !p.valid() {
		return deny()
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	var first pull
	if c.read(ctx, c.repoPath("/pulls/"+number(e.PullRequest)), nil, &first) != nil {
		return deny()
	}
	if closed := first.closed(e, c.repository); closed != nil {
		return Observation{}, closed
	}
	if !first.matches(e, c.repository) {
		return deny()
	}
	reasons := map[string]bool{}
	if first.Mergeable == nil || !*first.Mergeable || first.MergeableState != "clean" {
		reasons[ReasonNativeMergeNotReady] = true
	}
	var commit struct {
		SHA     string
		Tree    struct{ SHA string }
		Parents []struct{ SHA string }
	}
	if c.read(ctx, c.repoPath("/git/commits/"+e.HeadSHA), nil, &commit) != nil || commit.SHA != e.HeadSHA || commit.Tree.SHA != e.TreeSHA || len(commit.Parents) != 1 || commit.Parents[0].SHA != e.BaseSHA {
		return deny()
	}
	policyDigest, required, safe, err := c.nativePolicy(ctx, p)
	if err != nil {
		return deny()
	}
	if !safe {
		reasons[ReasonNativeProtectionInsufficient] = true
	}
	checks, digest, err := c.checks(ctx, e.HeadSHA, required)
	if err != nil {
		return deny()
	}
	if !checks {
		reasons[ReasonChecksNotPassed] = true
	}
	reviews, reviewDigest, err := c.reviews(ctx, e)
	if err != nil {
		return deny()
	}
	if reviews.ChangesRequested {
		reasons[ReasonChangesRequested] = true
	}
	if reviews.UnresolvedThreads > 0 {
		reasons[ReasonThreadsUnresolved] = true
	}
	// A ready snapshot cannot silently retain checks or feedback which changed
	// while other evidence was being collected. Native protection remains the
	// final atomic guard; this observation is never a merge capability.
	latestChecks, latestChecksDigest, err := c.checks(ctx, e.HeadSHA, required)
	if err != nil || latestChecks != checks || latestChecksDigest != digest {
		return deny()
	}
	latestReviews, latestReviewsDigest, err := c.reviews(ctx, e)
	if err != nil || latestReviews != reviews || latestReviewsDigest != reviewDigest {
		return deny()
	}
	// Re-observe mutable identities and protection after the bounded collection.
	latestDigest, _, latestSafe, err := c.nativePolicy(ctx, p)
	if err != nil || latestDigest != policyDigest || latestSafe != safe {
		return deny()
	}
	var latest pull
	if c.read(ctx, c.repoPath("/pulls/"+number(e.PullRequest)), nil, &latest) != nil || !latest.matches(e, c.repository) {
		return deny()
	}
	if latest.Mergeable == nil || !*latest.Mergeable || latest.MergeableState != "clean" {
		reasons[ReasonNativeMergeNotReady] = true
	}
	if ctx.Err() != nil {
		return deny()
	}
	list := []string{}
	for reason := range reasons {
		list = append(list, reason)
	}
	sort.Strings(list)
	return Observation{Expected: e, PolicySHA256: policyDigest, ChecksSHA256: digest, ReviewsSHA256: reviewDigest, ObservedAt: time.Now().UTC(), Ready: len(list) == 0, Reasons: list, UnresolvedThreads: reviews.UnresolvedThreads}, nil
}
func digest(v any) string { raw, _ := json.Marshal(v); return authority.Digest(raw) }
