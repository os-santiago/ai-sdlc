package flowevent

import (
	"os"
	"strings"
	"testing"
	"time"
)

func valid() Event {
	return Event{Version: Version, ID: NewID("x"), Time: time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC), Component: "worker",
		Repository: "os-santiago/ai-sdlc-sample", Issue: 196, WorkItem: "8f9a2c1e-3b4d-4e5f-8a6b-7c8d9e0f1a2b", Attempt: 2,
		Stage: "execution", Step: "stop", Kind: KindStoppedSafely, Diagnostic: "worker-exit-22", DurationMs: 1000,
		Attributes: map[string]string{"phase": "start-ready", "exitCode": "22"}}
}

func TestValidateAcceptsTheContractAndRejectsEverythingElse(t *testing.T) {
	if err := valid().Validate(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*Event){
		"version":         func(e *Event) { e.Version = "ai-sdlc.flow-event/v2" },
		"id":              func(e *Event) { e.ID = "not-an-id" },
		"time":            func(e *Event) { e.Time = time.Time{} },
		"stage":           func(e *Event) { e.Stage = "deploy" },
		"component":       func(e *Event) { e.Component = "verifier" },
		"kind":            func(e *Event) { e.Kind = "succeeded" },
		"repository":      func(e *Event) { e.Repository = "Not/Valid Repo" },
		"work item":       func(e *Event) { e.WorkItem = "x" },
		"no lane":         func(e *Event) { e.Issue, e.WorkItem = 0, "" },
		"diagnostic":      func(e *Event) { e.Diagnostic = "Worker exit: see prompt" },
		"step":            func(e *Event) { e.Step = strings.Repeat("a", 81) },
		"attribute key":   func(e *Event) { e.Attributes = map[string]string{"prompt": "x"} },
		"attribute value": func(e *Event) { e.Attributes = map[string]string{"reason": "contains spaces and text"} },
		"attribute count": func(e *Event) {
			e.Attributes = map[string]string{"phase": "a", "exitCode": "1", "reason": "r", "decision": "d", "risk": "r", "status": "s", "changes": "1", "modelCalls": "1", "sequence": "1"}
		},
		"negative latency": func(e *Event) { e.LatencyMs = -1 },
		"attempt":          func(e *Event) { e.Attempt = 1001 },
	}
	for name, mutate := range cases {
		e := valid()
		mutate(&e)
		if err := e.Validate(); err == nil {
			t.Errorf("%s accepted", name)
		} else if strings.Contains(err.Error(), "prompt") || strings.Contains(err.Error(), "spaces") {
			t.Errorf("%s error echoes the rejected value", name)
		}
	}
}

func TestDecodeRejectsUnknownFieldsDuplicatesAndOversizedLines(t *testing.T) {
	good := `{"version":"ai-sdlc.flow-event/v1","id":"` + NewID("a") + `","time":"2026-11-25T12:00:00Z","component":"intake","issue":1,"stage":"intake","kind":"completed"}`
	events, err := Decode(strings.NewReader("# comment\n\n" + good + "\n"))
	if err != nil || len(events) != 1 {
		t.Fatal(events, err)
	}
	for name, input := range map[string]string{
		"unknown field": strings.Replace(good, `"kind"`, `"output":"model text","kind"`, 1),
		"duplicate":     good + "\n" + good,
		"invalid":       strings.Replace(good, `"intake","issue"`, `"worker","issue"`, 1),
		"trailing":      good + good,
		"oversized":     `{"x":"` + strings.Repeat("a", MaxLineBytes) + `"}`,
	} {
		if _, err := Decode(strings.NewReader(input)); err == nil {
			t.Errorf("%s accepted", name)
		} else if strings.Contains(err.Error(), "model text") {
			t.Errorf("%s error echoes content", name)
		}
	}
}

func TestCommittedFixtureIsValidOrderedAndCoversEveryKind(t *testing.T) {
	file, err := os.Open("../../components/flow-console/fixtures/evaluation-batteries-29-30.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	events, err := Decode(file)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for i, e := range events {
		seen[e.Kind] = true
		if i > 0 && e.Time.Before(events[i-1].Time) {
			t.Fatal("fixture not ordered")
		}
	}
	for kind := range kinds {
		if !seen[kind] {
			t.Errorf("fixture lacks kind %s", kind)
		}
	}
}

func TestNewIDIsStableAndSeparatesParts(t *testing.T) {
	if NewID("a", "b") != NewID("a", "b") || NewID("ab") == NewID("a", "b") || len(NewID()) != 24 {
		t.Fatal("unstable or ambiguous ids")
	}
}
