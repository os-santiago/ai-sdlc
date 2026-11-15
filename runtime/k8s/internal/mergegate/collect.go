package mergegate

import (
	"context"
	"encoding/json"
	"regexp"
	"sort"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

// Expected must come from a checked terminal publication record, not an issue
// field or unauthenticated caller. This collector alone does not verify it.
type Expected struct {
	PullRequest                       int
	HeadSHA, BaseSHA, TreeSHA, Branch string
}
type RequiredCheck struct {
	Context string
	AppID   int64
}

// Policy is reviewed installation configuration, outside generated workspaces.
type Policy struct {
	RulesetID int
	Required  []RequiredCheck
}

func PilotPolicy() Policy {
	return Policy{9071701, []RequiredCheck{{"Quality Summary", 15368}, {"CI Summary", 15368}, {"Quality Gate Summary", 15368}}}
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
}

var branch = regexp.MustCompile(`^ai-sdlc/issue-[1-9][0-9]{0,9}-[a-f0-9]{32}$`)

func (p Policy) valid() bool {
	if p.RulesetID <= 0 || len(p.Required) < 1 || len(p.Required) > 16 {
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

func (p pull) matches(e Expected) bool {
	return p.Number == e.PullRequest && p.State == "open" && p.Draft != nil && !*p.Draft && p.Merged != nil && !*p.Merged && p.Head.SHA == e.HeadSHA && p.Head.Ref == e.Branch && p.Head.Repo.FullName == repository && p.Base.SHA == e.BaseSHA && p.Base.Ref == "main" && p.Base.Repo.FullName == repository && p.Base.Repo.DefaultBranch == "main"
}
func (c *Client) Collect(ctx context.Context, e Expected, p Policy) (Observation, error) {
	deny := func() (Observation, error) { return Observation{}, ErrGate }
	if c == nil || c.http == nil || e.PullRequest < 1 || e.PullRequest > 2147483647 || !workspacebundle.ValidSourceSHA(e.HeadSHA) || !workspacebundle.ValidSourceSHA(e.BaseSHA) || !workspacebundle.ValidSourceSHA(e.TreeSHA) || !branch.MatchString(e.Branch) || !p.valid() {
		return deny()
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	var first pull
	if c.read(ctx, repoPath("/pulls/"+number(e.PullRequest)), nil, &first) != nil || !first.matches(e) {
		return deny()
	}
	reasons := map[string]bool{}
	if first.Mergeable == nil || !*first.Mergeable || first.MergeableState != "clean" {
		reasons["native-merge-not-ready"] = true
	}
	var commit struct {
		SHA     string
		Tree    struct{ SHA string }
		Parents []struct{ SHA string }
	}
	if c.read(ctx, repoPath("/git/commits/"+e.HeadSHA), nil, &commit) != nil || commit.SHA != e.HeadSHA || commit.Tree.SHA != e.TreeSHA || len(commit.Parents) != 1 || commit.Parents[0].SHA != e.BaseSHA {
		return deny()
	}
	policyDigest, required, safe, err := c.nativePolicy(ctx, p)
	if err != nil {
		return deny()
	}
	if !safe {
		reasons["native-protection-insufficient"] = true
	}
	checks, digest, err := c.checks(ctx, e.HeadSHA, required)
	if err != nil {
		return deny()
	}
	if !checks {
		reasons["checks-not-passed"] = true
	}
	reviews, reviewDigest, err := c.reviews(ctx, e)
	if err != nil {
		return deny()
	}
	if !reviews {
		reasons["review-feedback-unresolved"] = true
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
	if c.read(ctx, repoPath("/pulls/"+number(e.PullRequest)), nil, &latest) != nil || !latest.matches(e) {
		return deny()
	}
	if latest.Mergeable == nil || !*latest.Mergeable || latest.MergeableState != "clean" {
		reasons["native-merge-not-ready"] = true
	}
	if ctx.Err() != nil {
		return deny()
	}
	list := []string{}
	for reason := range reasons {
		list = append(list, reason)
	}
	sort.Strings(list)
	return Observation{Expected: e, PolicySHA256: policyDigest, ChecksSHA256: digest, ReviewsSHA256: reviewDigest, ObservedAt: time.Now().UTC(), Ready: len(list) == 0, Reasons: list}, nil
}
func digest(v any) string { raw, _ := json.Marshal(v); return authority.Digest(raw) }
