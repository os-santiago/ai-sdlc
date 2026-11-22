package delivery

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/service"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/store"
)

// signedRouteCase exercises one /internal/<operation> path through the shared
// runtime boundary and the signed inbox handler. allowed reports whether the
// boundary must pass the route to the mux under env; disallowed routes must
// stay at maintenance observation even for a fully signed envelope.
type signedRouteCase struct {
	name      string
	operation string
	caller    string
	target    string
	env       map[string]string
	allowed   bool
}

func signedRouteCases() []signedRouteCase {
	verification := map[string]string{"AI_SDLC_RUNTIME_MODE": "verification", "AI_SDLC_INDEPENDENT_REVIEW": "false"}
	review := map[string]string{"AI_SDLC_RUNTIME_MODE": "verification", "AI_SDLC_INDEPENDENT_REVIEW": "true"}
	released := map[string]string{"AI_SDLC_RUNTIME_MODE": "verification", "AI_SDLC_INDEPENDENT_REVIEW": "true", "AI_SDLC_PUBLICATION": "true", "AI_SDLC_MERGE": "true", "AI_SDLC_BUILD": "true", "AI_SDLC_PROVENANCE": "true", "AI_SDLC_RELEASE": "true"}
	return []signedRouteCase{
		{"plan-admission", "plan-admission", "admission-controller", "orchestrator", verification, true},
		{"execute-work", "execute-work", "orchestrator", "worker", verification, true},
		{"execute-work-shadow", "execute-work", "orchestrator", "worker", map[string]string{"AI_SDLC_RUNTIME_MODE": "shadow"}, false},
		{"repair-candidate", "repair-candidate", "verifier", "orchestrator", review, true},
		{"stop-rejected-work", "stop-rejected-work", "verifier", "orchestrator", review, true},
		{"repair-candidate-without-review", "repair-candidate", "verifier", "orchestrator", verification, false},
		{"stop-rejected-work-without-review", "stop-rejected-work", "verifier", "orchestrator", verification, false},
		{"promote-artifact", "promote-artifact", "verifier", "release-manager", released, true},
		{"deploy-release", "deploy-release", "release-manager", "deployer", released, true},
		{"promote-artifact-without-release", "promote-artifact", "verifier", "release-manager", review, false},
		{"deploy-release-without-release", "deploy-release", "release-manager", "deployer", review, false},
		{"plan-admission-maintenance", "plan-admission", "admission-controller", "orchestrator", map[string]string{"AI_SDLC_RUNTIME_MODE": "maintenance"}, false},
	}
}

// boundaryAuthority mints disposable in-memory material with the highest
// autonomy ceiling so sealing every reviewed operation is possible. Component
// routes still gate each operation on its own installation option.
func boundaryAuthority(t *testing.T) (authority.Policy, map[string]authority.Identity) {
	t.Helper()
	material, err := authority.Bootstrap("example/project", "production-test")
	if err != nil {
		t.Fatal(err)
	}
	material.Policy.Autonomy, material.Policy.Freeze = "auto-deploy", false
	return material.Policy, material.Identities
}

// unreachableDB yields a non-nil pool whose first use fails. Signature
// verification runs before any storage access, so unsigned envelopes still
// get 401 while validly signed ones surface delivery_unavailable — proving
// the request passed the boundary and both authority layers.
func unreachableDB(t *testing.T) *store.DB {
	t.Helper()
	pool, err := sql.Open("pgx", "postgres://127.0.0.1:1/postgres")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	return &store.DB{Pool: pool}
}

func marshalEnvelope(t *testing.T, envelope Envelope) []byte {
	t.Helper()
	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func boundaryError(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body.Error
}

func TestSignedRoutesRejectUnsignedEnvelopesThroughRuntimeBoundary(t *testing.T) {
	policy, identities := boundaryAuthority(t)
	for _, route := range signedRouteCases() {
		t.Run(route.name, func(t *testing.T) {
			for key, value := range route.env {
				t.Setenv(key, value)
			}
			path := "/internal/" + route.operation
			var calls atomic.Int32
			effect := func(context.Context, *sql.Tx, authority.Receipt, []byte) ([]byte, error) {
				calls.Add(1)
				return []byte(`{"recorded":true}`), nil
			}
			inbox := Inbox{DB: unreachableDB(t), Target: route.target}
			mux := http.NewServeMux()
			mux.Handle("POST "+path, inbox.Handler(func() (authority.Policy, authority.Identity, error) {
				return policy, identities[route.target], nil
			}, route.operation, path, effect))
			handler := service.RuntimeBoundary(mux)

			payload := []byte(`{"id":"fixture"}`)
			receipt, err := identities[route.caller].Seal(policy, "event-"+route.name, route.operation, payload)
			if err != nil {
				t.Fatal(err)
			}
			transport, err := identities["relay"].Carry(policy, receipt, http.MethodPost, path, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			forged := receipt
			forged.Signature = ""
			for _, request := range []struct {
				name      string
				body      []byte
				wantCode  int
				wantError string
			}{
				{"malformed", []byte(`{"broken"`), http.StatusBadRequest, "invalid_envelope"},
				{"forged-receipt", marshalEnvelope(t, Envelope{Receipt: forged, Transport: transport, Payload: payload}), http.StatusUnauthorized, "authority_denied"},
				{"unsigned-transport", marshalEnvelope(t, Envelope{Receipt: receipt, Payload: payload}), http.StatusUnauthorized, "authority_denied"},
				{"signed", marshalEnvelope(t, Envelope{Receipt: receipt, Transport: transport, Payload: payload}), http.StatusServiceUnavailable, "delivery_unavailable"},
			} {
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(request.body)))
				wantCode, wantError := request.wantCode, request.wantError
				if !route.allowed {
					wantCode, wantError = http.StatusServiceUnavailable, "runtime_in_maintenance"
				}
				if recorder.Code != wantCode || boundaryError(t, recorder) != wantError {
					t.Fatalf("%s %s: status=%d error=%q", route.name, request.name, recorder.Code, boundaryError(t, recorder))
				}
			}
			if calls.Load() != 0 {
				t.Fatalf("%s mutated state", route.name)
			}
		})
	}
}

func TestSignedRoutesCommitOnceThroughRuntimeBoundary(t *testing.T) {
	f := setup(t)
	for _, route := range signedRouteCases() {
		t.Run(route.name, func(t *testing.T) {
			for key, value := range route.env {
				t.Setenv(key, value)
			}
			path := "/internal/" + route.operation
			var calls atomic.Int32
			effect := func(context.Context, *sql.Tx, authority.Receipt, []byte) ([]byte, error) {
				calls.Add(1)
				return []byte(`{"recorded":true}`), nil
			}
			inbox := Inbox{DB: f.dbs[route.target], Target: route.target}
			mux := http.NewServeMux()
			mux.Handle("POST "+path, inbox.Handler(func() (authority.Policy, authority.Identity, error) {
				return f.p, f.ids[route.target], nil
			}, route.operation, path, effect))
			handler := service.RuntimeBoundary(mux)

			payload := []byte(`{"id":"fixture"}`)
			receipt, err := f.ids[route.caller].Seal(f.p, "event-"+route.name, route.operation, payload)
			if err != nil {
				t.Fatal(err)
			}
			transport, err := f.ids["relay"].Carry(f.p, receipt, http.MethodPost, path, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			body := marshalEnvelope(t, Envelope{Receipt: receipt, Transport: transport, Payload: payload})
			post := func() *httptest.ResponseRecorder {
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body)))
				return recorder
			}
			stored := fmt.Sprintf(`SELECT count(*) FROM authorized_inbox WHERE target='%s' AND event_id='event-%s'`, route.target, route.name)
			if !route.allowed {
				if recorder := post(); recorder.Code != http.StatusServiceUnavailable || boundaryError(t, recorder) != "runtime_in_maintenance" {
					t.Fatalf("disabled capability admitted %s: %d", route.name, recorder.Code)
				}
				if calls.Load() != 0 || count(t, f.admin, stored) != 0 {
					t.Fatalf("disabled capability mutated state: %s", route.name)
				}
				return
			}
			if recorder := post(); recorder.Code != http.StatusOK {
				t.Fatalf("signed %s rejected: %d %s", route.name, recorder.Code, boundaryError(t, recorder))
			}
			if calls.Load() != 1 || count(t, f.admin, stored) != 1 {
				t.Fatalf("signed %s did not commit exactly once", route.name)
			}
			if recorder := post(); recorder.Code != http.StatusUnauthorized {
				t.Fatalf("replayed transport for %s: %d", route.name, recorder.Code)
			}
			if calls.Load() != 1 || count(t, f.admin, stored) != 1 {
				t.Fatalf("replay mutated %s", route.name)
			}
		})
	}
}
