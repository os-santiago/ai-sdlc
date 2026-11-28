package execution

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/executor"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/generatortools"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelmailbox"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/podtransport"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/project"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/tektontasks"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/workspacebundle"
)

type fakeToolSession struct {
	requests   []modelmailbox.Envelope
	published  []modelmailbox.Envelope
	failRead   bool
	notPending bool
}

func (f *fakeToolSession) ReadToolRequest(context.Context) (modelmailbox.Envelope, error) {
	if f.failRead {
		return modelmailbox.Envelope{}, errors.New("transport")
	}
	if len(f.requests) == 0 {
		return modelmailbox.Envelope{}, modelmailbox.ErrPending
	}
	r := f.requests[0]
	f.requests = f.requests[1:]
	return r, nil
}

func (f *fakeToolSession) PublishToolResponse(_ context.Context, e modelmailbox.Envelope) error {
	if f.notPending {
		return podtransport.ErrToolNotPending
	}
	f.published = append(f.published, e)
	return nil
}

func toolRequest(sequence uint64, payload string) modelmailbox.Envelope {
	return modelmailbox.Envelope{Version: modelmailbox.Version, Kind: "tool-request", Attempt: "11111111-1111-4111-8111-111111111111-1",
		PodUID: "22222222-2222-4222-8222-222222222222", Sequence: sequence, RequestSHA256: modelmailbox.Digest([]byte(payload)),
		ExpiresAt: time.Now().Add(time.Minute).UnixMilli(), Payload: []byte(payload)}
}

// Each tool call runs once and is answered with its own binding; a repeated
// observation of the answered call is not run again; refusals map to fixed
// statuses without forwarding the cause; a skipped or rewritten sequence and
// a failed read stop the run.
func TestToolDispatchRunsEachCallOnceInOrder(t *testing.T) {
	first := toolRequest(1, `{"tool":"build_test","target":"rest-villains","diff":""}`)
	session := &fakeToolSession{requests: []modelmailbox.Envelope{first, first, toolRequest(2, `{"tool":"build_test","target":"x","diff":""}`)}}
	runs := 0
	d := &toolDispatch{session: session, run: func(_ context.Context, call tektontasks.ToolCall) (tektontasks.ToolResult, error) {
		runs++
		if call.Target == "x" {
			return tektontasks.ToolResult{}, errors.New("secret cluster detail")
		}
		return tektontasks.ToolResult{Tool: call.Tool, Target: call.Target, Passed: true, Summary: json.RawMessage(`{"tests":94}`)}, nil
	}}
	for range 4 {
		if err := d.poll(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if runs != 2 || len(session.published) != 2 {
		t.Fatal("runs", runs, len(session.published))
	}
	ok, failed := session.published[0], session.published[1]
	if ok.Status != 200 || ok.Sequence != 1 || ok.RequestSHA256 != first.RequestSHA256 || ok.Kind != "tool-response" || !strings.Contains(string(ok.Payload), "Untrusted tool output") {
		t.Fatal("answer", ok.Status, string(ok.Payload))
	}
	if failed.Status != 500 || strings.Contains(string(failed.Payload), "secret") {
		t.Fatal("failure leaked or misclassified", failed.Status, string(failed.Payload))
	}
	if status, text := toolFailure(tektontasks.ErrToolBusy); status != 429 || !strings.Contains(text, "still running") {
		t.Fatal("busy", status, text)
	}
	for err, status := range map[error]int{tektontasks.ErrToolBudget: 429, tektontasks.ErrRequest: 403, tektontasks.ErrDeadline: 504, context.DeadlineExceeded: 504} {
		if got, _ := toolFailure(err); got != status {
			t.Fatal("status", err, got)
		}
	}
	for _, s := range []*fakeToolSession{
		{failRead: true},
	} {
		if (&toolDispatch{session: s, run: func(context.Context, tektontasks.ToolCall) (tektontasks.ToolResult, error) {
			t.Fatal("ran")
			return tektontasks.ToolResult{}, nil
		}}).poll(context.Background()) == nil {
			t.Fatal("sequence gap or read failure accepted")
		}
	}
	rewritten := &toolDispatch{session: &fakeToolSession{requests: []modelmailbox.Envelope{toolRequest(1, `{"tool":"a","target":"c","diff":""}`)}}, previous: first}
	if rewritten.poll(context.Background()) == nil {
		t.Fatal("rewritten sequence accepted")
	}
	if len(toolPayload(strings.Repeat("x", 64*1024))) > modelmailbox.MaxToolResponseBytes {
		t.Fatal("payload bound")
	}
}

func okRun(context.Context, tektontasks.ToolCall) (tektontasks.ToolResult, error) {
	return tektontasks.ToolResult{Passed: true, Summary: json.RawMessage(`{}`)}, nil
}

// The mailbox numbers calls that ended unread, so sequences may skip forward;
// an earlier one still stops the run. A call that is no longer pending when
// its answer is ready ended for the generator already and does not stop it.
func TestToolDispatchAcceptsForwardGapsAndUnreadCalls(t *testing.T) {
	session := &fakeToolSession{requests: []modelmailbox.Envelope{toolRequest(1, `{"tool":"a","target":"b","diff":""}`),
		toolRequest(3, `{"tool":"a","target":"c","diff":""}`)}}
	d := &toolDispatch{session: session, run: okRun}
	for range 2 {
		if err := d.poll(context.Background()); err != nil {
			t.Fatal("forward gap refused", err)
		}
	}
	if len(session.published) != 2 || session.published[1].Sequence != 3 {
		t.Fatal("answers", session.published)
	}
	back := &toolDispatch{session: &fakeToolSession{requests: []modelmailbox.Envelope{toolRequest(2, `{"tool":"a","target":"d","diff":""}`)}}, run: okRun, previous: toolRequest(3, `{}`)}
	if back.poll(context.Background()) == nil {
		t.Fatal("an earlier sequence was accepted")
	}
	expired := &toolDispatch{session: &fakeToolSession{requests: []modelmailbox.Envelope{toolRequest(1, `{"tool":"a","target":"b","diff":""}`)}, notPending: true}, run: okRun}
	if err := expired.poll(context.Background()); err != nil || expired.previous.Sequence != 1 {
		t.Fatal("a call that ended unanswered stopped the run", err)
	}
}

// The run's deadline comes from the request's expiry minus the answer margin.
func TestToolDispatchRunsWithinTheRequestWindow(t *testing.T) {
	request := toolRequest(1, `{"tool":"a","target":"b","diff":""}`)
	request.ExpiresAt = time.Now().Add(toolAnswerMargin + 30*time.Second).UnixMilli()
	var deadline time.Time
	d := &toolDispatch{session: &fakeToolSession{requests: []modelmailbox.Envelope{request}}, run: func(ctx context.Context, _ tektontasks.ToolCall) (tektontasks.ToolResult, error) {
		deadline, _ = ctx.Deadline()
		return okRun(ctx, tektontasks.ToolCall{})
	}}
	if err := d.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := time.UnixMilli(request.ExpiresAt).Add(-toolAnswerMargin); !deadline.Equal(want) {
		t.Fatal("deadline", deadline, want)
	}
}

type refused struct{ wait time.Duration }

func (refused) Error() string                 { return "refused" }
func (r refused) EvidenceWait() time.Duration { return r.wait }

// Authority is rechecked while a tool runs: a refusal cancels the run and
// stops the generation without answering; stale evidence is tolerated.
func TestToolDispatchStopsWhenAuthorityEndsDuringARun(t *testing.T) {
	every := toolAuthorizeEvery
	toolAuthorizeEvery = 20 * time.Millisecond
	t.Cleanup(func() { toolAuthorizeEvery = every })
	for name, tc := range map[string]struct {
		err  error
		stop bool
	}{"refused": {refused{}, true}, "stale": {refused{time.Minute}, false}} {
		session := &fakeToolSession{requests: []modelmailbox.Envelope{toolRequest(1, `{"tool":"a","target":"b","diff":""}`)}}
		d := &toolDispatch{session: session, authorize: func(context.Context) error { return tc.err }, run: func(ctx context.Context, _ tektontasks.ToolCall) (tektontasks.ToolResult, error) {
			select {
			case <-ctx.Done():
				return tektontasks.ToolResult{}, ctx.Err()
			case <-time.After(300 * time.Millisecond):
				return okRun(ctx, tektontasks.ToolCall{})
			}
		}}
		err := d.poll(context.Background())
		if tc.stop != (err != nil) || tc.stop != (len(session.published) == 0) {
			t.Fatal(name, err, session.published)
		}
	}
}

// The controller authorizes each call against the assignment's tools: the
// generator can reach the mailbox route without the tools server.
func TestControllerRefusesToolsTheAssignmentDoesNotOffer(t *testing.T) {
	spec := &generatortools.Spec{Tools: []generatortools.Tool{{Name: "build_test", Kind: "maven-build-test", Targets: []string{"rest-villains"}}}}
	if authorizeToolCall(spec, tektontasks.ToolCall{Tool: "build_test", Target: "rest-villains"}) != nil {
		t.Fatal("declared call refused")
	}
	for _, call := range []tektontasks.ToolCall{{Tool: "npm_lock_update", Target: "ui"}, {Tool: "build_test", Target: "rest-heroes"}, {Tool: "", Target: "rest-villains"}} {
		if !errors.Is(authorizeToolCall(spec, call), tektontasks.ErrRequest) {
			t.Fatal("undeclared call admitted", call)
		}
	}
	notOffered := &generatortools.Spec{Tools: []generatortools.Tool{{Name: "npm_lock_update", Kind: "npm-lock-update", Targets: []string{"ui"}}}}
	if !errors.Is(authorizeToolCall(notOffered, tektontasks.ToolCall{Tool: "npm_lock_update", Target: "ui"}), tektontasks.ErrRequest) ||
		!errors.Is(authorizeToolCall(nil, tektontasks.ToolCall{Tool: "build_test", Target: "rest-villains"}), tektontasks.ErrRequest) {
		t.Fatal("a tool that is not offered was admitted")
	}
	if status, _ := toolFailure(tektontasks.ErrRequest); status != 403 {
		t.Fatal("refusal status", status)
	}
}

// generatorTools offers tools only with a profile budget and assignment tools,
// and only while the policy's manifest still declares exactly those tools.
func TestGeneratorToolsRecheckTheBoundManifest(t *testing.T) {
	t.Cleanup(func() { project.SetInstalled(nil) })
	m := project.Manifest{Version: project.ManifestVersion, Repository: "fixture/product", BaseBranch: "main", Tekton: &project.Tekton{
		Modules: map[string]project.TektonModule{"rest-villains": {Services: []string{"none"}}},
		Tools:   []project.TektonTool{{Name: "build_test", Pipeline: project.TektonMavenBuildTest}}}}
	project.SetInstalled(&m)
	digest := m.Digest()
	profile := executor.Profile{Version: executor.ProfileVersion, Model: "fixture-model", MaxSteps: 8, MaxSeconds: 900, MaxTokens: 2048,
		ADevSHA256: strings.Repeat("a", 64), Tools: &executor.ToolProfile{Calls: 2, Seconds: 600}}
	prepared := func(p executor.Profile, tools *generatortools.Spec) Prepared {
		profileRaw, _ := json.Marshal(p)
		a := executor.Assignment{Version: executor.AssignmentVersion, WorkItemID: "11111111-1111-4111-8111-111111111111", Attempt: 1, Repository: "fixture/product",
			SourceSHA: strings.Repeat("b", 40), TreeSHA256: strings.Repeat("c", 64), ProfileSHA256: workspacebundle.Digest(profileRaw),
			AuthorityRevision: strings.Repeat("e", 64), IssueNumbers: []int{1}, Title: "Fixture", Body: "Synthetic.", AcceptanceCriteria: []string{"x"},
			ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Tools: tools}
		raw, _ := json.Marshal(a)
		return Prepared{assignment: raw, profile: profileRaw}
	}
	load := func(context.Context) (PreparationInputs, Placement, error) {
		return PreparationInputs{Policy: authority.Policy{Repository: "fixture/product", ProjectSHA256: digest}}, Placement{}, nil
	}
	cluster := func() (tektontasks.Cluster, error) { return nil, nil }
	journal := storeToolJournal{allocationID: "a"}
	spec := ToolSpecOf(m)
	if hook, err := generatorTools(context.Background(), prepared(profile, spec), load, &fakeToolSession{}, cluster, journal, nil); err != nil || hook == nil {
		t.Fatal("tools not offered", err)
	}
	plain := profile
	plain.Tools = nil
	if hook, err := generatorTools(context.Background(), prepared(plain, spec), load, &fakeToolSession{}, cluster, journal, nil); err != nil || hook != nil {
		t.Fatal("tools offered without a profile budget", err)
	}
	// A reload that changes the declared tools refuses instead of running
	// tools the assignment did not name.
	changed := m
	changed.Tekton = &project.Tekton{Modules: map[string]project.TektonModule{"rest-heroes": {Services: []string{"none"}}},
		Tools: []project.TektonTool{{Name: "build_test", Pipeline: project.TektonMavenBuildTest}}}
	project.SetInstalled(&changed)
	if _, err := generatorTools(context.Background(), prepared(profile, spec), load, &fakeToolSession{}, cluster, journal, nil); !errors.Is(err, ErrAuthority) {
		t.Fatal("changed manifest accepted", err)
	}
	digest = changed.Digest()
	if _, err := generatorTools(context.Background(), prepared(profile, spec), load, &fakeToolSession{}, cluster, journal, nil); !errors.Is(err, ErrAuthority) {
		t.Fatal("assignment tools differing from the bound manifest accepted", err)
	}
}

// Without a profile tool budget the assignment carries no tools, so its bytes
// and digest stay as before.
func TestAssignmentToolsOnlyWithAProfileBudget(t *testing.T) {
	spec := &generatortools.Spec{Tools: []generatortools.Tool{{Name: "build_test", Kind: "maven-build-test", Targets: []string{"m"}}}}
	profile := executor.Profile{Version: executor.ProfileVersion, Model: "fixture-model", MaxSteps: 8, MaxSeconds: 900, MaxTokens: 2048, ADevSHA256: strings.Repeat("a", 64)}
	raw, _ := json.Marshal(profile)
	if toolsForProfile(raw, workspacebundle.Digest(raw), spec) != nil {
		t.Fatal("tools without a budget")
	}
	profile.Tools = &executor.ToolProfile{Calls: 1, Seconds: 300}
	raw, _ = json.Marshal(profile)
	if toolsForProfile(raw, workspacebundle.Digest(raw), spec) != spec || toolsForProfile(raw, strings.Repeat("0", 64), spec) != nil {
		t.Fatal("tools with a budget, or with an unbound profile")
	}
}

type countingCluster struct{ created, configMaps int }

func (c *countingCluster) Create(context.Context, map[string]any) (string, string, error) {
	c.created++
	return "run-1", "u-1", nil
}
func (c *countingCluster) Get(context.Context, string) (map[string]any, error) {
	return nil, errors.New("no runs in this fixture")
}
func (c *countingCluster) Cancel(context.Context, string) error { return nil }
func (c *countingCluster) CreateConfigMap(context.Context, map[string]any) error {
	c.configMaps++
	return nil
}
func (c *countingCluster) OwnConfigMap(context.Context, string, string, string) error { return nil }
func (c *countingCluster) DeleteConfigMap(context.Context, string, string, string) error {
	return nil
}
func (c *countingCluster) ListCandidateConfigMaps(context.Context) ([]map[string]any, error) {
	return nil, nil
}

type countingJournal struct{ begun int }

func (j *countingJournal) Begin(context.Context, string) (int, error)         { j.begun++; return j.begun, nil }
func (j *countingJournal) Created(context.Context, int, string, string) error { return nil }
func (j *countingJournal) Finish(context.Context, int, tektontasks.ToolRecord) error {
	return nil
}

// Through the real generatorTools hook, a direct call the assignment does not
// offer (npm, declared in the manifest but not offered) or a declared tool on
// an undeclared target is answered 403 with no PipelineRun, no candidate and
// no journal entry: the controller authorizes calls, not only the tools server.
func TestGeneratorToolsHookRefusesCallsTheAssignmentDoesNotOffer(t *testing.T) {
	t.Cleanup(func() { project.SetInstalled(nil) })
	// The manifest declares npm (with a directory, so the catalog would accept
	// it) next to build_test; only build_test on rest-villains is offered.
	m := project.Manifest{Version: project.ManifestVersion, Repository: "fixture/product", BaseBranch: "main", Tekton: &project.Tekton{
		Modules:        map[string]project.TektonModule{"rest-villains": {Services: []string{"none"}}},
		NpmDirectories: []string{"ui"},
		Tools: []project.TektonTool{{Name: "build_test", Pipeline: project.TektonMavenBuildTest},
			{Name: "npm_lock_update", Pipeline: project.TektonNpmLockUpdate}}}}
	project.SetInstalled(&m)
	spec := ToolSpecOf(m)
	if len(spec.Tools) != 1 || spec.Tools[0].Name != "build_test" {
		t.Fatal("fixture spec", spec)
	}
	profile := executor.Profile{Version: executor.ProfileVersion, Model: "fixture-model", MaxSteps: 8, MaxSeconds: 900, MaxTokens: 2048,
		ADevSHA256: strings.Repeat("a", 64), Tools: &executor.ToolProfile{Calls: 4, Seconds: 600}}
	profileRaw, _ := json.Marshal(profile)
	a := executor.Assignment{Version: executor.AssignmentVersion, WorkItemID: "11111111-1111-4111-8111-111111111111", Attempt: 1, Repository: "fixture/product",
		SourceSHA: strings.Repeat("b", 40), TreeSHA256: strings.Repeat("c", 64), ProfileSHA256: workspacebundle.Digest(profileRaw),
		AuthorityRevision: strings.Repeat("e", 64), IssueNumbers: []int{1}, Title: "Fixture", Body: "Synthetic.", AcceptanceCriteria: []string{"x"},
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Tools: spec}
	raw, _ := json.Marshal(a)
	digest := m.Digest()
	load := func(context.Context) (PreparationInputs, Placement, error) {
		return PreparationInputs{Policy: authority.Policy{Repository: "fixture/product", ProjectSHA256: digest}}, Placement{}, nil
	}
	session := &fakeToolSession{requests: []modelmailbox.Envelope{
		toolRequest(1, `{"tool":"npm_lock_update","target":"ui","packages":["axios"],"diff":""}`),
		toolRequest(2, `{"tool":"npm_lock_update","target":"ui","packages":["axios"],"diff":"--- a/A\n+++ b/A\n"}`),
		toolRequest(3, `{"tool":"build_test","target":"rest-heroes","diff":""}`),
	}}
	cluster, journal := &countingCluster{}, &countingJournal{}
	hook, err := generatorTools(context.Background(), Prepared{assignment: raw, profile: profileRaw}, load, session,
		func() (tektontasks.Cluster, error) { return cluster, nil }, journal, nil)
	if err != nil || hook == nil {
		t.Fatal("hook", err)
	}
	for range 3 {
		if err := hook(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(session.published) != 3 || cluster.created != 0 || cluster.configMaps != 0 || journal.begun != 0 {
		t.Fatal("refused calls reached the cluster or the journal", len(session.published), cluster.created, cluster.configMaps, journal.begun)
	}
	for _, answer := range session.published {
		if answer.Status != 403 {
			t.Fatal("status", answer.Sequence, answer.Status, string(answer.Payload))
		}
	}
}
