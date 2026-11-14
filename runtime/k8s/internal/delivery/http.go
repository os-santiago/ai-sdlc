package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/authority"
)

// Handler exposes one fixed operation. Policy must be resolved from trusted
// configuration on every call; requests cannot select the target or operation.
func (in Inbox) Handler(policy func() (authority.Policy, authority.Identity, error), operation, path string, effect Effect) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fail := func(status int, code string) {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
		}
		if r.Method != http.MethodPost || r.URL.Path != path || r.URL.RawQuery != "" {
			fail(http.StatusNotFound, "unknown_operation")
			return
		}
		if policy == nil {
			fail(http.StatusServiceUnavailable, "policy_unavailable")
			return
		}
		p, identity, err := policy()
		if err != nil {
			fail(http.StatusServiceUnavailable, "policy_unavailable")
			return
		}
		if identity.Role != in.Target || identity.Validate(p) != nil {
			fail(http.StatusServiceUnavailable, "identity_unavailable")
			return
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2*1024*1024))
		if err != nil {
			fail(http.StatusBadRequest, "invalid_envelope")
			return
		}
		var envelope Envelope
		if err := authority.DecodeStrict(raw, &envelope); err != nil {
			fail(http.StatusBadRequest, "invalid_envelope")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		response, _, err := in.Accept(ctx, p, envelope, operation, r.Method, path, time.Now(), effect)
		switch {
		case errors.Is(err, ErrUnauthorized), errors.Is(err, ErrReplay):
			fail(http.StatusUnauthorized, "authority_denied")
		case errors.Is(err, ErrConflict):
			fail(http.StatusConflict, "event_conflict")
		case err != nil:
			fail(http.StatusServiceUnavailable, "delivery_unavailable")
		default:
			ack, err := identity.Confirm(p, envelope.Receipt, envelope.Transport, response)
			if err != nil {
				fail(http.StatusServiceUnavailable, "identity_unavailable")
				return
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(Acknowledged{Acknowledgement: ack, Result: response})
		}
	})
}

type Acknowledged struct {
	Acknowledgement authority.Acknowledgement `json:"acknowledgement"`
	Result          []byte                    `json:"result"`
}
