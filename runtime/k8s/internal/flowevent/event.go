// Package flowevent defines ai-sdlc.flow-event/v1, the read-only timeline
// contract of the flow console (docs/operations/flow-console.md).
//
// A flow event is a bounded, derived observation of the autonomous flow: when
// a stage of one work item started, progressed, finished, stopped, waited or
// degraded. It deliberately carries no source code, prompts, model output,
// issue text, tokens, digests of private material or credentials. Every
// string field is either a closed vocabulary or a short name matching a fixed
// pattern, so an event can be shown, logged and recorded as a fixture without
// review of its contents. The package has no dependencies beyond the standard
// library and performs no I/O except decoding a caller-supplied reader.
package flowevent

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"time"
)

// Version is the only accepted value of Event.Version.
const Version = "ai-sdlc.flow-event/v1"

// Limits of one event and of one decoded stream.
const (
	MaxAttributes  = 8
	MaxLineBytes   = 4096
	MaxFixtureSize = 50000
)

// Kinds, in the vocabulary an administrator reads on the timeline.
const (
	KindStarted       = "started"        // a stage began doing work (a claim or the first effect)
	KindProgressed    = "progressed"     // a bounded step inside a stage finished (an effect, a model call)
	KindCompleted     = "completed"      // the stage reached its successful terminal state
	KindFailed        = "failed"         // the stage reached a verdict against the work (for example a refused candidate)
	KindStoppedSafely = "stopped-safely" // the controller stopped the stage without a verdict (expired, revoked, uncertain)
	KindWaiting       = "waiting"        // queued and not yet claimed; diagnostic stale-wait once it waits too long
	KindRetried       = "retried"        // a later attempt of the same work item started
	KindQuarantined   = "quarantined"    // an unresolved effect or uncertain cleanup that blocks further work
	KindDegraded      = "degraded"       // the stage continued with a degraded signal (a failed model call, cleanup retries)
	KindBlocked       = "blocked"        // the stage needs a person or its lease lapsed without a terminal state
)

// Stages, in flow order.
var Stages = []string{"intake", "admission", "planning", "execution", "verification", "review", "publication", "merge"}

// Components that own each stage.
var stageComponent = map[string]string{
	"intake": "intake", "admission": "admission-controller", "planning": "orchestrator",
	"execution": "worker", "verification": "verifier", "review": "verifier",
	"publication": "release-manager", "merge": "release-manager",
}

var kinds = map[string]bool{KindStarted: true, KindProgressed: true, KindCompleted: true, KindFailed: true, KindStoppedSafely: true,
	KindWaiting: true, KindRetried: true, KindQuarantined: true, KindDegraded: true, KindBlocked: true}

// Attribute keys are a closed set; values are short names or integers.
var attributeKeys = map[string]bool{
	"phase": true, "exitCode": true, "reason": true, "decision": true, "risk": true, "status": true,
	"changes": true, "modelCalls": true, "sequence": true, "inputBytes": true, "outputBytes": true,
	"responseStatus": true, "cleanup": true, "cleanupAttempts": true, "generation": true,
	"previousAttempt": true, "waitedMs": true, "effects": true, "source": true, "effect": true,
}

var (
	namePattern       = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,79}$`)
	idPattern         = regexp.MustCompile(`^[a-f0-9]{24}$`)
	workItemPattern   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	repositoryPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,99}/[a-z0-9][a-z0-9._-]{0,99}$`)
	valuePattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)
)

// Event is one ai-sdlc.flow-event/v1 record. Optional numbers are omitted
// when unknown; zero is never used to mean "instant".
type Event struct {
	Version    string            `json:"version"`
	ID         string            `json:"id"`
	Time       time.Time         `json:"time"`
	Component  string            `json:"component"`
	Repository string            `json:"repository,omitempty"`
	Issue      int               `json:"issue,omitempty"`
	WorkItem   string            `json:"workItem,omitempty"`
	Attempt    int               `json:"attempt,omitempty"`
	Stage      string            `json:"stage"`
	Step       string            `json:"step,omitempty"`
	Kind       string            `json:"kind"`
	Diagnostic string            `json:"diagnostic,omitempty"`
	DurationMs int64             `json:"durationMs,omitempty"`
	LatencyMs  int64             `json:"latencyMs,omitempty"`
	Attributes map[string]string `json:"attributes,omitempty"`
}

// NewID derives a stable event id from the journal identity of the fact it
// describes (table, key, kind, ...), so re-deriving the same journal rows on
// every poll yields the same ids and a consumer can de-duplicate by id.
func NewID(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:24]
}

// ComponentOf returns the component that owns a stage, or "" when unknown.
func ComponentOf(stage string) string { return stageComponent[stage] }

// IsKind reports whether kind belongs to the closed vocabulary.
func IsKind(kind string) bool { return kinds[kind] }

// IsName reports whether s is a bounded diagnostic or step name.
func IsName(s string) bool { return namePattern.MatchString(s) }

// IsValue reports whether s is an acceptable attribute value.
func IsValue(s string) bool { return valuePattern.MatchString(s) }

// IsAttribute reports whether key belongs to the closed attribute set.
func IsAttribute(key string) bool { return attributeKeys[key] }

// Validate rejects any event outside the contract. Errors name the field,
// never echo its value, so a rejected fixture line cannot leak content.
func (e Event) Validate() error {
	switch {
	case e.Version != Version:
		return errors.New("flow event: unsupported version")
	case !idPattern.MatchString(e.ID):
		return errors.New("flow event: invalid id")
	case e.Time.IsZero() || e.Time.Year() < 2000 || e.Time.Year() > 9999:
		return errors.New("flow event: invalid time")
	case stageComponent[e.Stage] == "":
		return errors.New("flow event: unknown stage")
	case e.Component != stageComponent[e.Stage]:
		return errors.New("flow event: component does not own stage")
	case !kinds[e.Kind]:
		return errors.New("flow event: unknown kind")
	case e.Repository != "" && !repositoryPattern.MatchString(e.Repository):
		return errors.New("flow event: invalid repository")
	case e.Issue < 0 || e.Issue > 1<<30:
		return errors.New("flow event: invalid issue")
	case e.WorkItem != "" && !workItemPattern.MatchString(e.WorkItem):
		return errors.New("flow event: invalid work item")
	case e.Attempt < 0 || e.Attempt > 1000:
		return errors.New("flow event: invalid attempt")
	case e.Step != "" && !namePattern.MatchString(e.Step):
		return errors.New("flow event: invalid step")
	case e.Diagnostic != "" && !namePattern.MatchString(e.Diagnostic):
		return errors.New("flow event: invalid diagnostic")
	case e.DurationMs < 0 || e.DurationMs > 7*24*3600*1000:
		return errors.New("flow event: invalid duration")
	case e.LatencyMs < 0 || e.LatencyMs > 7*24*3600*1000:
		return errors.New("flow event: invalid latency")
	case len(e.Attributes) > MaxAttributes:
		return errors.New("flow event: too many attributes")
	case e.Issue == 0 && e.WorkItem == "":
		return errors.New("flow event: no issue or work item")
	}
	for key, value := range e.Attributes {
		if !attributeKeys[key] {
			return errors.New("flow event: unknown attribute")
		}
		if !valuePattern.MatchString(value) {
			return errors.New("flow event: invalid attribute value")
		}
	}
	return nil
}

// Lane returns the swimlane key: the issue when known, otherwise the work item.
func (e Event) Lane() string {
	if e.Issue > 0 {
		if e.Repository != "" {
			return fmt.Sprintf("%s#%d", e.Repository, e.Issue)
		}
		return fmt.Sprintf("#%d", e.Issue)
	}
	return "work-item/" + e.WorkItem
}

// Sort orders events by time, then stage order, then id: a total order, so
// replay is deterministic for equal timestamps.
func Sort(events []Event) {
	order := map[string]int{}
	for i, stage := range Stages {
		order[stage] = i
	}
	sort.SliceStable(events, func(i, j int) bool {
		a, b := events[i], events[j]
		if !a.Time.Equal(b.Time) {
			return a.Time.Before(b.Time)
		}
		if order[a.Stage] != order[b.Stage] {
			return order[a.Stage] < order[b.Stage]
		}
		return a.ID < b.ID
	})
}

// Decode reads newline-delimited events (a recorded fixture). Blank lines and
// lines starting with '#' are skipped. Unknown fields, oversized lines,
// duplicate ids and invalid events are rejected with the line number only.
func Decode(r io.Reader) ([]Event, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, MaxLineBytes), MaxLineBytes)
	var events []Event
	seen := map[string]bool{}
	line := 0
	for scanner.Scan() {
		line++
		raw := bytes.TrimSpace(scanner.Bytes())
		if len(raw) == 0 || raw[0] == '#' {
			continue
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		var e Event
		if err := decoder.Decode(&e); err != nil || decoder.More() {
			return nil, fmt.Errorf("flow event fixture line %d: malformed", line)
		}
		if err := e.Validate(); err != nil {
			return nil, fmt.Errorf("flow event fixture line %d: %w", line, err)
		}
		if seen[e.ID] {
			return nil, fmt.Errorf("flow event fixture line %d: duplicate id", line)
		}
		seen[e.ID] = true
		e.Time = e.Time.UTC()
		events = append(events, e)
		if len(events) > MaxFixtureSize {
			return nil, errors.New("flow event fixture: too many events")
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("flow event fixture line %d: unreadable or oversized", line+1)
	}
	Sort(events)
	return events, nil
}
