package flowconsole

import (
	"fmt"
	"math/rand"
	"strconv"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/flowevent"
)

// Synthetic generates a deterministic, contract-valid load fixture: items
// work items spread over the day before end, with the outcome mix of the
// evaluation batteries (most delivered, some stopped, refused, retried,
// blocked or quarantined, the newest still in flight). It exists to measure
// the UI at scale (fixture mode, -fixture-synthetic) and carries the
// attribute source=synthetic on every terminal event.
func Synthetic(items int, end time.Time) []flowevent.Event {
	rng := rand.New(rand.NewSource(int64(items)*7919 + 1))
	var out []flowevent.Event
	span := 24 * time.Hour
	for i := 0; i < items; i++ {
		issue := 1000 + i
		repository := fmt.Sprintf("example-org/service-%02d", i%7)
		if i%11 == 0 {
			repository = "example-org/a-much-longer-repository-name-for-truncation"
		}
		item := fmt.Sprintf("%08x-0000-4000-8000-%012x", issue, i)
		start := end.Add(-span + time.Duration(float64(span)*float64(i)/float64(items)))
		g := synth{out: &out, rng: rng, repository: repository, issue: issue, item: item, end: end}
		g.emit("intake", "observed", flowevent.KindCompleted, "", start.Add(-40*time.Second), 0, 0, 0, map[string]string{"generation": "1"})
		g.emit("admission", "decision", flowevent.KindCompleted, "", start.Add(-35*time.Second), 0, 800, 0, map[string]string{"decision": "accepted", "risk": "low"})
		g.emit("planning", "work-item", flowevent.KindCompleted, "", start.Add(-32*time.Second), 0, 0, 0, map[string]string{"status": "ready"})
		roll := rng.Float64()
		live := end.Sub(start) < 40*time.Minute
		t := start
		attempt := 1
		if roll < 0.12 && !live {
			// A first attempt stopped safely, then a retry.
			t = g.execution(t, attempt, 9+rng.Intn(20), "worker-exit-22", end)
			attempt++
			t = t.Add(time.Duration(30+rng.Intn(300)) * time.Second)
			g.emit("execution", "attempt", flowevent.KindRetried, "", t, attempt, 0, 0, map[string]string{"previousAttempt": "1"})
		}
		switch {
		case roll >= 0.12 && roll < 0.22 && !live:
			g.execution(t, attempt, 8+rng.Intn(24), []string{"uncertain", "worker-exit-25", "expired"}[rng.Intn(3)], end)
			continue
		case roll >= 0.22 && roll < 0.24:
			g.emit("admission", "decision", flowevent.KindBlocked, "needs-clarification", start.Add(-30*time.Second), 0, 0, 0, map[string]string{"decision": "needs-clarification"})
			continue
		}
		t = g.execution(t, attempt, 12+rng.Intn(20), "", end)
		if t.After(end) {
			continue
		}
		if roll >= 0.24 && roll < 0.30 {
			g.verdict("verification", t, attempt, flowevent.KindFailed, "comparison-failed", 0, end)
			continue
		}
		if t = g.verdict("verification", t, attempt, flowevent.KindCompleted, "", 0, end); t.After(end) {
			continue
		}
		if t = g.verdict("review", t, attempt, flowevent.KindCompleted, "", 2+rng.Intn(3), end); t.After(end) {
			continue
		}
		if roll >= 0.30 && roll < 0.32 {
			g.verdict("publication", t, attempt, flowevent.KindStoppedSafely, "uncertain", 0, end)
			g.emit("publication", "pull-request", flowevent.KindQuarantined, "unresolved-effect", t.Add(40*time.Second), attempt, 0, 0, map[string]string{"source": "synthetic"})
			continue
		}
		if t = g.verdict("publication", t, attempt, flowevent.KindCompleted, "", 0, end); t.After(end) {
			continue
		}
		g.verdict("merge", t, attempt, flowevent.KindCompleted, "", 0, end)
	}
	flowevent.Sort(out)
	return out
}

type synth struct {
	out        *[]flowevent.Event
	rng        *rand.Rand
	repository string
	issue      int
	item       string
	end        time.Time
	n          int
}

func (g *synth) emit(stage, step, kind, diagnostic string, at time.Time, attempt int, duration, latency int64, attributes map[string]string) {
	// Nothing after end: offsets after a check (a queue time, a degraded
	// call) could otherwise land in the future.
	if at.After(g.end) {
		return
	}
	g.n++
	e := flowevent.Event{Version: flowevent.Version, ID: flowevent.NewID("synthetic", g.item, strconv.Itoa(g.n)), Time: at.UTC().Truncate(time.Millisecond),
		Component: flowevent.ComponentOf(stage), Repository: g.repository, Issue: g.issue, Attempt: attempt, Stage: stage, Step: step, Kind: kind,
		Diagnostic: diagnostic, DurationMs: duration, LatencyMs: latency, Attributes: attributes}
	if stage != "intake" && stage != "admission" {
		e.WorkItem = g.item
	}
	if e.Validate() == nil {
		*g.out = append(*g.out, e)
	}
}

func (g *synth) seconds(lo, hi int) time.Duration {
	return time.Duration(lo+g.rng.Intn(hi-lo+1)) * time.Second
}

// execution emits a generation attempt and returns its end. A non-empty stop
// ends it safely; events after end are not emitted (still in flight).
func (g *synth) execution(t time.Time, attempt, calls int, stop string, end time.Time) time.Time {
	g.emit("execution", "queued", flowevent.KindWaiting, "", t, attempt, 0, 0, nil)
	claim := t.Add(g.seconds(2, 8))
	if claim.After(end) {
		return end.Add(time.Hour)
	}
	g.emit("execution", "claim", flowevent.KindStarted, "", claim, attempt, 0, 0, nil)
	at := claim
	for _, kind := range []string{"config", "pod", "source", "start"} {
		d := g.seconds(2, 9)
		at = at.Add(d)
		if at.After(end) {
			return end.Add(time.Hour)
		}
		g.emit("execution", kind, flowevent.KindProgressed, "", at, attempt, d.Milliseconds(), 0, nil)
	}
	for c := 1; c <= calls; c++ {
		d := g.seconds(10, 45)
		at = at.Add(d)
		if at.After(end) {
			return end.Add(time.Hour)
		}
		g.emit("execution", "model-call", flowevent.KindProgressed, "", at, attempt, 0, d.Milliseconds(), map[string]string{"sequence": strconv.Itoa(c), "responseStatus": "200"})
		if g.rng.Float64() < 0.01 {
			g.emit("execution", "model-call", flowevent.KindDegraded, "model-status-503", at.Add(time.Second), attempt, 0, 0, map[string]string{"responseStatus": "503"})
		}
	}
	done := at.Add(g.seconds(5, 12))
	if done.After(end) {
		return end.Add(time.Hour)
	}
	if stop != "" {
		g.emit("execution", "stop", flowevent.KindStoppedSafely, stop, done, attempt, done.Sub(claim).Milliseconds(), 0, map[string]string{"reason": "uncertain", "phase": "start-ready", "source": "synthetic"})
	} else {
		g.emit("execution", "candidate", flowevent.KindCompleted, "", done, attempt, done.Sub(claim).Milliseconds(), 0, map[string]string{"modelCalls": strconv.Itoa(calls), "source": "synthetic"})
	}
	return done
}

func (g *synth) verdict(stage string, t time.Time, attempt int, kind, diagnostic string, calls int, end time.Time) time.Time {
	queued := t.Add(g.seconds(1, 4))
	g.emit(stage, "queued", flowevent.KindWaiting, "", queued, attempt, 0, 0, nil)
	wait := g.seconds(1, 6)
	if stage == "merge" {
		wait = g.seconds(60, 240)
	}
	claim := queued.Add(wait)
	if claim.After(end) {
		return end.Add(time.Hour)
	}
	g.emit(stage, "claim", flowevent.KindStarted, "", claim, attempt, 0, 0, nil)
	at := claim.Add(g.seconds(5, 20))
	for c := 1; c <= calls; c++ {
		d := g.seconds(20, 70)
		at = at.Add(d)
		if at.After(end) {
			return end.Add(time.Hour)
		}
		g.emit(stage, "model-call", flowevent.KindProgressed, "", at, attempt, 0, d.Milliseconds(), map[string]string{"sequence": strconv.Itoa(c)})
	}
	done := at.Add(g.seconds(10, 60))
	if done.After(end) {
		return end.Add(time.Hour)
	}
	attributes := map[string]string{"source": "synthetic"}
	if diagnostic != "" {
		attributes["reason"] = diagnostic
	}
	g.emit(stage, "verdict", kind, diagnostic, done, attempt, done.Sub(claim).Milliseconds(), 0, attributes)
	return done
}
