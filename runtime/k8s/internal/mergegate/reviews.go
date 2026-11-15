package mergegate

import (
	"context"
	"encoding/json"
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

const threadQuery = `query($number:Int!){repository(owner:"os-santiago",name:"homedir"){nameWithOwner pullRequest(number:$number){number headRefOid baseRefOid reviewThreads(first:100){totalCount pageInfo{hasNextPage} nodes{id isResolved}}}}}`

func (c *Client) reviews(ctx context.Context, e Expected) (bool, string, error) {
	var reviews []review
	if c.read(ctx, repoPath("/pulls/"+number(e.PullRequest)+"/reviews?per_page=100"), nil, &reviews) != nil || reviews == nil || len(reviews) > 100 {
		return false, "", ErrGate
	}
	latest := map[int64]review{}
	seen := map[int64]bool{}
	for _, r := range reviews {
		if r.ID <= 0 || r.User.ID <= 0 || seen[r.ID] {
			return false, "", ErrGate
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
			return false, "", ErrGate
		default:
			return false, "", ErrGate
		}
	}
	ready := true
	for _, r := range latest {
		if r.State == "CHANGES_REQUESTED" {
			ready = false
		}
	}
	raw, _ := json.Marshal(map[string]any{"query": threadQuery, "variables": map[string]int{"number": e.PullRequest}})
	var response struct {
		Errors []json.RawMessage
		Data   *struct {
			Repository *struct {
				Name string `json:"nameWithOwner"`
				Pull *struct {
					Number  int
					Head    string `json:"headRefOid"`
					Base    string `json:"baseRefOid"`
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
	if c.read(ctx, "/graphql", raw, &response) != nil || len(response.Errors) != 0 || response.Data == nil || response.Data.Repository == nil || response.Data.Repository.Name != repository || response.Data.Repository.Pull == nil {
		return false, "", ErrGate
	}
	p := response.Data.Repository.Pull
	if p.Number != e.PullRequest || p.Head != e.HeadSHA || p.Base != e.BaseSHA || p.Threads == nil {
		return false, "", ErrGate
	}
	threads := p.Threads
	if threads.Total == nil || *threads.Total < 0 || *threads.Total > 100 || threads.Page == nil || threads.Page.Next == nil || *threads.Page.Next || threads.Nodes == nil || len(threads.Nodes) != *threads.Total {
		return false, "", ErrGate
	}
	threadIDs := map[string]bool{}
	for _, thread := range threads.Nodes {
		if thread.ID == "" || len(thread.ID) > 256 || threadIDs[thread.ID] || thread.Resolved == nil {
			return false, "", ErrGate
		}
		threadIDs[thread.ID] = true
		if !*thread.Resolved {
			ready = false
		}
	}
	return ready, digest(struct {
		Reviews []review
		Threads any
	}{reviews, threads}), nil
}
