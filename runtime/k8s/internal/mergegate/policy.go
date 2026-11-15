package mergegate

import (
	"context"
	"encoding/json"
)

type rule struct {
	Type       string          `json:"type"`
	Parameters json.RawMessage `json:"parameters"`
	RulesetID  int             `json:"ruleset_id"`
	Source     string          `json:"ruleset_source"`
	SourceType string          `json:"ruleset_source_type"`
}
type ruleset struct {
	ID                          int
	Target, Enforcement, Source string
	SourceType                  string             `json:"source_type"`
	Bypass                      *[]json.RawMessage `json:"bypass_actors"`
	Conditions                  struct {
		Ref struct{ Include, Exclude []string } `json:"ref_name"`
	}
	Rules []rule
}

func (c *Client) nativePolicy(ctx context.Context, p Policy) (string, []RequiredCheck, bool, error) {
	var set ruleset
	var effective []rule
	if c.read(ctx, repoPath("/rulesets/"+number(p.RulesetID)), nil, &set) != nil || c.read(ctx, repoPath("/rules/branches/main"), nil, &effective) != nil {
		return "", nil, false, ErrGate
	}
	if set.ID != p.RulesetID || set.Target != "branch" || set.Enforcement != "active" || set.Source != repository || set.SourceType != "Repository" || set.Bypass == nil || set.Conditions.Ref.Exclude == nil || len(set.Conditions.Ref.Exclude) != 0 || len(set.Conditions.Ref.Include) == 0 || len(set.Rules) == 0 || len(set.Rules) > 32 || len(effective) == 0 || len(effective) > 32 {
		return "", nil, false, ErrGate
	}
	applies := false
	for _, ref := range set.Conditions.Ref.Include {
		if ref == "refs/heads/main" || ref == "~DEFAULT_BRANCH" {
			applies = true
		}
	}
	if !applies {
		return "", nil, false, ErrGate
	}
	safe := len(*set.Bypass) == 0
	required := []RequiredCheck{}
	seen := map[string]bool{}
	hasChecks, hasReview := false, false
	for _, r := range effective {
		if r.RulesetID != p.RulesetID || r.Source != repository || r.SourceType != "Repository" {
			return "", nil, false, ErrGate
		}
		// Every effective rule must be present in the explicitly reviewed ruleset.
		match := false
		for _, configured := range set.Rules {
			if configured.Type == r.Type && parameterDigest(configured.Parameters) == parameterDigest(r.Parameters) {
				match = true
			}
		}
		if !match {
			return "", nil, false, ErrGate
		}
		switch r.Type {
		case "required_status_checks":
			if hasChecks {
				return "", nil, false, ErrGate
			}
			hasChecks = true
			var value struct {
				Strict *bool `json:"strict_required_status_checks_policy"`
				Checks []struct {
					Context string
					AppID   int64 `json:"integration_id"`
				} `json:"required_status_checks"`
			}
			if json.Unmarshal(r.Parameters, &value) != nil || value.Strict == nil || len(value.Checks) < 1 || len(value.Checks) > 16 {
				return "", nil, false, ErrGate
			}
			safe = safe && *value.Strict
			for _, check := range value.Checks {
				if check.Context == "" || len(check.Context) > 256 || check.AppID <= 0 || seen[check.Context] {
					return "", nil, false, ErrGate
				}
				seen[check.Context] = true
				required = append(required, RequiredCheck{check.Context, check.AppID})
			}
		case "pull_request":
			if hasReview {
				return "", nil, false, ErrGate
			}
			hasReview = true
			var value struct {
				Resolved *bool `json:"required_review_thread_resolution"`
				Count    *int  `json:"required_approving_review_count"`
			}
			if json.Unmarshal(r.Parameters, &value) != nil || value.Resolved == nil || value.Count == nil || *value.Count < 0 {
				return "", nil, false, ErrGate
			}
			safe = safe && *value.Resolved
		}
	}
	if !hasChecks || !hasReview {
		return "", nil, false, ErrGate
	}
	for _, floor := range p.Required {
		found := false
		for _, native := range required {
			if native == floor {
				found = true
			}
		}
		if !found {
			safe = false
		}
	}
	return digest(struct {
		Configured Policy
		Native     ruleset
		Effective  []rule
	}{p, set, effective}), required, safe, nil
}

func parameterDigest(raw json.RawMessage) string {
	var value any
	if len(raw) == 0 {
		return digest(nil)
	}
	if json.Unmarshal(raw, &value) != nil {
		return "invalid"
	}
	return digest(value)
}
