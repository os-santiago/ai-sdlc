package mergegate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/forge"
)

func fixtureExpected() Expected {
	return Expected{repository, 42, strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40), "ai-sdlc/issue-3-" + strings.Repeat("d", 32)}
}
func fixtureCredential() forge.Credential {
	return forge.Credential{Version: 1, Mode: "gh-cli", Host: "github.com", Repository: repository, ActorID: 42, ActorLogin: "fixture", Token: strings.Repeat("x", 40)}
}

const repository = "os-santiago/homedir"

func repoPath(suffix string) string { return "/repos/" + repository + suffix }

func fixtureRules() []map[string]any { return fixtureRulesFor(PilotPolicy()) }
func fixtureRulesFor(policy Policy) []map[string]any {
	required := []map[string]any{}
	for _, r := range policy.Required {
		required = append(required, map[string]any{"context": r.Context, "integration_id": r.AppID})
	}
	return []map[string]any{
		{"type": "required_status_checks", "parameters": map[string]any{"strict_required_status_checks_policy": true, "required_status_checks": required}},
		{"type": "pull_request", "parameters": map[string]any{"required_review_thread_resolution": true, "required_approving_review_count": 0}},
	}
}
func fixtureAPI(t *testing.T, mode string) (*Client, *atomic.Int32) {
	return fixtureAPIForRepository(t, mode, repository)
}
func fixtureAPIForRepository(t *testing.T, mode, repository string) (*Client, *atomic.Int32) {
	policy := PolicyForRepository(repository, "")
	repoPath := func(suffix string) string { return "/repos/" + repository + suffix }
	t.Helper()
	calls := new(atomic.Int32)
	pulls := new(atomic.Int32)
	sets := new(atomic.Int32)
	checkReads := new(atomic.Int32)
	statusReads := new(atomic.Int32)
	reviewReads := new(atomic.Int32)
	threadReads := new(atomic.Int32)
	expected := fixtureExpected()
	expected.Repository = repository
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+fixtureCredential().Token {
			t.Error("missing private API credential")
		}
		if mode == "redirect" {
			w.Header().Set("Location", "https://untrusted.invalid")
			w.WriteHeader(302)
			return
		}
		if mode == "forbidden" {
			w.WriteHeader(403)
			w.Write([]byte("secret response must not escape"))
			return
		}
		if mode == "duplicate" {
			w.Write([]byte(`{"number":42,"number":43}`))
			return
		}
		if mode == "oversize" {
			w.Write([]byte(strings.Repeat("x", 2*1024*1024+1)))
			return
		}
		if mode == "pagination" {
			w.Header().Set("Link", `<https://api.github.com/next>; rel="next"`)
		}
		var out any
		switch r.URL.Path {
		case repoPath("/pulls/42"):
			if r.Method != "GET" {
				t.Error("unexpected write")
			}
			head, base := expected.HeadSHA, expected.BaseSHA
			if mode == "changed-head" || mode == "late-head" && pulls.Load() > 0 {
				head = strings.Repeat("e", 40)
			}
			if mode == "changed-base" || mode == "late-base" && pulls.Load() > 0 {
				base = strings.Repeat("e", 40)
			}
			pulls.Add(1)
			out = map[string]any{"number": 42, "state": "open", "draft": false, "merged": false, "mergeable": true, "mergeable_state": "clean", "head": map[string]any{"ref": expected.Branch, "sha": head, "repo": map[string]any{"full_name": repository}}, "base": map[string]any{"ref": "main", "sha": base, "repo": map[string]any{"full_name": repository, "default_branch": "main"}}}
		case repoPath("/git/commits/" + expected.HeadSHA):
			tree := expected.TreeSHA
			if mode == "wrong-tree" {
				tree = strings.Repeat("e", 40)
			}
			out = map[string]any{"sha": expected.HeadSHA, "tree": map[string]string{"sha": tree}, "parents": []map[string]string{{"sha": expected.BaseSHA}}}
		case repoPath("/rulesets/" + strconv.Itoa(policy.RulesetID)), repoPath("/rules/branches/main"):
			rules := fixtureRulesFor(policy)
			if mode == "strict-off" {
				rules[0]["parameters"].(map[string]any)["strict_required_status_checks_policy"] = false
			}
			if mode == "threads-rule-off" {
				rules[1]["parameters"].(map[string]any)["required_review_thread_resolution"] = false
			}
			if r.URL.Path == repoPath("/rulesets/"+strconv.Itoa(policy.RulesetID)) {
				bypass := []any{}
				if mode == "bypass" || mode == "late-protection" && sets.Load() > 0 {
					bypass = append(bypass, map[string]any{"actor_id": 5, "actor_type": "RepositoryRole", "bypass_mode": "pull_request"})
				}
				sets.Add(1)
				out = map[string]any{"id": policy.RulesetID, "target": "branch", "enforcement": "active", "source": repository, "source_type": "Repository", "bypass_actors": bypass, "conditions": map[string]any{"ref_name": map[string]any{"include": []string{"~DEFAULT_BRANCH"}, "exclude": []string{}}}, "rules": rules}
			} else {
				for _, rule := range rules {
					rule["ruleset_id"] = policy.RulesetID
					rule["ruleset_source"] = repository
					rule["ruleset_source_type"] = "Repository"
				}
				out = rules
			}
		case repoPath("/commits/" + expected.HeadSHA + "/check-runs"):
			if r.URL.Query().Get("filter") != "latest" || r.URL.Query().Get("per_page") != "100" {
				t.Error("unbounded checks")
			}
			runs := []map[string]any{}
			for i, check := range policy.Required {
				runs = append(runs, map[string]any{"id": i + 1, "name": check.Context, "head_sha": expected.HeadSHA, "app": map[string]any{"id": check.AppID}, "status": "completed", "conclusion": "success"})
			}
			switch mode {
			case "wrong-app":
				runs[0]["app"] = map[string]any{"id": 1}
			case "failed-check":
				runs[0]["conclusion"] = "failure"
			case "pending-check":
				runs[0]["status"] = "in_progress"
			case "skipped-required":
				runs[0]["conclusion"] = "skipped"
			case "missing-check":
				runs = runs[:len(runs)-1]
			case "cross-app-check":
				runs[1]["name"] = runs[0]["name"]
				runs[1]["app"] = map[string]any{"id": 99}
			case "duplicate-required", "duplicate-required-pending", "duplicate-required-failure":
				copy := map[string]any{}
				for k, v := range runs[0] {
					copy[k] = v
				}
				copy["id"] = 20
				if mode == "duplicate-required-pending" {
					copy["status"] = "in_progress"
				}
				if mode == "duplicate-required-failure" {
					copy["conclusion"] = "failure"
				}
				runs = append(runs, copy)
			case "duplicate-optional", "duplicate-optional-pending", "duplicate-optional-failure":
				for i := 0; i < 2; i++ {
					runs = append(runs, map[string]any{"id": 20 + i, "name": "optional", "head_sha": expected.HeadSHA, "app": map[string]any{"id": 15368}, "status": "completed", "conclusion": "success"})
				}
				if mode == "duplicate-optional-pending" {
					runs[len(runs)-1]["status"] = "in_progress"
				}
				if mode == "duplicate-optional-failure" {
					runs[len(runs)-1]["conclusion"] = "failure"
				}
			case "wrong-check-head":
				runs[0]["head_sha"] = strings.Repeat("e", 40)
			case "failed-optional":
				runs = append(runs, map[string]any{"id": 10, "name": "optional", "head_sha": expected.HeadSHA, "app": map[string]any{"id": 15368}, "status": "completed", "conclusion": "failure"})
			}
			if mode == "late-check" && checkReads.Load() > 0 {
				runs[0]["conclusion"] = "failure"
			}
			checkReads.Add(1)
			out = map[string]any{"total_count": len(runs), "check_runs": runs}
		case repoPath("/commits/" + expected.HeadSHA + "/status"):
			state := "success"
			if mode == "failed-status" || mode == "late-status" && statusReads.Load() > 0 {
				state = "failure"
			}
			statusReads.Add(1)
			out = map[string]any{"sha": expected.HeadSHA, "state": state, "total_count": 1, "statuses": []map[string]any{{"id": 1, "context": "external-check", "state": state}}}
		case repoPath("/pulls/42/reviews"):
			list := []any{}
			review := func(id int, state, head string) map[string]any {
				return map[string]any{"id": id, "state": state, "commit_id": head, "user": map[string]any{"id": 7, "type": "User"}}
			}
			switch mode {
			case "changes-requested", "comment-after-request", "stale-approval", "resolved-request":
				list = append(list, review(1, "CHANGES_REQUESTED", expected.HeadSHA))
				if mode == "comment-after-request" {
					list = append(list, review(2, "COMMENTED", expected.HeadSHA))
				}
				if mode == "stale-approval" {
					list = append(list, review(2, "APPROVED", expected.BaseSHA))
				}
				if mode == "resolved-request" {
					list = append(list, review(2, "APPROVED", expected.HeadSHA))
				}
			}
			if mode == "late-review" && reviewReads.Load() > 0 {
				list = append(list, review(1, "CHANGES_REQUESTED", expected.HeadSHA))
			}
			reviewReads.Add(1)
			out = list
		case "/graphql":
			if r.Method != "POST" {
				t.Error("query method")
			}
			var query struct {
				Query     string
				Variables struct {
					Number      int
					Owner, Name string
				}
			}
			if json.NewDecoder(r.Body).Decode(&query) != nil || query.Query != threadQuery || query.Variables.Number != 42 || query.Variables.Owner != "os-santiago" || query.Variables.Name != strings.Split(repository, "/")[1] {
				t.Error("caller controlled query")
			}
			out = map[string]any{"data": map[string]any{"repository": map[string]any{"nameWithOwner": repository, "pullRequest": map[string]any{"number": 42, "headRefOid": expected.HeadSHA, "baseRefOid": expected.BaseSHA, "closingIssuesReferences": map[string]any{"totalCount": 0, "pageInfo": map[string]bool{"hasNextPage": false}, "nodes": []map[string]string{}}, "reviewThreads": map[string]any{"totalCount": 1, "pageInfo": map[string]any{"hasNextPage": mode == "truncated-threads"}, "nodes": []map[string]any{{"id": "thread1", "isResolved": mode != "unresolved-thread" && !(mode == "late-thread" && threadReads.Load() > 0)}}}}}}}
			pull := out.(map[string]any)["data"].(map[string]any)["repository"].(map[string]any)["pullRequest"].(map[string]any)
			if mode == "closing-issue" || mode == "late-closing-issue" && threadReads.Load() > 0 {
				pull["closingIssuesReferences"] = map[string]any{"totalCount": 1, "pageInfo": map[string]bool{"hasNextPage": false}, "nodes": []map[string]string{{"id": "linked-issue"}}}
			}
			if mode == "missing-closing-issues" {
				delete(pull, "closingIssuesReferences")
			}
			if mode == "truncated-closing-issues" {
				pull["closingIssuesReferences"] = map[string]any{"totalCount": 0, "pageInfo": map[string]bool{"hasNextPage": true}, "nodes": []map[string]string{}}
			}
			threadReads.Add(1)
			if mode == "graphql-error" {
				out = map[string]any{"errors": []map[string]string{{"message": "private diagnostic"}}}
			}
		default:
			t.Error("unexpected API path", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(server.Close)
	credential := fixtureCredential()
	credential.Repository = repository
	c, e := New(credential)
	if e != nil {
		t.Fatal(e)
	}
	c.base = server.URL
	c.http.Transport.(*http.Transport).TLSClientConfig.RootCAs = server.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	return c, calls
}
func TestAuthenticatedGateCompleteBoundEvidence(t *testing.T) {
	for _, mode := range []string{"success", "resolved-request", "duplicate-required", "duplicate-optional"} {
		t.Run(mode, func(t *testing.T) {
			c, calls := fixtureAPI(t, mode)
			result, e := c.Collect(context.Background(), fixtureExpected(), PilotPolicy())
			if e != nil || !result.Ready || result.MergeAuthorized || len(result.Reasons) != 0 || calls.Load() != 15 || result.Expected != fixtureExpected() || result.ObservedAt.IsZero() {
				t.Fatal("valid observation", result, e, calls.Load())
			}
		})
	}
}
func TestAuthenticatedGateBlocksIncompleteOrUnsafeEvidence(t *testing.T) {
	for _, mode := range []string{"bypass", "strict-off", "threads-rule-off", "wrong-app", "failed-check", "pending-check", "skipped-required", "missing-check", "failed-optional", "failed-status", "changes-requested", "comment-after-request", "stale-approval", "unresolved-thread", "duplicate-required-pending", "duplicate-required-failure", "duplicate-optional-pending", "duplicate-optional-failure"} {
		t.Run(mode, func(t *testing.T) {
			c, _ := fixtureAPI(t, mode)
			r, e := c.Collect(context.Background(), fixtureExpected(), PilotPolicy())
			if e != nil || r.Ready || len(r.Reasons) == 0 {
				t.Fatal("unsafe observation", r, e)
			}
		})
	}
}

// Each review blocker is its own reason: an unresolved thread is not a request
// for changes, and the thread count travels with the observation.
func TestAuthenticatedGateNamesEachReviewBlocker(t *testing.T) {
	for mode, want := range map[string]struct {
		reason  string
		threads int
	}{
		"unresolved-thread": {ReasonThreadsUnresolved, 1},
		"changes-requested": {ReasonChangesRequested, 0},
	} {
		t.Run(mode, func(t *testing.T) {
			c, _ := fixtureAPI(t, mode)
			r, e := c.Collect(context.Background(), fixtureExpected(), PilotPolicy())
			if e != nil || r.Ready || len(r.Reasons) != 1 || r.Reasons[0] != want.reason || r.UnresolvedThreads != want.threads {
				t.Fatal("review blocker", r, e)
			}
		})
	}
	c, _ := fixtureAPI(t, "success")
	r, e := c.Collect(context.Background(), fixtureExpected(), PilotPolicy())
	raw, _ := json.Marshal(r)
	if e != nil || strings.Contains(string(raw), "unresolvedThreads") {
		t.Fatal("a ready gate changed shape", string(raw), e)
	}
	known := map[string]bool{}
	for _, reason := range Reasons {
		known[reason] = true
	}
	if len(known) != 5 || !known[ReasonThreadsUnresolved] || !known[ReasonChangesRequested] {
		t.Fatal("reason set", Reasons)
	}
}
func TestAuthenticatedGateRejectsAmbiguityChangesAndTransport(t *testing.T) {
	for _, mode := range []string{"changed-head", "changed-base", "late-head", "late-base", "late-protection", "wrong-tree", "cross-app-check", "late-check", "late-status", "late-review", "late-thread", "wrong-check-head", "truncated-threads", "graphql-error", "pagination", "redirect", "forbidden", "duplicate", "oversize"} {
		t.Run(mode, func(t *testing.T) {
			c, _ := fixtureAPI(t, mode)
			r, e := c.Collect(context.Background(), fixtureExpected(), PilotPolicy())
			if e != ErrGate || r.Ready {
				t.Fatal("untrusted observation accepted", r, e)
			}
		})
	}
}
func TestGateCannotWriteAndHonorsCancellation(t *testing.T) {
	c, calls := fixtureAPI(t, "success")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := c.Collect(ctx, fixtureExpected(), PilotPolicy()); e != ErrGate || calls.Load() != 0 {
		t.Fatal("canceled query reached API")
	}
	wrong := fixtureExpected()
	wrong.Repository = "os-santiago/saferus"
	if _, e := c.Collect(context.Background(), wrong, PilotPolicy()); e != ErrGate || calls.Load() != 0 {
		t.Fatal("cross-repository evidence reached API")
	}
	p := PilotPolicy()
	p.Required = nil
	if _, e := c.Collect(context.Background(), fixtureExpected(), p); e != ErrGate || calls.Load() != 0 {
		t.Fatal("empty configured policy accepted")
	}
	credential := fixtureCredential()
	credential.Repository = "other/repo"
	if _, e := New(credential); e != ErrGate {
		t.Fatal("wrong repository accepted")
	}
}

func TestSaferusPolicyAndCredentialScope(t *testing.T) {
	for _, mode := range []string{"success", "wrong-app", "failed-check", "unresolved-thread", "changes-requested", "missing-check", "closing-issue", "late-closing-issue", "missing-closing-issues", "truncated-closing-issues"} {
		t.Run(mode, func(t *testing.T) {
			c, calls := fixtureAPIForRepository(t, mode, "os-santiago/saferus")
			expected := fixtureExpected()
			expected.Repository = "os-santiago/saferus"
			r, err := c.Collect(context.Background(), expected, PolicyForRepository("os-santiago/saferus", ""))
			if (err != nil) != strings.Contains(mode, "closing-issue") || r.Ready != (mode == "success") || calls.Load() == 0 {
				t.Fatal(r, err)
			}
			before := calls.Load()
			if _, err := c.Collect(context.Background(), fixtureExpected(), PolicyForRepository("os-santiago/saferus", "")); err != ErrGate || calls.Load() != before {
				t.Fatal("cross-repository evidence reached API")
			}
			if _, err := c.Collect(context.Background(), fixtureExpected(), PilotPolicy()); err != ErrGate || calls.Load() != before {
				t.Fatal("cross-repository policy reached API")
			}
		})
	}
}
