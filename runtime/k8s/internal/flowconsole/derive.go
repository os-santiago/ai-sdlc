// Package flowconsole is the read-only flow console: it derives
// ai-sdlc.flow-event/v1 events from the authoritative journal (or replays a
// recorded fixture), keeps a bounded cursor-addressed history, and serves it
// as Server-Sent Events, a JSON snapshot and an embedded static UI.
//
// It holds no write privilege, sends nothing to any component and grants no
// workflow authority. See docs/operations/flow-console.md.
package flowconsole

import (
	"sort"
	"strconv"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/flowevent"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

// DeriveOptions bounds what counts as a stale wait.
type DeriveOptions struct {
	// StaleWait marks a queued stage as stale once it waited this long
	// without a claim. Zero uses DefaultStaleWait.
	StaleWait time.Duration
}

// DefaultStaleWait is the default stale-wait threshold.
const DefaultStaleWait = 10 * time.Minute

// verdictReasons are refusals of the work itself; every other failure reason
// (expired, revoked, uncertain) is a safe stop without a verdict.
var verdictReasons = map[string]bool{"rejected": true, "comparison-failed": true, "request-changes": true}

type deriver struct {
	now    time.Time
	stale  time.Duration
	issues map[string]store.FlowWorkItem
	out    []flowevent.Event
}

// Derive maps one journal read to flow events. It is pure and deterministic:
// the same journal and options always yield the same events and ids.
func Derive(j store.FlowJournal, options DeriveOptions) []flowevent.Event {
	d := &deriver{now: j.ObservedAt.UTC(), stale: options.StaleWait, issues: map[string]store.FlowWorkItem{}}
	if d.stale <= 0 {
		d.stale = DefaultStaleWait
	}
	for _, w := range j.WorkItems {
		d.issues[w.UUID] = w
	}
	d.intake(j.Intake)
	d.admissions(j.Admissions, d.mergedIssues(j.Merges))
	d.workItems(j.WorkItems)
	d.executions(j.Executions, group(j.ExecutionEffects), groupCalls(j.ModelCalls))
	d.verdicts("verification", j.Verifications, group(j.VerificationEffects), nil)
	d.verdicts("review", j.Reviews, group(j.ReviewEffects), groupCalls(j.ReviewModelCalls))
	d.verdicts("publication", j.Publications, group(j.PublicationEffects), nil)
	d.verdicts("merge", j.Merges, group(j.MergeEffects), nil)
	flowevent.Sort(d.out)
	return d.out
}

func group(effects []store.FlowEffect) map[string][]store.FlowEffect {
	out := map[string][]store.FlowEffect{}
	for _, e := range effects {
		out[e.Parent] = append(out[e.Parent], e)
	}
	for _, list := range out {
		sort.Slice(list, func(i, j int) bool {
			if !list[i].CreatedAt.Equal(list[j].CreatedAt) {
				return list[i].CreatedAt.Before(list[j].CreatedAt)
			}
			return list[i].Kind < list[j].Kind
		})
	}
	return out
}

func groupCalls(calls []store.FlowModelCall) map[string][]store.FlowModelCall {
	out := map[string][]store.FlowModelCall{}
	for _, c := range calls {
		out[c.Parent] = append(out[c.Parent], c)
	}
	for _, list := range out {
		sort.Slice(list, func(i, j int) bool { return list[i].Sequence < list[j].Sequence })
	}
	return out
}

func ms(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	return d.Milliseconds()
}

// attrs keeps only contract-valid attributes, so an unexpected journal value
// (for example a new state name) degrades to an absent attribute, never to an
// invalid event.
func attrs(pairs ...string) map[string]string {
	out := map[string]string{}
	for i := 0; i+1 < len(pairs); i += 2 {
		if pairs[i+1] != "" && flowevent.IsAttribute(pairs[i]) && flowevent.IsValue(pairs[i+1]) && len(out) < flowevent.MaxAttributes {
			out[pairs[i]] = pairs[i+1]
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func name(s string) string {
	if flowevent.IsName(s) {
		return s
	}
	if s == "" {
		return ""
	}
	return "unknown"
}

type link struct {
	repository string
	issue      int
	workItem   string
	attempt    int
}

func (d *deriver) link(l store.FlowLink) link {
	out := link{repository: l.Repository, workItem: l.WorkItem, attempt: l.Attempt}
	if w, ok := d.issues[l.WorkItem]; ok {
		if out.repository == "" {
			out.repository = w.Repository
		}
		if len(w.Issues) > 0 {
			out.issue = w.Issues[0]
		}
	}
	return out
}

func (d *deriver) emit(l link, stage, step, kind, diagnostic string, at time.Time, duration, latency int64, attributes map[string]string, key ...string) {
	e := flowevent.Event{Version: flowevent.Version, ID: flowevent.NewID(append([]string{stage, kind, step}, key...)...), Time: at.UTC(),
		Component: flowevent.ComponentOf(stage), Repository: l.repository, Issue: l.issue, WorkItem: l.workItem, Attempt: l.attempt,
		Stage: stage, Step: name(step), Kind: kind, Diagnostic: name(diagnostic), DurationMs: duration, LatencyMs: latency, Attributes: attributes}
	if e.Validate() == nil {
		d.out = append(d.out, e)
	}
}

func (d *deriver) intake(rows []store.FlowIntake) {
	for _, r := range rows {
		l := link{repository: r.Repository, issue: r.Issue}
		generation := strconv.FormatInt(r.Generation, 10)
		d.emit(l, "intake", "observed", flowevent.KindCompleted, "", r.ObservedAt, 0, 0, attrs("generation", generation), r.Repository, strconv.Itoa(r.Issue), generation)
	}
}

// mergedIssues maps repository#issue to the time its work was merged.
func (d *deriver) mergedIssues(merges []store.FlowVerdict) map[string]time.Time {
	out := map[string]time.Time{}
	for _, m := range merges {
		if m.State != "merged" || m.FinishedAt == nil {
			continue
		}
		l := d.link(m.FlowLink)
		key := l.repository + "#" + strconv.Itoa(l.issue)
		if t, ok := out[key]; l.issue > 0 && (!ok || m.FinishedAt.Before(t)) {
			out[key] = *m.FinishedAt
		}
	}
	return out
}

// admissions maps admission decisions. Merging closes the issue, and intake
// then observes a new revision that admission rejects ("issue is closed").
// That rejection, at or after the issue's merge, is the end of delivered
// work, not a refusal: it becomes a completed step named issue-closed.
func (d *deriver) admissions(rows []store.FlowAdmission, merged map[string]time.Time) {
	for _, r := range rows {
		l := link{repository: r.Repository, issue: r.Issue}
		kind, diagnostic, step := flowevent.KindBlocked, r.Decision, "decision"
		switch r.Decision {
		case "accepted":
			kind, diagnostic = flowevent.KindCompleted, ""
		case "rejected":
			kind = flowevent.KindFailed
			if at, ok := merged[r.Repository+"#"+strconv.Itoa(r.Issue)]; ok && !r.UpdatedAt.Before(at) {
				kind, diagnostic, step = flowevent.KindCompleted, "", "issue-closed"
			}
		}
		d.emit(l, "admission", step, kind, diagnostic, r.UpdatedAt, ms(r.UpdatedAt.Sub(r.CreatedAt)), 0, attrs("decision", r.Decision, "risk", r.Risk),
			r.Repository, strconv.Itoa(r.Issue), r.CreatedAt.UTC().Format(time.RFC3339Nano), r.Decision)
	}
}

func (d *deriver) workItems(rows []store.FlowWorkItem) {
	for _, r := range rows {
		l := d.link(store.FlowLink{WorkItem: r.UUID, Repository: r.Repository})
		d.emit(l, "planning", "work-item", flowevent.KindCompleted, "", r.CreatedAt, 0, 0, attrs("status", r.Status), r.UUID)
	}
}

// wait emits the queued interval and, while still unclaimed past the
// threshold, one stale-wait marker at the moment it became stale.
func (d *deriver) wait(l link, stage string, created time.Time, claimed *time.Time, unclaimed bool, key string) {
	waited := ""
	if claimed != nil {
		waited = strconv.FormatInt(ms(claimed.Sub(created)), 10)
	}
	d.emit(l, stage, "queued", flowevent.KindWaiting, "", created, 0, 0, attrs("waitedMs", waited), key)
	if unclaimed && d.now.Sub(created) >= d.stale {
		d.emit(l, stage, "queued", flowevent.KindWaiting, "stale-wait", created.Add(d.stale), 0, 0,
			attrs("waitedMs", strconv.FormatInt(ms(d.now.Sub(created)), 10)), key, "stale")
	}
}

func (d *deriver) effects(l link, stage string, effects []store.FlowEffect, key string) {
	for _, e := range effects {
		if e.CompletedAt != nil {
			d.emit(l, stage, e.Kind, flowevent.KindProgressed, "", *e.CompletedAt, ms(e.CompletedAt.Sub(e.CreatedAt)), 0, nil, key, e.Kind, e.CreatedAt.UTC().Format(time.RFC3339Nano))
		}
	}
}

// calls emits each model call. Reviewer calls record their own start, so
// their latency is exact; generator calls record only their handoff, so
// their latency is the interval since the previous handoff (or allocation),
// which includes the executor's tool time between calls.
func (d *deriver) calls(l link, stage string, calls []store.FlowModelCall, fallback time.Time, key string) {
	previous := time.Time{}
	for _, c := range calls {
		start := c.WindowStart
		if c.CreatedAt != nil {
			start = *c.CreatedAt
		} else if !previous.IsZero() {
			start = previous
		}
		status, output := "", ""
		if c.Status != nil {
			status = strconv.Itoa(*c.Status)
		}
		if c.OutputBytes != nil {
			output = strconv.Itoa(*c.OutputBytes)
		}
		a := attrs("sequence", strconv.Itoa(c.Sequence), "inputBytes", strconv.Itoa(c.InputBytes), "outputBytes", output, "responseStatus", status)
		seq := strconv.Itoa(c.Sequence)
		if c.Finished != nil {
			d.emit(l, stage, "model-call", flowevent.KindProgressed, "", *c.Finished, 0, ms(c.Finished.Sub(start)), a, key, seq)
			previous = *c.Finished
		}
		at := fallback
		if c.Finished != nil {
			at = *c.Finished
		}
		switch {
		case c.Status != nil && *c.Status != 200:
			d.emit(l, stage, "model-call", flowevent.KindDegraded, "model-status-"+status, at, 0, 0, a, key, seq, "degraded")
		case c.State == "ambiguous" || c.State == "stopped" || c.State == "uncertain":
			d.emit(l, stage, "model-call", flowevent.KindDegraded, "model-call-"+c.State, at, 0, 0, a, key, seq, "degraded")
		}
	}
}

func lastEffect(effects []store.FlowEffect) time.Time {
	var last time.Time
	for _, e := range effects {
		for _, t := range []*time.Time{e.CompletedAt, e.NotSentAt} {
			if t != nil && t.After(last) {
				last = *t
			}
		}
		if e.CreatedAt.After(last) {
			last = e.CreatedAt
		}
	}
	return last
}

func (d *deriver) executions(rows []store.FlowExecution, effects map[string][]store.FlowEffect, calls map[string][]store.FlowModelCall) {
	for _, r := range rows {
		l := d.link(r.FlowLink)
		key := r.ID
		d.wait(l, "execution", r.CreatedAt, r.LifecycleAt, r.State == "queued", key)
		if r.Attempt > 1 {
			d.emit(l, "execution", "attempt", flowevent.KindRetried, "", r.CreatedAt, 0, 0, attrs("previousAttempt", strconv.Itoa(r.Attempt-1)), key)
		}
		start := r.CreatedAt
		if r.LifecycleAt != nil {
			start = *r.LifecycleAt
			d.emit(l, "execution", "claim", flowevent.KindStarted, "", start, 0, 0, nil, key)
		}
		d.effects(l, "execution", effects[r.ID], key)
		d.calls(l, "execution", calls[r.ID], r.UpdatedAt, key)
		exitCode, changes, modelCalls := "", "", ""
		if r.ExitCode != nil {
			exitCode = strconv.Itoa(*r.ExitCode)
		}
		if r.Changes != nil {
			changes = strconv.Itoa(*r.Changes)
		}
		if r.ModelCalls != nil {
			modelCalls = strconv.Itoa(*r.ModelCalls)
		}
		switch {
		case r.State == "stopped" && r.StopReason == "candidate-captured":
			at := r.UpdatedAt
			if r.CandidateAt != nil {
				at = *r.CandidateAt
			}
			d.emit(l, "execution", "candidate", flowevent.KindCompleted, "", at, ms(at.Sub(start)), 0,
				attrs("changes", changes, "modelCalls", modelCalls, "exitCode", exitCode), key)
		case r.State == "stopped":
			diagnostic := r.StopReason
			if r.ExitCode != nil {
				diagnostic = "worker-exit-" + exitCode
			}
			d.emit(l, "execution", "stop", flowevent.KindStoppedSafely, diagnostic, r.UpdatedAt, ms(r.UpdatedAt.Sub(start)), 0,
				attrs("reason", r.StopReason, "phase", r.Phase, "exitCode", exitCode, "modelCalls", strconv.Itoa(len(calls[r.ID]))), key)
		case r.State == "claimed" && r.ClaimUntil != nil && r.ClaimUntil.Before(d.now):
			d.emit(l, "execution", "lease", flowevent.KindBlocked, "lease-lapsed", *r.ClaimUntil, 0, 0, attrs("phase", r.Phase), key)
		}
		d.cleanup(l, "execution", r.Cleanup, r.CleanupAttempts, r.LifecycleUpdated, key)
	}
}

func (d *deriver) cleanup(l link, stage, state string, attempts int, at *time.Time, key string) {
	if at == nil {
		return
	}
	if state == "uncertain" {
		d.emit(l, stage, "cleanup", flowevent.KindQuarantined, "cleanup-uncertain", *at, 0, 0, attrs("cleanup", state, "cleanupAttempts", strconv.Itoa(attempts)), key)
	} else if attempts > 1 {
		d.emit(l, stage, "cleanup", flowevent.KindDegraded, "cleanup-retried", *at, 0, 0, attrs("cleanup", state, "cleanupAttempts", strconv.Itoa(attempts)), key)
	}
}

// verdicts covers the four claim-based stages that end in a state and reason.
func (d *deriver) verdicts(stage string, rows []store.FlowVerdict, effects map[string][]store.FlowEffect, calls map[string][]store.FlowModelCall) {
	success := map[string]string{"verification": "passed", "review": "passed", "publication": "published", "merge": "merged"}[stage]
	for _, r := range rows {
		l := d.link(r.FlowLink)
		key := r.ID
		list := effects[r.ID]
		var first *time.Time
		if len(list) > 0 {
			first = &list[0].CreatedAt
		}
		if c := calls[r.ID]; len(c) > 0 && c[0].CreatedAt != nil && (first == nil || c[0].CreatedAt.Before(*first)) {
			first = c[0].CreatedAt
		}
		d.wait(l, stage, r.CreatedAt, first, r.State == "queued", key)
		start := r.CreatedAt
		if first != nil {
			start = *first
			d.emit(l, stage, "claim", flowevent.KindStarted, "", start, 0, 0, nil, key)
		}
		d.effects(l, stage, list, key)
		end := lastEffect(list)
		for _, c := range calls[r.ID] {
			for _, t := range []*time.Time{c.CreatedAt, c.Finished} {
				if t != nil && t.After(end) {
					end = *t
				}
			}
		}
		if end.IsZero() || end.Before(r.CreatedAt) {
			end = r.CreatedAt
		}
		d.calls(l, stage, calls[r.ID], end, key)
		switch {
		case r.State == success:
			at := end
			if r.FinishedAt != nil {
				at = *r.FinishedAt
			} else if r.CommitTime != nil && r.CommitTime.After(at) {
				at = *r.CommitTime
			}
			d.emit(l, stage, "verdict", flowevent.KindCompleted, "", at, ms(at.Sub(start)), 0, attrs("effects", strconv.Itoa(len(list))), key)
		case r.State == "failed":
			kind := flowevent.KindStoppedSafely
			if verdictReasons[r.Reason] {
				kind = flowevent.KindFailed
			}
			d.emit(l, stage, "verdict", kind, r.Reason, end, ms(end.Sub(start)), 0, attrs("reason", r.Reason, "effects", strconv.Itoa(len(list))), key)
			// An effect that was neither acknowledged nor sealed as not sent
			// blocks the release manager: the same predicate as
			// PublicationBlocked and MergeBlocked.
			for _, e := range list {
				if e.CompletedAt == nil && e.NotSentAt == nil && (stage == "publication" || stage == "merge") {
					d.emit(l, stage, e.Kind, flowevent.KindQuarantined, "unresolved-effect", end, 0, 0, attrs("reason", r.Reason), key, e.Kind)
					break
				}
			}
		case r.State == "claimed" && r.ClaimUntil != nil && r.ClaimUntil.Before(d.now):
			d.emit(l, stage, "lease", flowevent.KindBlocked, "lease-lapsed", *r.ClaimUntil, 0, 0, nil, key)
		}
		if r.Cleanup == "uncertain" {
			d.emit(l, stage, "cleanup", flowevent.KindQuarantined, "cleanup-uncertain", end, 0, 0, attrs("cleanup", r.Cleanup), key)
		}
	}
}
