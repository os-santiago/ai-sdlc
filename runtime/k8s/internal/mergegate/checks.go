package mergegate

import "context"

type checkRun struct {
	ID                       int64
	Name, Status, Conclusion string
	HeadSHA                  string `json:"head_sha"`
	App                      struct{ ID int64 }
}

func (c *Client) checks(ctx context.Context, head string, required []RequiredCheck) (bool, string, error) {
	var runs struct {
		Total *int       `json:"total_count"`
		Runs  []checkRun `json:"check_runs"`
	}
	if c.read(ctx, repoPath("/commits/"+head+"/check-runs?filter=latest&per_page=100"), nil, &runs) != nil || runs.Total == nil || *runs.Total < 0 || *runs.Total > 100 || len(runs.Runs) != *runs.Total || runs.Runs == nil {
		return false, "", ErrGate
	}
	ready := true
	names := map[string][]checkRun{}
	ids := map[int64]bool{}
	for _, r := range runs.Runs {
		if r.ID <= 0 || r.Name == "" || len(r.Name) > 256 || r.HeadSHA != head || r.App.ID <= 0 || ids[r.ID] {
			return false, "", ErrGate
		}
		if existing := names[r.Name]; len(existing) > 0 && existing[0].App.ID != r.App.ID {
			return false, "", ErrGate
		}
		names[r.Name] = append(names[r.Name], r)
		ids[r.ID] = true
		if r.Status != "completed" || (r.Conclusion != "success" && r.Conclusion != "neutral" && r.Conclusion != "skipped") {
			ready = false
		}
	}
	for _, expected := range required {
		group := names[expected.Context]
		if len(group) == 0 {
			ready = false
		}
		for _, r := range group {
			if r.App.ID != expected.AppID || r.Status != "completed" || r.Conclusion != "success" {
				ready = false
			}
		}
	}
	var statuses struct {
		SHA, State string
		Total      *int `json:"total_count"`
		Statuses   []struct {
			ID             int64
			Context, State string
		}
	}
	if c.read(ctx, repoPath("/commits/"+head+"/status?per_page=100"), nil, &statuses) != nil || statuses.SHA != head || statuses.Total == nil || *statuses.Total < 0 || *statuses.Total > 100 || len(statuses.Statuses) != *statuses.Total || statuses.Statuses == nil {
		return false, "", ErrGate
	}
	contexts := map[string]bool{}
	for _, s := range statuses.Statuses {
		if s.ID <= 0 || s.Context == "" || len(s.Context) > 256 || contexts[s.Context] {
			return false, "", ErrGate
		}
		contexts[s.Context] = true
		if s.State != "success" {
			ready = false
		}
	}
	if len(statuses.Statuses) > 0 && statuses.State != "success" {
		ready = false
	}
	return ready, digest(struct {
		Runs     []checkRun
		Statuses any
	}{runs.Runs, statuses}), nil
}
