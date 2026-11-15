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

func TestBrokerEmitsOnlyAllowlistedPublicationStage(t *testing.T) {
	for _, tc := range []struct{ stage, want string }{{"helper-transport", "publication-helper-transport"}, {"sensitive arbitrary exception", "publication-unknown"}} {
		t.Run(tc.want, func(t *testing.T) {
			d, a, profile, request := brokerFixture(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(completionBody)
			}, publisherFunc(func(context.Context, modelmailbox.Envelope) error { return fixturePublicationDiagnostic(tc.stage) }))
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
