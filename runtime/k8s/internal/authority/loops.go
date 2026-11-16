package authority

import "errors"

// OuterOperation is a closed routing decision. Payloads, issue text and URLs
// cannot select a policy. Unknown operations are never treated as outerloop.
func OuterOperation(operation string) bool {
	switch operation {
	case "build-merged", "verify-built", "promote-artifact", "deploy-release", "record-deployment":
		return true
	}
	return false
}

// ValidateLoopPolicies checks separately mounted reviewed capabilities. Both
// policies share custody identities, but neither may grant the other's effects.
// Freeze is checked by callers at every effect boundary, not normalized here.
func ValidateLoopPolicies(inner, outer Policy) error {
	denied := errors.New("invalid separated loop policies")
	if inner.Validate() != nil || outer.Validate() != nil ||
		inner.Repository != "os-santiago/saferus" || inner.Environment != "ai-sdlc-production" ||
		outer.Repository != inner.Repository || outer.Environment != inner.Environment ||
		inner.Autonomy != "auto-merge-low" || outer.Autonomy != "auto-deploy" {
		return denied
	}
	for _, g := range inner.Grants {
		switch g.Operation {
		case "admit-issue", "plan-admission", "execute-work", "verify-artifact", "publish-candidate", "merge-publication":
		default:
			return denied
		}
	}
	for _, g := range outer.Grants {
		if !OuterOperation(g.Operation) {
			return denied
		}
	}
	// Shared outer keys must match installation-owned inner keys. The builder
	// exists only in outer policy; adding it must not invalidate active inner work.
	// Keeping a rotated or foreign key in either mount fails closed.
	roles := map[string]bool{}
	for id, key := range outer.Keys {
		if key.Role != "build-controller" && inner.Keys[id] != key {
			return denied
		}
		switch key.Role {
		case "release-manager", "build-controller", "verifier", "deployer", "relay":
			roles[key.Role] = true
		default:
			return denied
		}
	}
	for _, role := range []string{"release-manager", "build-controller", "verifier", "deployer", "relay"} {
		if !roles[role] {
			return denied
		}
	}
	for id, key := range inner.Keys {
		if key.Role == "build-controller" || (roles[key.Role] && outer.Keys[id] != key) {
			return denied
		}
	}
	return nil
}
