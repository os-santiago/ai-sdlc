package mergegate

import (
	"context"
	"encoding/json"
	"strings"
)

type rule struct {
	Type       string          `json:"type"`
	Parameters json.RawMessage `json:"parameters"`
	RulesetID  int             `json:"ruleset_id"`
	Source     string          `json:"ruleset_source"`
	SourceType string          `json:"ruleset_source_type"`
}
type ruleset struct {
	ID                                int
	Target, Enforcement, Source, Name string
	SourceType                        string             `json:"source_type"`
	Bypass                            *[]json.RawMessage `json:"bypass_actors"`
	Conditions                        struct {
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
	if (p.Name != "" && set.Name != p.Name) || set.ID != p.RulesetID || set.Target != "branch" || set.Enforcement != "active" || set.Source != repository || set.SourceType != "Repository" || set.Bypass == nil || set.Conditions.Ref.Exclude == nil || len(set.Conditions.Ref.Exclude) != 0 || len(set.Conditions.Ref.Include) == 0 || len(set.Rules) == 0 || len(set.Rules) > 32 || len(effective) == 0 || len(effective) > 32 {
		return "", nil, false, ErrGate
	}
	applies := false
	for _, ref := range set.Conditions.Ref.Include {
		if ref == "refs/heads/main" || ref == "~DEFAULT_BRANCH" {
			applies = true
		}
	}
	if !applies || (p.Name != "" && (len(set.Conditions.Ref.Include) != 1 || set.Conditions.Ref.Include[0] != "refs/heads/main")) {
		return "", nil, false, ErrGate
	}
	safe := len(*set.Bypass) == 0
	required := []RequiredCheck{}
	seen := map[string]int64{}
	ownChecks := map[string]int64{}
	hasChecks, hasReview := false, false
	for _, r := range effective {
		own := r.RulesetID == p.RulesetID
		if r.RulesetID <= 0 || (!own && p.Name == "") ||
			!((r.SourceType == "Repository" && r.Source == repository) || (!own && r.SourceType == "Organization" && r.Source == strings.Split(repository, "/")[0])) {
			return "", nil, false, ErrGate
		}
		// The dedicated floor must match its configured rules. Other authenticated
		// effective rules can only add native requirements and remain in the digest.
		match := false
		for _, configured := range set.Rules {
			if configured.Type == r.Type && parameterDigest(configured.Parameters) == parameterDigest(r.Parameters) {
				match = true
			}
		}
		if own && !match {
			return "", nil, false, ErrGate
		}
		switch r.Type {
		case "required_status_checks":
			if own && hasChecks {
				return "", nil, false, ErrGate
			}
			if own {
				hasChecks = true
			}
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
			if own {
				safe = safe && *value.Strict
			}
			local := map[string]bool{}
			for _, check := range value.Checks {
				if check.Context == "" || len(check.Context) > 256 || check.AppID <= 0 || local[check.Context] || (seen[check.Context] != 0 && seen[check.Context] != check.AppID) {
					return "", nil, false, ErrGate
				}
				local[check.Context] = true
				if seen[check.Context] == 0 {
					required = append(required, RequiredCheck{check.Context, check.AppID})
				}
				seen[check.Context] = check.AppID
				if own {
					ownChecks[check.Context] = check.AppID
				}
			}
		case "pull_request":
			if own && hasReview {
				return "", nil, false, ErrGate
			}
			if own {
				hasReview = true
			}
			var value struct {
				Resolved *bool `json:"required_review_thread_resolution"`
				Count    *int  `json:"required_approving_review_count"`
			}
			if json.Unmarshal(r.Parameters, &value) != nil || value.Resolved == nil || value.Count == nil || *value.Count < 0 {
				return "", nil, false, ErrGate
			}
			if own {
				safe = safe && *value.Resolved
			}
		}
	}
	if !hasChecks || !hasReview || len(required) > 16 {
		return "", nil, false, ErrGate
	}
	for _, floor := range p.Required {
		if ownChecks[floor.Context] != floor.AppID {
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
