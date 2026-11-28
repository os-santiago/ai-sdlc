package mergegate

import (
	"context"
	"encoding/json"
	"strings"
)

type review struct {
	ID    int64
	State string
	User  struct {
		ID   int64
		Type string
	}
	CommitID string `json:"commit_id"`
}

const threadQuery = `query($number:Int!,$owner:String!,$name:String!){repository(owner:$owner,name:$name){nameWithOwner pullRequest(number:$number){number headRefOid baseRefOid closingIssuesReferences(first:1,excludeUserLinked:false){totalCount pageInfo{hasNextPage} nodes{id}} reviewThreads(first:100){totalCount pageInfo{hasNextPage} nodes{id isResolved}}}}}`

// reviewState is the review evidence of one observation. ChangesRequested and
// UnresolvedThreads are separate blockers, so the merge controller can name
// the one that actually holds the pull request.
type reviewState struct {
	ChangesRequested  bool
	UnresolvedThreads int
}

func (c *Client) reviews(ctx context.Context, e Expected) (reviewState, string, error) {
	deny := func() (reviewState, string, error) { return reviewState{}, "", ErrGate }
	var reviews []review
	if c.read(ctx, c.repoPath("/pulls/"+number(e.PullRequest)+"/reviews?per_page=100"), nil, &reviews) != nil || reviews == nil || len(reviews) > 100 {
		return deny()
	}
	latest := map[int64]review{}
	seen := map[int64]bool{}
	for _, r := range reviews {
		if r.ID <= 0 || r.User.ID <= 0 || seen[r.ID] {
			return deny()
		}
		seen[r.ID] = true
		switch r.State {
		case "APPROVED", "CHANGES_REQUESTED":
			// Approval on another commit cannot clear an active request for
			// changes on the publication currently under consideration.
			if r.State == "APPROVED" && r.CommitID != e.HeadSHA {
				continue
			}
			if old, exists := latest[r.User.ID]; !exists || old.ID < r.ID {
				latest[r.User.ID] = r
			}
		case "COMMENTED", "DISMISSED":
			// A comment is not resolution of a prior changes-requested review. A
			// dismissed entry no longer requests changes; other active requests stand.
		case "PENDING":
			return deny()
		default:
			return deny()
		}
	}
	state := reviewState{}
	for _, r := range latest {
		if r.State == "CHANGES_REQUESTED" {
			state.ChangesRequested = true
		}
	}
	raw, _ := json.Marshal(map[string]any{"query": threadQuery, "variables": map[string]any{"number": e.PullRequest, "owner": strings.Split(c.repository, "/")[0], "name": strings.Split(c.repository, "/")[1]}})
	var response struct {
		Errors []json.RawMessage
		Data   *struct {
			Repository *struct {
				Name string `json:"nameWithOwner"`
				Pull *struct {
					Number  int
					Head    string `json:"headRefOid"`
					Base    string `json:"baseRefOid"`
					Closing *struct {
						Total *int `json:"totalCount"`
						Page  *struct {
							Next *bool `json:"hasNextPage"`
						} `json:"pageInfo"`
						Nodes []struct{ ID string }
					} `json:"closingIssuesReferences"`
					Threads *struct {
						Total *int `json:"totalCount"`
						Page  *struct {
							Next *bool `json:"hasNextPage"`
						} `json:"pageInfo"`
						Nodes []struct {
							ID       string
							Resolved *bool `json:"isResolved"`
						}
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			}
		}
	}
	if c.read(ctx, "/graphql", raw, &response) != nil || len(response.Errors) != 0 || response.Data == nil || response.Data.Repository == nil || response.Data.Repository.Name != c.repository || response.Data.Repository.Pull == nil {
		return deny()
	}
	p := response.Data.Repository.Pull
	if p.Number != e.PullRequest || p.Head != e.HeadSHA || p.Base != e.BaseSHA || p.Threads == nil {
		return deny()
	}
	// Saferus closes issues only after its acknowledged production release. The
	// GraphQL connection includes manually linked issues, not just body keywords.
	// Missing, truncated or nonempty data cannot authorize a premature close.
	if c.repository == "os-santiago/saferus" && (p.Closing == nil || p.Closing.Total == nil || *p.Closing.Total != 0 || p.Closing.Page == nil || p.Closing.Page.Next == nil || *p.Closing.Page.Next || p.Closing.Nodes == nil || len(p.Closing.Nodes) != 0) {
		return deny()
	}
	threads := p.Threads
	if threads.Total == nil || *threads.Total < 0 || *threads.Total > 100 || threads.Page == nil || threads.Page.Next == nil || *threads.Page.Next || threads.Nodes == nil || len(threads.Nodes) != *threads.Total {
		return deny()
	}
	threadIDs := map[string]bool{}
	for _, thread := range threads.Nodes {
		if thread.ID == "" || len(thread.ID) > 256 || threadIDs[thread.ID] || thread.Resolved == nil {
			return deny()
		}
		threadIDs[thread.ID] = true
		if !*thread.Resolved {
			state.UnresolvedThreads++
		}
	}
	return state, digest(struct {
		Reviews []review
		Threads any
		Closing any
	}{reviews, threads, p.Closing}), nil
}
