package modelbroker

import (
	"context"
	"net/http"
	"testing"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/modelmailbox"
)

type fixturePublicationDiagnostic string

func (e fixturePublicationDiagnostic) Error() string            { return "sensitive arbitrary exception" }
func (e fixturePublicationDiagnostic) PublicationStage() string { return string(e) }

type fixtureExitedWorker struct{ code int32 }

func (e fixtureExitedWorker) Error() string                 { return "sensitive arbitrary exception" }
func (e fixtureExitedWorker) PublicationStage() string      { return "before-observation" }
func (e fixtureExitedWorker) WorkerExitCode() (int32, bool) { return e.code, true }

func TestBrokerEmitsOnlyAllowlistedPublicationStage(t *testing.T) {
	for _, tc := range []struct {
		stage, want string
		err         error
	}{
		{"helper-transport", "publication-helper-transport", nil},
		{"helper-exit-124", "publication-helper-exit-124", nil},
		{"helper-exit-0", "publication-unknown", nil},
		{"helper-exit-256", "publication-unknown", nil},
		{"sensitive arbitrary exception", "publication-unknown", nil},
		{"", "publication-worker-exit-124", fixtureExitedWorker{124}},
	} {
		t.Run(tc.want, func(t *testing.T) {
			failure := tc.err
			if failure == nil {
				failure = fixturePublicationDiagnostic(tc.stage)
			}
			d, a, profile, request := brokerFixture(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(completionBody)
			}, publisherFunc(func(context.Context, modelmailbox.Envelope) error { return failure }))
			result, err := d.Deliver(context.Background(), a.ID, profile, request)
			if err == nil || result.FailureStage != tc.want || result.HandoffConfirmed {
				t.Fatal("unsafe diagnostic or handoff")
			}
			_, state, handed, _ := ledgerState(t, d.DB, a)
			if state != "ambiguous" || handed {
				t.Fatal("diagnostic changed terminal outcome")
			}
		})
	}
	if SafeFailureStage("sensitive arbitrary exception") != "unknown" {
		t.Fatal("diagnostic escaped allowlist")
	}
}

func TestPublicationWorkerExitStageIsBounded(t *testing.T) {
	for _, good := range []string{"publication-worker-exit-0", "publication-worker-exit-124", "publication-worker-exit-255"} {
		if SafeFailureStage(good) != good {
			t.Fatal(good)
		}
	}
	for _, bad := range []string{"publication-worker-exit-256", "publication-worker-exit-01", "publication-worker-exit--1", "publication-worker-exit-"} {
		if SafeFailureStage(bad) != "unknown" {
			t.Fatal(bad)
		}
	}
}

func TestPublicationHelperExitStageIsBounded(t *testing.T) {
	for _, good := range []string{"publication-helper-exit-1", "publication-helper-exit-3", "publication-helper-exit-124", "publication-helper-exit-255"} {
		if SafeFailureStage(good) != good {
			t.Fatal(good)
		}
	}
	for _, bad := range []string{"publication-helper-exit-0", "publication-helper-exit-256", "publication-helper-exit-03", "publication-helper-exit-"} {
		if SafeFailureStage(bad) != "unknown" {
			t.Fatal(bad)
		}
	}
}
